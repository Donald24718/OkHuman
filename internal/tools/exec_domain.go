package tools

// 执行域契约（平台无关层）。
//
// 本文件承载不依赖任何平台 API 的契约与纯函数，供 bash.go（平台无关主体）
// 与各平台执行实现（bash_unix.go / bash_windows.go）共享。
//
// 阶段 1：平移 isTimeoutKill（逐字）。
// 阶段 2：追加 ExitReason / ExecInfo / executor 接口 + platformExecutor() 平台选择点。
// 详见 docs/跨平台-bash-工具完整方案-v2-实证修订.md §三 / §八。

import (
	"io"
	"sync/atomic"
)

// isTimeoutKill 判定一次执行是否属于"被本工具的超时机制强制终止"。
//
// 抽成纯函数是为了可确定性单测：真实竞态是概率性的（~2-3%），
// 靠集成测试反复跑无法可靠区分真修与假修（变异测试会漏网）。
//
// 参数语义：
//   - killed：计时器回调是否成功向进程组/顶层进程发出过 SIGKILL；
//   - signaled：被 Wait 回收的进程是否死于信号（syscall.WaitStatus.Signaled()）。
//
// 两个条件缺一不可：
//   - 只看 killed：命令在计时器触发的同一瞬间自行 exit(0) 时，Kill 会打在
//     尚未回收的僵尸组上并"成功"返回，killed=true 而进程其实正常退出 →
//     输出"退出码: 0（超过 N 秒上限被强制终止）"的自相矛盾文案。
//   - 只看 signaled：命令自己 `kill -9 $$` 也会 Signaled=true，但那不是超时。
func isTimeoutKill(killed, signaled bool) bool {
	return killed && signaled
}

// ExitReason 进程结束的成因分类。
//
// **它不是 isTimeoutKill 的替代品**（方案 §3.1 订正为"不取代"）：
//   - isTimeoutKill(killed, signaled) 仍是判定"是否加超时文案"的权威口径；
//   - ExitReason 是平台实现显式记录的成因，用作**对双条件的交叉校验**
//     （两者不一致时暴露实现 bug），而非取代它。
//
// 为什么不能只用 ExitReason：双条件防的两条错误路径是**跨平台共有**的——
//  1. 僵尸组 Kill 假成功（Unix: Kill 打在已退出未回收的组上返回 nil；
//     Windows: TerminateJobObject 对已空 Job 同样返回成功）；
//  2. 命令自杀（Unix: kill -9 $$；Windows: ExitProcess(0xC000013A) 产生同码）。
type ExitReason int

const (
	ExitNormal   ExitReason = iota // 进程自然退出（含非零退出码）
	ExitSignaled                   // 死于信号（Unix）/ 异常退出码（Windows）
	ExitTimedOut                   // 超时控制器触发杀灭
)

// String 便于日志与测试可读（不参与任何判定逻辑）。
func (r ExitReason) String() string {
	switch r {
	case ExitNormal:
		return "normal"
	case ExitSignaled:
		return "signaled"
	case ExitTimedOut:
		return "timedout"
	default:
		return "unknown"
	}
}

// ExecInfo 一次执行的完整结果（平台实现产出，bash.go 消费）。
type ExecInfo struct {
	Reason   ExitReason // 成因分类（**交叉校验用**，不替代 isTimeoutKill）
	Code     int        // 进程退出码；被杀时语义见 Reason
	Degraded string     // 非空 = 平台降级说明（如 "job-unavailable: 父 Job 不允许 breakaway"）

	// Killed/Signaled 是平台侧回答「是否由本工具超时机制杀灭」的两个原始条件。
	// bash.go 用 isTimeoutKill(Killed, Signaled) 做**权威判定**（方案 §3.1），
	// Reason 只作对账：两者不一致说明平台实现有 bug（见 bash.go 的交叉校验）。
	Killed   bool
	Signaled bool
}

// executor 平台执行域抽象：bash.go 只依赖本接口，不依赖具体平台。
//
// **签名与方案 §3.2 的差异及理由**（订正记录）：
//   - 方案写 `Run(ctx, scriptPath, stdout, stderr)`，超时由 ctx 控制；
//     但阶段 2 的硬约束是「Unix 逻辑逐字不变」，而 Unix 的定时器实现
//     （time.AfterFunc + killed/done 双 atomic + 相位竞态修复）改由 ctx 驱动
//     会**重写**该逻辑 → 违背约束。
//   - 故阶段 2 保留 `timeoutSec int`（定时器留在平台实现内部），
//     仅把「平台函数」外化为「接口方法」。ctx 化留待后续单独评估（见 §八 阶段 3 备注）。
//
// 返回：
//   - info：执行结果（含 Degraded 降级说明）；
//   - startErr：**启动**失败的错误（对应原 `cmd.Start()` 的 error；正常为 nil）。
//     bash.go 在 startErr != nil 时直接返回 error（原语义）。
type executor interface {
	// Run 执行 scriptPath：创建执行域、绑定子进程、等待完成、超时杀灭整组。
	// stdout/stderr 由调用方提供（封顶缓冲），平台实现把它们接到子进程。
	Run(scriptPath string, timeoutSec int, stdout, stderr io.Writer) (ExecInfo, error)
}

// platformExecutor 返回当前平台的执行域实现。
//
// 由各平台文件提供（bash_unix.go / bash_windows.go），bash.go 只调本函数。
// 抽成函数而非包级变量：保持无状态、无初始化顺序依赖（与本包既有风格一致）。
//
// 测试注入：executorOverride 非 nil 时优先使用（仅测试设置，见 executor_test.go）。
// 用 atomic.Pointer 而非裸变量：测试可能并发跑（t.Parallel），读写需同步。
func platformExecutor() executor {
	if p := executorOverride.Load(); p != nil {
		return *p
	}
	return defaultExecutor{}
}

// executorOverride 测试专用执行域覆盖（生产恒为 nil）。
// 仅 executor_test.go 写入；用 atomic.Pointer 保证与并发读（toolBash）无数据竞争。
var executorOverride atomic.Pointer[executor]

// detachHint / detachHintBrief 返回「让长驻服务脱离本次执行域」的平台专属指引。
//
// 为什么必须分平台（阶段 4）：
//   - Unix 用 `setsid` 脱离进程组，`/tmp`、`< /dev/null`、`curl` 都是 Unix 习语；
//   - Windows **没有 setsid**，Git Bash 里也没有；`/tmp` 在 Windows 是映射或不存在。
//     若把 Unix 文案原样喂给 Windows 上的模型，它会照着跑一条**必然失败**的命令。
//
// 这两个函数**进入模型可见的文本**（工具描述 + 超时自述），故必须与平台一致。
// 由 bash_unix.go / bash_windows.go 分别提供；返回值对同平台恒定（不影响 KV 前缀稳定）。
//
// detachHint(toSec int) 用于工具描述（含超时数字）；detachHintBrief() 用于超时自述文案
// （不含数字，与既有 `退出码: N（超过 X 秒…）` 拼接）。

// joinDegraded 合并降级说明（多条用 "; " 连接），空串被忽略。
func joinDegraded(existing, add string) string {
	switch {
	case existing == "":
		return add
	case add == "":
		return existing
	default:
		return existing + "; " + add
	}
}
