//go:build windows

package tools

// Windows 执行域：Job Object 三级策略 + 超时杀整树（方案 §4.3）。
//
// 功能对齐目标（对 Unix 的 kill(-pgid)）：
//   Unix  : Setpgid + kill(-pgid)        → 杀整个进程组
//   Windows: Job Object + TerminateJobObject → 杀整个 Job（**内核级**，nohup/setsid/
//            disown/start //b/double-fork 都无法逃逸，见方案 §9.0.2）
//
// 三级策略（从优到劣，逐级降级并在失败时上报 Degraded）：
//   级 1 预绑定：CreateProcess + PROC_THREAD_ATTRIBUTE_JOB_LIST → **零竞态**（首选）
//   级 2 后绑定：Start 后 AssignProcessToJobObject → 有极小竞态窗口（子进程可能在
//                Assign 前 fork 并逃逸）
//   级 3 兜底：  仅 TerminateProcess 杀顶层进程 → 孙进程可能成孤儿（必须上报降级）
//
// 实证依据（2026-10-04，本机 Go 1.27.1 + x/sys v0.48.0）：
//   - 级 1 预绑定**本机可用**（无需 CREATE_BREAKAWAY_FROM_JOB；IsProcessInJob=1）；
//   - Job 继承是内核级：bash fork 的 6 个孙进程自动进 Job（ActiveProcesses=7）；
//   - TerminateJobObject 后 ActiveProcesses=0、3ms 返回（整树清空）。
//
// 未导出符号（须自定义，方案 §零实证）：
//   - PROC_THREAD_ATTRIBUTE_JOB_LIST = 0x0002000D（x/sys 未导出）
//   - IsProcessInJob（x/sys 未导出，走 LazyDLL）
//   - JOBOBJECT_BASIC_ACCOUNTING_INFORMATION（x/sys 未导出结构体）

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows API 常量（x/sys 未导出的部分）。
const (
	// procThreadAttributeJobList 把 Job 句柄写进 STARTUPINFOEX 的属性表，
	// 使子进程**创建时即在 Job 内**（零竞态）。x/sys v0.48.0 未导出，故自定义。
	procThreadAttributeJobList = 0x0002000D
	// procThreadAttributeHandleList 精确指定"只继承这些句柄"（避免继承无关句柄）。
	procThreadAttributeHandleList = 0x00020002
	// extendedStartupInfoPresent 使用 StartupInfoEx 时必须置位（syscall 未导出）。
	extendedStartupInfoPresent = 0x00080000
)

// terminatedByUsCode 是"由本工具超时机制杀死"的**专属退出码哨兵**（阶段 3 实证新增）。
//
// 为什么必需（实测，方案 §4.4 勘误）：
//   TerminateJobObject(job, N) 把进程退出码**逐字设为 N**。若传 N=1，退出码就是 1，
//   与普通非零退出**无法区分** → 双条件里的 signaled 恒为 false → 真超时被静默
//   报成"退出码: 1"（与"不静默降级"原则相悖）。故必须传一个**可识别且不可伪造**的值。
//
// 为什么 65535 不可被伪造（探针 P6 实测）：
//   - 自然退出：Windows/bash 把 `exit N` 截断到低 8 位（`exit 65535`→255、`exit 256`→0），
//     故自然退出码恒在 0..255，够不到 65535；
//   - 信号死亡：Git Bash 编码为 `signo<<8`（SIGKILL(9)→0x900、SIGTERM(15)→0xF00），
//     最大 SIGKILL 类不超过 0x4000，且**低字节恒为 0**；65535 低字节为 0xFF，不属于该族；
//   - 高位 NTSTATUS：均 >= 0x80000000，65535 远小于它。
//   三者交集为空 → **只有我们的 TerminateJobObject 能产出 65535**。
const terminatedByUsCode = 0x0000FFFF

// defaultExecutor Windows 执行域实现（实现 executor 接口）。
type defaultExecutor struct{}

