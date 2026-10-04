package tools

// 本文件承载本包的配置与全局状态：超时常量、包级原子状态、Configure/Specs
// 与工具表工厂 buildTools。改超时语义时只需看这一个文件——描述文本
// （buildTools）与实际执行上限（toolTimeoutMS）必须同源。

import (
	"fmt"
	"sync/atomic"

	"okhuman/internal/types"
)

// 超时默认与可配上限（2026-10-02）
const (
	DefaultToolTimeoutMS = 600000   // 命令总时长上限默认 600s（与旧硬编码一致）
	MaxToolTimeoutMS     = 86400000 // 可配上限 24h：再大等于没有兜底
	DefaultFgTimeoutMS   = 30000
)

var (
	toolTimeoutMS atomic.Int64 // 命令总时长上限（毫秒）
	toolFgMS      atomic.Int64 // 前台等待（毫秒）——仅用于生成描述文本
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
