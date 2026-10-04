package tools

// bash 元工具实现。第二个元工具（如 ipython）应新建同目录 ipython.go，
// 不要塞进本文件；本文件只放 bash 特有逻辑，公共设施见 output.go / args.go。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

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
	// 文件在 toolBash 返回时删除（= 前台命令结束），绑的是本函数的 defer，不是真正的
	// 进程退出：命令转后台、或 setsid 完全脱离后本函数已返回，脚本此时已被删除，而
	// 命令本身仍在运行。（提前 unlink 不中断执行，故此举无副作用。）
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