// detachHint Windows 版（阶段 4 · 实测依据见下）。
//
// **关键实测（2026-10-04 探针）**：Windows 上**没有 setsid**（Git Bash 里
// `setsid: command not found`），且 Job Object 是**内核级、无法逃逸**的——
// `&`、`nohup`、双 fork、`disown`、`cmd /c start /b` 起的进程**全部**会被
// `TerminateJobObject` 一并清除（实测 5/5 全被清）。
//
// 这意味着：**Unix 的"脱离三件套"在 Windows 上没有等价物**。本工具在 Windows 上
// 无法让长驻服务"逃出"执行域——服务只要由本工具的 bash 启动，就属于这个 Job，
// 超时必被连坐。这是**平台能力边界**，不是实现缺陷（方案 §九 已声明）。
//
// 故 Windows 文案**不教**一个不存在的逃逸法，而是如实告知 + 给出可行的替代路径
// （用 Windows 原生机制在本工具之外启动服务）。
func detachHint(toSec int) string {
	return fmt.Sprintf("注意：本工具在 Windows 上把命令放进系统级 Job 容器，超时（%d 秒）会**连同其后代一起终止**，且无法脱离（Windows 无进程组脱离命令，Job 为内核级隔离，实测 nohup/后台符/双 fork/disown 全部无法逃逸）。", toSec) +
		"若需长期运行的服务（网站/监控等），请**不要用本工具启动**，而应让用户用 Windows 原生方式在工具之外启动" +
		"（如资源管理器双击、系统服务、或用户在终端手动 `Start-Process`），再用 curl 探活、cat 读日志。"
}

// detachHintBrief Windows 版：超时自述里的短出路提示。
func detachHintBrief() string {
	return "Windows 上无法脱离 Job 容器（无 setsid 且内核级隔离），长驻服务请在本工具之外用 Windows 原生方式启动；紧急需要可调大配置 tools.timeout_ms"
}

// tmpHint Windows 版：告诉模型**不要用 /tmp**（2026-10-05 实证，见下）。
//
// **实测事实（勿凭直觉删改）**：
//   - Git 的 `/etc/fstab` 有 `none /tmp usertemp ...`，`/tmp` 是 **MSYS 运行时挂载点**，
//     挂载目标**由 MSYS 运行时自行推导**，实测**完全不受** TMP / TEMP / TMPDIR /
//     MSYS_TMP 影响（四组控制变量实验，目标始终为 `.../AppData/Local/Temp`）。
//   - 该目标会随宿主会话**动态变化**：观察到过 `.../Temp` 与
//     `.../Temp/reasonix-session-tmp-<NNN>` 两种形态，后者是宿主会话临时目录，
//     **会被回收**。目标一旦被回收 → `/tmp` 写操作 ENOENT，
//     并伴随 bash 启动告警 `could not find /tmp, please create!`
//     （该字符串实测位于 **bash.exe / sh.exe**，不在 msys-2.0.dll）。
//
// 因此 OkHuman **无法从环境层修**（改不动 MSYS 挂载），只能从**行为层**规避：
// 明确告诉模型改用 `$TEMP` / 当前目录——这两处由 Windows 保证存在且稳定。
func tmpHint() string {
	return "临时文件请写到当前目录或 $TEMP，**不要写 /tmp**：本环境 /tmp 是 MSYS 挂载点，" +
		"其目标由宿主会话动态决定且可能被回收，写操作会失败（报 No such file or directory）。"
}

// Run 实现 executor：启动 bash scriptPath，超时杀整个 Job。
func (defaultExecutor) Run(scriptPath string, timeoutSec int, stdout, stderr io.Writer) (ExecInfo, error) {
	return (defaultExecutor{}).RunWithEnv(scriptPath, timeoutSec, stdout, stderr, nil)
}

