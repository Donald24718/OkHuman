package tools

// 工具系统（2026-09-07）：唯一元工具 bash，静态注入，TOOLS 数组永远不变
// → KV 前缀全程稳定。一切外部操作（文件读写改、搜索、抓被截断的 log 全文）
// 都靠 bash 完成。

import (
	"bytes"
	"fmt"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"okhuman/internal/types"
)

// TOOLS 静态工具表（与 TS 版逐字一致）
var TOOLS = []types.ToolSpec{
	{
		Name: "bash",
		Description: "执行 bash 命令（异步并行；输出超长被截断时，全文已写入 log 文件、截断处会给出路径，用 bash 的 cat / grep / tail 抓回）。" +
			"前台等待 30 秒未完成自动转后台（你收到通知后继续干活，结果在轮内下一个工具调用间隙或下一次 run 带回来）；命令总时长超 600 秒被强制终止（整个命令进程组）。" +
			"起长驻服务（python 服务器等）必须完全脱离，否则服务占住本工具的进程组、会被 600 秒超时连带杀掉：用 " +
			"(setsid <启动命令> < /dev/null > /tmp/<名>.log 2>&1 &)，之后用 curl 探活、cat 读日志。脱离后服务独立存活，不受前台超时/转后台影响。",
		Parameters: types.ToolParameters{
			Type: "object",
			Properties: map[string]interface{}{
				"command":         map[string]interface{}{"type": "string", "description": "要执行的 bash 命令"},
				"timeout_seconds": map[string]interface{}{"type": "number", "description": "超时秒数（默认 600，上限 600）；命令超过此时长被强制终止"},
			},
			Required: []string{"command"},
		},
	},
}

// ToolFailPrefix 工具执行失败结果的统一前缀（agent 与后台判定成功/失败用）
const ToolFailPrefix = "工具执行失败: "

// ExecuteTool 执行工具（唯一 bash；错误以返回 error 抛出，由 agent 并入字符串）
func ExecuteTool(name string, args map[string]interface{}) (string, error) {
	switch name {
	case "bash":
		return toolBash(args) // 参数错误 → error，由 agent 并入 ToolFailPrefix 字符串（与 TS throw→catch 等价）
	default:
		return "", fmt.Errorf("未知工具: %s（唯一元工具是 bash）", name)
	}
}

// toolBash bash 元工具：detached 独立进程组 + 超时杀整组。
// 串行链是死锁放大器——一条命令挂住，后面全部堵死（真实事故：agent 用
// nohup ... & 拉起服务，后台子壳握着输出管道写端不退出，等 EOF 永挂）。
// 配套防死锁：独立进程组 + 超时杀整组（-pid）——孤儿子进程（管道写端持有者）
// 一并 SIGKILL，管道必关，命令永不永久挂起。
func toolBash(args map[string]interface{}) (string, error) {
	command, ok := requireStr(args, "command")
	if !ok {
		return "", fmt.Errorf("参数 command 缺失或不是字符串")
	}
	timeoutSec := 600
	if v, ok := args["timeout_seconds"]; ok {
		if f, ok := v.(float64); ok && f > 0 {
			timeoutSec = int(f)
		}
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	if timeoutSec > 600 {
		timeoutSec = 600
	}

	cmd := exec.Command("bash", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 独立进程组
	// 审计 H3 修（2026-09-14）：输出按流封顶 32MB（防单条命令灌 GB 级输出
	// OOM 整个进程；600s 只限时间不限大小）。超上限部分丢弃但持续读取
	// （管道不堵，子进程不挂起），正文留前 32MB + 截断说明。
	stdout := newCappedBuffer(maxToolOutputBytes)
	stderr := newCappedBuffer(maxToolOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	// 超时标记用 atomic.Bool（2026-09-14 审计修：旧裸 bool 由 AfterFunc
	// goroutine 写、主 goroutine 读，无同步 → 数据竞争）
	var killed atomic.Bool
	timer := time.AfterFunc(time.Duration(timeoutSec)*time.Second, func() {
		killed.Store(true)
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			_ = cmd.Process.Kill() // 组已不存在时兜底杀顶层
		}
	})
	defer timer.Stop()

	waitErr := cmd.Wait()
	code := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	head := fmt.Sprintf("退出码: %d", code)
	if killed.Load() {
		head += "（已超时被终止）"
	}
	out := stdout.String()
	errOut := stderr.String()
	body := out
	if errOut != "" {
		if body != "" {
			body += "\n"
		}
		body += "[stderr]\n" + errOut
	}
	if body != "" {
		return head + "\n\n" + body, nil
	}
	return head, nil
}

// maxToolOutputBytes 单流（stdout/stderr 各自）输出封顶（2026-09-14 审计 H3 修）
const maxToolOutputBytes = 32 << 20

// cappedBuffer 保留前 n 字节；超出部分丢弃但持续读取（管道保持排空、子进程
// 永不阻塞在写管道上），并记录被丢弃字节数供截断说明用。
type cappedBuffer struct {
	buf  bytes.Buffer
	cap  int
	over int64
}

func newCappedBuffer(n int) *cappedBuffer { return &cappedBuffer{cap: n} }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.cap - c.buf.Len()
	if room >= len(p) {
		return c.buf.Write(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
		p = p[room:]
	}
	c.over += int64(len(p))
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.over == 0 {
		return c.buf.String()
	}
	// 说明放开头：agent 的 ResultLimit 截断保留头部（模型据此知道丢弃量）；
	// 尾部会被切掉。
	return fmt.Sprintf("[输出截断：仅保留前 %d 字节，另有 %d 字节丢弃（共约 %d 字节）]\n",
		c.cap, c.over, int64(c.cap)+c.over) + c.buf.String()
}

func requireStr(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key]
	s, ok2 := v.(string)
	if !ok || !ok2 || s == "" {
		return "", false
	}
	return s, true
}
