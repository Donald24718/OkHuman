package tools

// 工具系统（2026-09-07）：唯一元工具 bash。一切外部操作（文件读写改、搜索、
// 抓被截断的 log 全文）都靠 bash 完成。
//
// 工具表由配置现生成（2026-10-02：命令总时长上限从硬编码 600s 改为
// tools.timeout_ms 可配）：同配置 → 逐字同文本 → KV 前缀稳定。改 tools 段
// 会让描述变化，代价是前缀作废一次——这是必要的：描述里的超时数字必须与
// 实际执行一致，否则模型基于假数字做决策（旧实现正是配置可改、描述写死
// 30s/600s，两处不一致）。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"okhuman/internal/types"
)

// 超时默认与可配上限（2026-10-02）
const (
	DefaultToolTimeoutMS = 600000   // 命令总时长上限默认 600s（与旧硬编码一致）
	MaxToolTimeoutMS     = 86400000 // 可配上限 24h：再大等于没有兜底
	DefaultFgTimeoutMS   = 30000
)

var (
	toolTimeoutMS atomic.Int64            // 命令总时长上限（毫秒）
	toolFgMS      atomic.Int64            // 前台等待（毫秒）——仅用于生成描述文本
	toolsSpec     atomic.Pointer[[]types.ToolSpec]
)

// Configure 设置工具超时并重建工具表：启动时一次，tools 段热更新时再调。
// 新上限对"下一条命令"生效；已在跑的命令沿用其启动时的上限。
// atomic.Pointer 与 context.ConfigureInjectResolver 同构（与在飞 LLM 调用的读并发）。
func Configure(fgMS, timeoutMS int) {
	if timeoutMS <= 0 || timeoutMS > MaxToolTimeoutMS {
		timeoutMS = DefaultToolTimeoutMS
	}
	if fgMS <= 0 {
		fgMS = DefaultFgTimeoutMS
	}
	toolTimeoutMS.Store(int64(timeoutMS))
	toolFgMS.Store(int64(fgMS))
	s := buildTools(fgMS, timeoutMS)
	toolsSpec.Store(&s)
}

// Specs 当前工具表（注入 LLM 请求用；与 ExecuteTool 的超时同源）
func Specs() []types.ToolSpec {
	if p := toolsSpec.Load(); p != nil {
		return *p
	}
	Configure(DefaultFgTimeoutMS, DefaultToolTimeoutMS) // 未接线兜底（测试/误用）
	return *toolsSpec.Load()
}

// buildTools 由超时参数生成工具表：同参数 → 逐字同文本 → KV 前缀稳定
func buildTools(fgMS, timeoutMS int) []types.ToolSpec {
	fgSec, toSec := fgMS/1000, timeoutMS/1000
	return []types.ToolSpec{
		{
			Name: "bash",
			Description: "执行 bash 命令（异步并行；输出超长被截断时，全文已写入 log 文件、截断处会给出路径，用 bash 的 cat / grep / tail 抓回）。" +
				fmt.Sprintf("前台等待 %d 秒未完成自动转后台（你收到通知后继续干活，结果在轮内下一个工具调用间隙或下一次 run 带回来）；命令总时长超 %d 秒被强制终止（整个命令进程组）。", fgSec, toSec) +
				fmt.Sprintf("起长驻服务（python 服务器等）必须完全脱离，否则服务占住本工具的进程组、会被 %d 秒超时连带杀掉：用 ", toSec) +
				"(setsid <启动命令> < /dev/null > /tmp/<名>.log 2>&1 &)，之后用 curl 探活、cat 读日志。脱离后服务独立存活，不受前台超时/转后台影响。" +
				"脱离必须三件套：setsid + 重定向 stdout/stderr + < /dev/null——只 setsid 不重定向，脱离子进程仍持有输出管道写端，Wait 收不到 EOF 会永久挂住，且超时杀组打不到它（上限彻底失效）。",
			Parameters: types.ToolParameters{
				Type: "object",
				Properties: map[string]interface{}{
					"command": map[string]interface{}{"type": "string", "description": "要执行的 bash 命令"},
					"timeout_seconds": map[string]interface{}{"type": "number",
						"description": fmt.Sprintf("超时秒数（默认 %d，上限 %d）；命令超过此时长被强制终止", toSec, toSec)},
				},
				Required: []string{"command"},
			},
		},
	}
}

// numArg 数值参数取数：JSON 解码恒为 float64，但 Go 侧调用方可能传 int/int64——
// 只认 float64 会把这类值静默忽略（退回默认上限），行为与调用方意图相反。
func numArg(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
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
	// 上限来自配置 tools.timeout_ms（2026-10-02；旧为硬编码 600）
	limitSec := int(toolTimeoutMS.Load()) / 1000
	if limitSec < 1 {
		limitSec = DefaultToolTimeoutMS / 1000
	}
	timeoutSec := limitSec
	if v, ok := args["timeout_seconds"]; ok {
		if f, ok := numArg(v); ok && f > 0 {
			timeoutSec = int(f)
		}
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	if timeoutSec > limitSec { // 模型可下调，不可越过配置上限
		timeoutSec = limitSec
	}

	// 方案 B（2026-09-15）：命令文本进 /tmp 临时脚本，不进 argv。pkill/pgrep -f 按完整
	// 命令行（argv）匹配——旧 `bash -c <全文>` 把命令文本挂在包装壳 argv 上，任何命中
	// 命令文本的 -f 模式必然匹配到包装壳自己（自杀 exit 143 / 自匹配假 pid，2026-09-15
	// 实测事故）。执行脚本文件是 bash 原生形态：命令文本既不在 argv 也不在 environ。
	// 文件在进程退出后删除（绑退出，不绑 30s 前台超时——届时命令可能已转后台仍在跑；
	// 提前 unlink 也不会中断执行，绑退出是保留取证价值）。
	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("okhuman-bash-%d-%s.sh", time.Now().UnixMilli(), randHex(4)))
	if err := os.WriteFile(scriptPath, []byte(command), 0o700); err != nil {
		return "", fmt.Errorf("写临时脚本失败: %w", err)
	}
	defer os.Remove(scriptPath)
	cmd := exec.Command("bash", scriptPath)
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
		// 光说"已超时"不够：模型分不清"命令自己失败"与"被上限杀掉"，会误判重试。
		// 给出上限值与出路（脱离进程组 / 调大配置）。
		head += fmt.Sprintf("（超过 %d 秒上限被强制终止；要跑更久：用 setsid + 重定向完全脱离进程组，或调大配置 tools.timeout_ms）", timeoutSec)
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

// randHex n 字节的十六进制随机串（临时脚本文件名去重用）
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()) // crypto/rand 失败兜底（理论上不会）
	}
	return hex.EncodeToString(b)
}