// RunWithEnv 同 Run，但允许显式指定环境变量（nil = 用 childEnv() 默认值）。
//
// 存在的意义：让「环境构造」可被测试。Agent 通道的环境问题是真实故障源
// （2026-10-04：PATH 无 Git usr/bin → coreutils 全灭），必须能构造该环境做回归。
func (defaultExecutor) RunWithEnv(scriptPath string, timeoutSec int, stdout, stderr io.Writer, env []string) (ExecInfo, error) {
	bashPath, err := findBash()
	if err != nil {
		return ExecInfo{Code: -1}, err
	}
	if env == nil {
		env = childEnv()
	}

	// 构造"被 Child 继承"的输出写端：匿名管道写端。
	// 级 1 自建 CreateProcess 时无法用 os/exec 的自动接线，需手工管理。
	outW, outR, err := makeInheritablePipe()
	if err != nil {
		return ExecInfo{Code: -1}, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	errW, errR, err := makeInheritablePipe()
	if err != nil {
		windows.CloseHandle(outW)
		windows.CloseHandle(outR)
		return ExecInfo{Code: -1}, fmt.Errorf("创建 stderr 管道失败: %w", err)
	}

	// 起两个 goroutine 把管道内容拷贝到调用方的 stdout/stderr（封顶缓冲）。
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyHandleTo(outR, stdout) }()
	go func() { defer wg.Done(); copyHandleTo(errR, stderr) }()

	// 尝试三级策略。
	run, degrade, err := startWindowsProcess(bashPath, scriptPath, outW, errW, env)
	// 父进程必须立刻关掉自己的写端副本，否则读端等不到 EOF（子进程退出后仍挂着）。
	windows.CloseHandle(outW)
	windows.CloseHandle(errW)
	if err != nil {
		// 启动失败：**没有任何子进程**存在，写端只余父进程副本（已在上面关闭）
		// → 读端立即 EOF。此处顺序无关紧要，但仍保持"先等 goroutine 再关句柄"
		// 的统一纪律，避免日后复制粘贴到有后代的路径上埋雷。
		wg.Wait()
		windows.CloseHandle(outR)
		windows.CloseHandle(errR)
		return ExecInfo{Code: -1}, err
	}

	// 句柄纪律（方案 §11.2）：进程句柄与 Job 句柄必须关闭，否则每次调用泄漏 2 个。
	// Job 的 KILL_ON_JOB_CLOSE 在关闭时兜底杀灭残留（若超时杀后仍有存活后代）。
	defer run.cleanup()

	// 超时控制：与 Unix 同构的双 atomic（killed/done）+ AfterFunc。
	// 注意：Windows 无"信号"，用 killed 表示"我们发起过杀灭"。
	var killed atomicBool
	var done atomicBool
	timer := time.AfterFunc(time.Duration(timeoutSec)*time.Second, func() {
		if done.Load() {
			return
		}
		if run.killTree() == nil {
			killed.Store(true)
		}
	})
	defer timer.Stop()

	// 等待子进程结束（顶层进程句柄）。
	waitCode := run.wait()

	done.Store(true)
	timer.Stop()

	// **关闭顺序（2026-10-04 缺陷修复，实测依据见下）**：
	//
	// 必须**先 wg.Wait()（读到 EOF）再关读端**，不能反过来。
	//
	// 原实现是"先 CloseHandle(outR/errR) 再 wg.Wait()"，理由是"顶层进程已退出，
	// 写端应已全部释放"。但这个前提**只在没有存活后代时成立**。当脚本把命令放进
	// 后台（`cmd &`）时，**后台任务在顶层退出后仍持有写端**：
	//   - 顶层退出 → wait() 返回 → 我们提前关掉读端 → copyHandleTo 的 ReadFile
	//     立刻报错返回 → **后台任务后续输出全部丢失**（实测：100 行只收到 1 行，
	//     丢失 99；慢写 20 行只收到 1 行）。
	//
	// 修正：**一直读到 EOF 才关读端**——这正是 Unix 侧（os/exec + cmd.Wait）的语义。
	//
	// Unix 对照实测（同一批脚本，2026-10-04）：
	//   - 顶层 `exit 0` + 后台任务持写端 → **Unix 也会一直等到后台任务结束**
	//     （实测挂起 >12s；`wait()` 语义即"等所有管道写端释放"）。
	//   - 这是 Unix **已知且已文档化**的行为：detachHint 明确告知用户"只 setsid
	//     不重定向，脱离子进程仍持有输出管道写端，Wait 收不到 EOF 会永久挂住"。
	//     即**用户有责任**用 setsid+重定向让服务脱离。
	//
	// 因此 Windows 侧**对齐 Unix 的"等"**，不擅自杀掉用户有意留在后台的任务
	// （那会偏离跨平台契约：Unix 留后台任务存活，Windows 亦应如此）。
	//
	// ⚠️ 已知残留风险（与 Unix 同源，非本次引入，见方案 §边界）：
	//   "顶层退出 + 后台任务持写端"会让本步等待至后台任务结束。若后台任务是长驻
	//   服务（如 `nohup server &`），本工具会长时间不返回。这是 Unix 基线既有的
	//   行为，Windows 继承之；用户可用 `set MSYS...` 无关——Windows 无 setsid
	//   等价物（见 detachHint），故应避免用本工具启动长驻服务。
	wg.Wait()

	// 两个 goroutine 均已返回（读端 EOF 或出错），此时关闭读端句柄才安全。
	windows.CloseHandle(outR)
	windows.CloseHandle(errR)

	// 退出码与"是否死于异常"。
	code := int(waitCode)
	signaled := isSignaledExit(code)

	// 双条件判定（与 Unix 完全同源）：我们杀过 **且** 进程死于异常/信号。
	timedOut := isTimeoutKill(killed.Load(), signaled)

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
		Degraded: degrade,
		Killed:   killed.Load(),
		Signaled: signaled,
	}, nil
}

