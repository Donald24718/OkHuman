//go:build unix

package tools

// Unix 执行域（阶段 1 从 bash.go 逐字平移；阶段 2 外化为 defaultExecutor 方法，逻辑零改写）。
//
// 本文件承载 Unix 专属的进程控制：独立进程组（Setpgid）+ 超时杀整组（kill(-pgid)）
// + 从 wait 状态提取"是否死于信号"（syscall.WaitStatus）。
// 这些调用在 Windows 上不存在（无 Setpgid 字段、无 syscall.Kill），故用 build tag 隔离；
// Windows 侧由 bash_windows.go 提供同语义实现（阶段 3）。

import (
	"fmt"
	"io"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// defaultExecutor Unix 执行域实现（io.Writer 版）。
// 阶段 2 从 runBashProcess 外化而来：**判定逻辑逐字未变**，只换了取用户输出流的来源
// （接口传入的 stdout/stderr 直接接到 cmd.Stdout/cmd.Stderr）。
type defaultExecutor struct{}

// detachHint Unix 版（阶段 4）：指导模型用 setsid 三件套让长驻服务脱离进程组。
// 文本与阶段 1~3 的既有描述**逐字一致**（保持 Unix 侧 KV 前缀不变）。
func detachHint(toSec int) string {
	return fmt.Sprintf("起长驻服务（python 服务器等）必须完全脱离，否则服务占住本工具的进程组、会被 %d 秒超时连带杀掉：用 ", toSec) +
		"(setsid <启动命令> < /dev/null > /tmp/<名>.log 2>&1 &)，之后用 curl 探活、cat 读日志。脱离后服务独立存活，不受前台超时/转后台影响。" +
		"脱离必须三件套：setsid + 重定向 stdout/stderr + < /dev/null——只 setsid 不重定向，脱离子进程仍持有输出管道写端，Wait 收不到 EOF 会永久挂住，且超时杀组打不到它（上限彻底失效）。"
}

// detachHintBrief Unix 版：超时自述里的短出路提示（与既有文案逐字一致）。
func detachHintBrief() string {
	return "用 setsid + 重定向完全脱离进程组，或调大配置 tools.timeout_ms"
}

// tmpHint Unix 版：**无提示**（返回空串，保持 Unix 侧工具描述逐字不变）。
//
// Unix 上 /tmp 是真实目录（tmpfs 或磁盘目录），永久存在且可写，
// detachHint 里的 `/tmp/<名>.log` 也依赖它——无需任何规避提示。
func tmpHint() string { return "" }

// Run 实现 executor：启动 bash scriptPath，超时杀整个进程组。
//
// Unix 语义：独立进程组 + 超时 kill(-pgid) 杀整组——孤儿子进程（管道写端持有者）
// 一并 SIGKILL，管道必关，命令永不永久挂起。
func (defaultExecutor) Run(scriptPath string, timeoutSec int, stdout, stderr io.Writer) (ExecInfo, error) {
	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 独立进程组
	if err := cmd.Start(); err != nil {
		return ExecInfo{Code: -1}, err
	}
	// 超时标记用 atomic.Bool（2026-09-14 审计修：旧裸 bool 由 AfterFunc
	// goroutine 写、主 goroutine 读，无同步 → 数据竞争）。
	//
	// 竞态修（2026-10-04，两轮）：旧写法在计时器回调里**先** killed.Store(true) 后 Kill。
	// 若命令恰好在计时器触发后、SIGKILL 生效前自行 exit(0)，Wait 返回 nil（code=0），
	// 但 killed 已为 true → 输出"退出码: 0（超过 N 秒上限被强制终止）"——自相矛盾
	// （实测相位扫描命中 ~2-3%）。
	//
	// 第一层修（done 标志 + Kill 成功才置位 killed）经实测**不足以**根除：
	// SIGKILL 送达时若组首进程已退出但尚未被 Wait 回收（僵尸态），Kill 返回 nil
	// （杀僵尸是成功的空操作），killed 仍被置位，而 Wait 随后报告正常退出 0。
	// 即"Kill 成功"推不出"是我们杀死的"。
	//
	// 第二层修（根治）：权威信号来自 wait 状态——Go 的 ExitError.Sys() 给出
	// syscall.WaitStatus，Signaled() 为真表示进程**死于信号**（而非自己的退出码）。
	// 只有 killed（我们发过 kill）**且** Signaled（进程确实死于此信号）才算超时终止；
	// 进程自行 exit(0)/exit(N) 则不论 killed 与否都不加超时文案（见下方 timedOut 计算）。
	//
	// 注意：仍保留 done 快路径（Wait 已返回 → 回调直接不做），缩小无谓的 kill 尝试。
	var killed atomic.Bool
	var done atomic.Bool
	timer := time.AfterFunc(time.Duration(timeoutSec)*time.Second, func() {
		if done.Load() { // Wait 已返回 → 命令自行结束，不是超时
			return
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
			killed.Store(true)
		} else if err := cmd.Process.Kill(); err == nil {
			killed.Store(true) // 组已不存在时兜底杀顶层，成功才计为超时
		}
	})
	defer timer.Stop()

	waitErr := cmd.Wait()
	done.Store(true)

	// 从 wait 状态提取退出码与"是否死于信号"（Unix 专属：WaitStatus）。
	code := 0
	signaled := false
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				signaled = ws.Signaled()
			}
		} else {
			code = -1
		}
	}
	// 超时终止的权威判定：我们发过 kill **且** 进程死于信号。
	// 只 killed 不看 signaled 会把"命令自行 exit(0)"误报成超时（退出码 0 + 超时文案）；
	// 只 signaled 不看 killed 会把"命令自己 kill -9 自己"误报成超时。
	timedOut := isTimeoutKill(killed.Load(), signaled)

	// ExitReason 与双条件的交叉校验（方案 §3.1：不取代，只对账）。
	// 这里 Reason 由双条件推导，仅作记录；权威判定仍由 bash.go 用双条件做。
	reason := ExitNormal
	if signaled {
		reason = ExitSignaled
	}
	if timedOut {
		reason = ExitTimedOut
	}
	return ExecInfo{
		Reason:   reason,
		Code:     code,
		Killed:   killed.Load(),
		Signaled: signaled,
	}, nil
}
