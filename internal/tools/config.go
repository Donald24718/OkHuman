package tools

// 本文件承载本包的配置与全局状态：超时常量、包级原子状态、Configure/Specs
// 与工具表工厂 buildTools。改超时语义时只需看这一个文件——描述文本
// （buildTools）与实际执行上限（toolTimeoutMS）必须同源。

import (
	"fmt"
	"sync/atomic"

	"okhuman/internal/tools/ipython"
	"okhuman/internal/types"
)

// 超时默认与可配上限（2026-10-02）
const (
	DefaultToolTimeoutMS = 600000   // 命令总时长上限默认 600s（与旧硬编码一致）
	MaxToolTimeoutMS     = 86400000 // 可配上限 24h：再大等于没有兜底
	DefaultFgTimeoutMS   = 30000

	// DefaultIPythonMS ipython 单次执行的默认上限。
	//
	// 为什么要比前台等待（30s）长：前台到点会转后台继续跑（agent 侧机制），
	// 所以默认给到 60s 不会让模型干等，却能覆盖 `import pandas` 这类冷启动。
	// 需要更久时模型显式传 timeout_seconds（上限同 tools.timeout_ms）。
	DefaultIPythonMS = 60000
)

var (
	toolTimeoutMS atomic.Int64 // 命令总时长上限（毫秒）
	toolFgMS      atomic.Int64 // 前台等待（毫秒）——仅用于生成描述文本
	toolsSpec     atomic.Pointer[[]types.ToolSpec]

	// ipythonReady 决定是否把 ipython 注册进工具表。
	// 未配置 tools.ipython_python 时为 false —— 工具表与现状逐字一致，
	// 既有部署的 KV 前缀不会因为本次升级而失效。
	ipythonReady atomic.Bool
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
	rebuildTools(fgMS, timeoutMS)
}

// ConfigureIPython 启用/禁用 ipython 工具并重建工具表。
//
// python 为空串 → 禁用；"auto" → 自动探测；其余按解释器路径校验。
// 返回 error 仅表示「启用了但探测失败」（未配置而禁用不是错误），
// 调用方应记录日志而非中断启动。
//
// 必须在 Configure 之后调用：ipython 的超时上限取自 tools.timeout_ms。
func ConfigureIPython(python string) error {
	err := ipython.Setup(python, DefaultIPythonMS, int(toolTimeoutMS.Load()))
	ipythonReady.Store(ipython.Enabled())
	rebuildTools(int(toolFgMS.Load()), int(toolTimeoutMS.Load()))
	return err
}

// IPythonEnabled 报告 ipython 是否已在工具表中（供启动日志与观测使用）。
func IPythonEnabled() bool { return ipythonReady.Load() }

// ResetIPythonSession 新会话边界（/reset）调用：作废旧会话的 IPython 内核。
//
// 不调用它，重置后的会话会继承上一会话的全部变量——模型以为干净，
// 实际变量都在。未启用 ipython 时是空操作（内核池本来就是空的）。
func ResetIPythonSession(key string) { ipython.ResetSession(key) }

// PythonPathForLog 当前生效的解释器路径（启动日志用）。
func PythonPathForLog() string { return ipython.PythonPath() }

// SetIPythonArtifactDir 指定 ipython 图片/PDF 的落盘根目录（<dir>/ipython-output）。
//
// 用数据目录而非临时目录：临时目录随时可能被系统清理，而模型往往隔几轮才
// 回来读这张图。未调用时 ipython 包退回临时目录，功能不降级。
func SetIPythonArtifactDir(dir string) { ipython.SetArtifactDir(dir) }

// rebuildTools 用当前 fg/总超时重建工具表。
func rebuildTools(fgMS, timeoutMS int) {
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
	specs := []types.ToolSpec{
		{
			Name: "bash",
			Description: "执行 bash 命令（异步并行；输出超长被截断时，全文已写入 log 文件、截断处会给出路径，用 bash 的 cat / grep / tail 抓回）。" +
				fmt.Sprintf("前台等待 %d 秒未完成自动转后台（你收到通知后继续干活，结果在轮内下一个工具调用间隙或下一次 run 带回来）；命令总时长超 %d 秒被强制终止（整个命令进程组）。", fgSec, toSec) +
				detachHint(toSec) +
				tmpHint(), // 平台专属：Unix 为空串，Windows 提示别用 /tmp
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
	if ipythonReady.Load() {
		specs = append(specs, buildIPythonSpec())
	}
	return specs
}

// buildIPythonSpec 生成 ipython 工具规格。
//
// 数字一律取自 ipython.DefaultSec()/MaxSec()，与实际执行时的夹逼同源——
// 绝不在描述里写死数字（否则模型会按假数字决策）。
//
// 描述取材铁律：**写差异，不写常识**（与 bash 同一把尺子）。
//   - 不写用法说明（%magic / !cmd / pandas / In-Out 历史…）：模型的 Python
//     先验里全有，写出来是噪声，还会稀释它对真正关键条款的注意力。
//   - 只写本实例偏离标准行为之处：超时是中断而非杀、变量条件性丢失、
//     没有 stdin、图片走落盘路径而非回传 base64、哪类活该转给 bash。
//   - 反直觉但重要：先验越强越危险——正因为它懂 Python，才会写出 input()，
//     所以"没有交互输入"这条必须显式写。
//
// 注意描述里**不提平台差异**：级联中断已经在双平台实跑通过（Windows 命中
// stage 1、Linux 自动升到 stage 2），行为一致就该给模型一致的承诺。
// 副作用：本段文字跨平台逐字相同（bash 段因 tmpHint/detachHint 不同而不同）。
func buildIPythonSpec() types.ToolSpec {
	defSec, maxSec := ipython.DefaultSec(), ipython.MaxSec()
	return types.ToolSpec{
		Name: "ipython",
		Description: "IPython 内核，变量、导入、函数跨调用保留（常驻进程）。" +
			fmt.Sprintf("单次执行默认 %d 秒、最多 %d 秒；超时先尝试中断（内核保留、变量不丢），无法中断时重启内核（丢变量）。", defSec, maxSec) +
			"图片/PDF 自动落盘并在返回值给出绝对路径；display() 的富内容同样被捕获。" +
			"无交互输入：input() / getpass 会立刻报错，请把值直接写进代码。" +
			"grep/sed 纯文本处理，用 bash。",
		Parameters: types.ToolParameters{
			Type: "object",
			Properties: map[string]interface{}{
				"code": map[string]interface{}{"type": "string",
					"description": "要执行的 Python/IPython 代码"},
				"timeout_seconds": map[string]interface{}{"type": "number",
					"description": fmt.Sprintf("超时秒数（默认 %d，上限 %d）", defSec, maxSec)},
			},
			Required: []string{"code"},
		},
	}
}