// runningProcess 抽象"一个已启动、可等待/可杀灭的进程（组）"。
type runningProcess struct {
	pid  uint32
	proc windows.Handle
	job  windows.Handle // 非零 = 在 Job 内（级 1/2）；0 = 级 3
}

// killTree 杀灭整个执行域：有 Job 用 TerminateJobObject（整树），否则兜底杀顶层。
//
// 用 terminatedByUsCode 作为终止码：使"我们的杀灭"在 ExitCode 上**可识别**
// （否则退出码退化为 1，与自然退出无法区分 → 双条件的 signaled 失效）。
func (r *runningProcess) killTree() error {
	if r.job != 0 {
		return windows.TerminateJobObject(r.job, terminatedByUsCode)
	}
	return windows.TerminateProcess(r.proc, terminatedByUsCode)
}

// wait 等待顶层进程结束并返回其退出码。
func (r *runningProcess) wait() uint32 {
	_, err := windows.WaitForSingleObject(r.proc, windows.INFINITE)
	if err != nil {
		return uint32(0xFFFFFFFF)
	}
	var code uint32
	windows.GetExitCodeProcess(r.proc, &code)
	return code
}

// cleanup 释放执行域资源（幂等，defer 调用）。
func (r *runningProcess) cleanup() {
	if r.job != 0 {
		windows.CloseHandle(r.job)
		r.job = 0
	}
	if r.proc != 0 {
		windows.CloseHandle(r.proc)
		r.proc = 0
	}
}

// atomicBool 极简 atomic.Bool 替代（避免与 Unix 版共享 import；语义完全一致）。
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) Store(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) Load() bool   { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

// makeInheritablePipe 创建匿名管道，返回（可继承写端, 不可继承读端）。
//
// 关键：读端**必须清除继承位**——否则子进程持着读端不放，父进程读不到 EOF
// （子进程退出后管道仍有写端/读端引用，copyHandleTo 永不返回）。
func makeInheritablePipe() (write, read windows.Handle, err error) {
	var sa windows.SecurityAttributes
	sa.Length = uint32(unsafe.Sizeof(sa))
	sa.InheritHandle = 1
	if err = windows.CreatePipe(&read, &write, &sa, 0); err != nil {
		return 0, 0, err
	}
	if err = windows.SetHandleInformation(read, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(read)
		windows.CloseHandle(write)
		return 0, 0, err
	}
	return write, read, nil
}

// copyHandleTo 从管道读端拷贝到 w，直到 EOF。用有限的缓冲避免大输出爆内存
// （调用方 w 是封顶缓冲，写入本身已封顶）。
func copyHandleTo(h windows.Handle, w io.Writer) {
	buf := make([]byte, 32*1024)
	for {
		var n uint32
		err := windows.ReadFile(h, buf, &n, nil)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// 写失败（调用方不再需要）：继续读空管道，避免子进程阻塞在写端。
				continue
			}
		}
		if err != nil || n == 0 {
			return
		}
	}
}

// isSignaledExit 判定退出码是否表示"非自然终止"（Windows 语义）。
//
// **阶段 3 实测修正（方案 §4.4 勘误）**：旧式 `code >= 0x80000000` 在 Git Bash 上
// **不成立**——bash 用 `signo<<8` 编码信号死亡（SIGKILL→0x900），远低于 0x80000000。
// 故改为**三源判定**，任一命中即视为"外部终止"：
//
//	① 高位 NTSTATUS（>= 0x80000000）：崩溃 / Ctrl-C（0xC000013A）等；
//	② Git Bash 信号码（signo<<8，即 code>=0x100 且低字节为 0）：kill -N 导致的死亡；
//	③ terminatedByUsCode（65535）：本工具 TerminateJobObject/TerminateProcess 所致。
//
// **为什么这三源不会互相污染双条件**（关键，见方案 §3.1 的两条陷阱）：
//   - 陷阱① 僵尸组假成功：命令在计时器触发的同一瞬间自行 exit(0)。此时房间里已无
//     活进程，TerminateJobObject 对空 Job"成功"→ killed=true，但**进程的退出码是它
//     自己的自然码（0..255），不会变成 65535**（已退出进程的退出码不可变，探针 P5
//     的 KILL_ON_JOB_CLOSE 用例返回 0 证实）→ 三源全不命中 → signaled=false →
//     isTimeoutKill(true,false)=false，**正确不报超时**。
//   - 陷阱② 命令自杀：`kill -9 $$` → 退出码 0x900 → 源②命中 → signaled=true，但
//     未被我们杀（killed=false）→ isTimeoutKill(false,true)=false，**正确不报超时**。
//   - 真超时：我们杀活进程 → 退出码被设为 65535 → 源③命中 → signaled=true，
//     且 killed=true → **正确报超时**。
//
// 注意 exit code 是 uint32；0xFFFFFFFF 是本实现的"等待失败"哨兵（视为异常）。
func isSignaledExit(code int) bool {
	u := uint32(code)
	if u == 0xFFFFFFFF { // 等待失败哨兵 → 视为异常（但不与超时混淆：超时还需 killed）
		return true
	}
	if u >= 0x80000000 { // ① 高位 NTSTATUS
		return true
	}
	if u == terminatedByUsCode { // ③ 我们的专属终止码
		return true
	}
	// ② Git Bash 信号码：signo<<8（低 8 位恒为 0），signo 合理范围 1..64。
	if u >= 0x100 && u&0xFF == 0 && u>>8 <= 64 {
		return true
	}
	return false
}

// startWindowsProcess 按三级策略启动进程，返回运行句柄、降级说明。
//
// 返回 degraded 非空 = 用了降级路径（模型可见，兑现"不静默降级"）。
func startWindowsProcess(bashPath, scriptPath string, outW, errW windows.Handle, env []string) (*runningProcess, string, error) {
	// 级 1：预绑定（零竞态）——首选。
	rp, err := startLevel1Prebind(bashPath, scriptPath, outW, errW, env)
	if err == nil {
		return rp, "", nil
	}
	level1Err := err

	// 级 2：Start 后 Assign（有极小竞态窗口）。
	rp2, err2 := startLevel2Assign(bashPath, scriptPath, outW, errW, env)
	if err2 == nil {
		return rp2, fmt.Sprintf("job-assign-after-start: 预绑定失败(%v)，改用启动后绑定（存在极小的逃逸竞态窗口）", level1Err), nil
	}

	// 级 3：无 Job，仅杀顶层进程（孙进程可能成孤儿）。
	rp3, err3 := startLevel3Plain(bashPath, scriptPath, outW, errW, env)
	if err3 == nil {
		return rp3, fmt.Sprintf("job-unavailable: 预绑定(%v)与后绑定(%v)均失败，已降级为仅杀顶层进程——"+
			"超时杀灭**不会**清空孙进程（可能留下孤儿），跨平台功能未完全对齐", level1Err, err2), nil
	}

	return nil, "", fmt.Errorf("三级策略全部失败：级1=%v；级2=%v；级3=%v", level1Err, err2, err3)
}

// startLevel1Prebind 级 1：CreateProcess + PROC_THREAD_ATTRIBUTE_JOB_LIST（零竞态）。
//
// StartupInfoEx 的三个必要件（缺一不可，方案 §4.3）：
//  1. si.Cb = sizeof(StartupInfoEx)
//  2. CreationFlags 含 EXTENDED_STARTUPINFO_PRESENT
//  3. 传 &si.StartupInfo（嵌入结构体地址，**不是** si 本身）
func startLevel1Prebind(bashPath, scriptPath string, outW, errW windows.Handle, env []string) (*runningProcess, error) {
	job, err := createKillOnCloseJob()
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	// 复用统一底层，extraJob 非 0 → 属性表写入 JOB_LIST（预绑定，零竞态）。
	rp, err := createWindowsProcess(bashPath, scriptPath, outW, errW, job, env)
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	rp.job = job
	return rp, nil
}

// startLevel2Assign 级 2：CreateProcess（不预绑定）+ AssignProcessToJobObject。
//
// 有极小竞态窗口：进程已创建但尚未 Assign 之间，若它立刻 fork，孙进程会逃逸。
// 故级 2 是**降级路径**，必须在 Degraded 里如实标注。
func startLevel2Assign(bashPath, scriptPath string, outW, errW windows.Handle, env []string) (*runningProcess, error) {
	job, err := createKillOnCloseJob()
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	rp, err := createWindowsProcess(bashPath, scriptPath, outW, errW, 0, env)
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	if err := windows.AssignProcessToJobObject(job, rp.proc); err != nil {
		rp.cleanup()
		windows.CloseHandle(job)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	rp.job = job
	return rp, nil
}

// startLevel3Plain 级 3：无 Job，仅记录子进程句柄（杀顶层，孙进程可能成孤儿）。
func startLevel3Plain(bashPath, scriptPath string, outW, errW windows.Handle, env []string) (*runningProcess, error) {
	return createWindowsProcess(bashPath, scriptPath, outW, errW, 0, env)
}

// createWindowsProcess 统一的自建 CreateProcess（不含 Job 逻辑，由调用方决定绑定方式）。
//
// 三处落地要点（缺一不可）：
//  1. si.Cb = sizeof(StartupInfoEx) + CreationFlags 含 EXTENDED_STARTUPINFO_PRESENT；
//  2. 用 PROC_THREAD_ATTRIBUTE_HANDLE_LIST 精确限定继承（只继承两个写端）；
//  3. 传 &si.StartupInfo（嵌入结构体地址）。
//
// extraJob 非 0 时额外写入 PROC_THREAD_ATTRIBUTE_JOB_LIST（级 1 预绑定用）。
func createWindowsProcess(bashPath, scriptPath string, outW, errW windows.Handle, extraJob windows.Handle, env []string) (*runningProcess, error) {
	attrCount := uint32(1) // HANDLE_LIST
	if extraJob != 0 {
		attrCount = 2 // + JOB_LIST
	}
	attrList, err := windows.NewProcThreadAttributeList(attrCount)
	if err != nil {
		return nil, fmt.Errorf("NewProcThreadAttributeList: %w", err)
	}
	defer attrList.Delete()

	handles := []windows.Handle{outW, errW}
	if err := attrList.Update(procThreadAttributeHandleList,
		unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return nil, fmt.Errorf("Update(HANDLE_LIST): %w", err)
	}
	if extraJob != 0 {
		jobHandle := extraJob
		if err := attrList.Update(procThreadAttributeJobList,
			unsafe.Pointer(&jobHandle), unsafe.Sizeof(jobHandle)); err != nil {
			return nil, fmt.Errorf("Update(JOB_LIST): %w", err)
		}
	}

	si := &windows.StartupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(*si)) // 落地要点 1
	si.ProcThreadAttributeList = attrList.List()
	si.StdOutput = outW
	si.StdErr = errW
	si.Flags |= windows.STARTF_USESTDHANDLES

	appName, err := windows.UTF16PtrFromString(bashPath)
	if err != nil {
		return nil, err
	}
	// 脚本路径用 Windows 原生形式（不做 cygpath，方案 §4.2 实证）。
	cmdLine, err := windows.UTF16PtrFromString(quoteWinArg(bashPath) + " " + quoteWinArg(scriptPath))
	if err != nil {
		return nil, err
	}
	envBlock := mustBuildEnvBlock(env)

	var pi windows.ProcessInformation
	err = windows.CreateProcess(
		appName, cmdLine,
		nil, nil,
		true, // bInheritHandles（HANDLE_LIST 已限定范围）
		extendedStartupInfoPresent|windows.CREATE_UNICODE_ENVIRONMENT,
		envBlock, nil,
		&si.StartupInfo, // 落地要点 3
		&pi,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}
	// 句柄纪律：Thread 句柄立刻关（不用）；Process 保留供 Wait/GetExitCode，
	// 由 runningProcess.cleanup 负责关闭。漏关 = 每次泄漏句柄（方案 §11.2）。
	windows.CloseHandle(pi.Thread)

	return &runningProcess{pid: pi.ProcessId, proc: pi.Process, job: 0}, nil
}

// createKillOnCloseJob 创建 Job 并设置"句柄关闭即杀灭"（防父进程崩溃留孤儿）。
func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	lim := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	lim.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&lim)), uint32(unsafe.Sizeof(lim)),
	); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// quoteWinArg 按 Windows 命令行规则给参数加引号（仅需处理含空格/引号的情形）。
func quoteWinArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			b.WriteString(`\"`)
			continue
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String()
}
