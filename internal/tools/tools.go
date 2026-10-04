package tools

// 工具系统（2026-09-07）：唯一元工具 bash。一切外部操作（文件读写改、搜索、
// 抓被截断的 log 全文）都靠 bash 完成。
//
// 工具表由配置现生成（2026-10-02：命令总时长上限从硬编码 600s 改为
// tools.timeout_ms 可配）：同配置 → 逐字同文本 → KV 前缀稳定。改 tools 段
// 会让描述变化，代价是前缀作废一次——这是必要的：描述里的超时数字必须与
// 实际执行一致，否则模型基于假数字做决策（旧实现正是配置可改、描述写死
// 30s/600s，两处不一致）。
//
// 本文件是本包的对外契约与工具分发中枢：新增元工具时，在 ExecuteTool 的
// switch 里登记一个 case，并把该工具的实现放到同名的 <工具名>.go。
// 工具表（注入 LLM 的 spec）在 config.go 的 buildTools 里生成。

import (
	"fmt"
)

// ToolFailPrefix 工具执行失败结果的统一前缀（agent 与后台判定成功/失败用）
const ToolFailPrefix = "工具执行失败: "

// ExecuteTool 执行工具（唯一 bash；错误以返回 error 抛出，由 agent 并入字符串）。
//
// 返回契约：成功 → (输出, nil)；失败 → ("", error)。实现约定**不会**返回
// (非空输出, 非 nil error)：toolBash 的错误路径一律返回空串，命令自身的非零
// 退出码（含被超时杀掉）算成功返回（"退出码: N" 落在输出里，err 为 nil）。
//
// ToolFailPrefix 的判定前提：agent 在 err != nil 时拼 ToolFailPrefix+err.Error()，
// background 侧用 HasPrefix 判定失败。该判定成立依赖"成功输出恒以 '退出码: '
// 开头、绝不以 ToolFailPrefix 开头"——此不变量由 toolBash 的 head 前缀保证，
// 改动 toolBash 的输出格式时必须一并守住（否则背景任务成败会误判）。
func ExecuteTool(name string, args map[string]interface{}) (string, error) {
	switch name {
	case "bash":
		return toolBash(args) // 参数错误 → error，由 agent 并入 ToolFailPrefix 字符串（与 TS throw→catch 等价）
	default:
		// %q 转义：name 来自 LLM 的 tool_calls[].function.name（模型可控），
		// 可能含换行/ANSI 等，直接 %s 会污染日志与 LLM 上下文。
		// 不写"唯一"二字：本 switch 是可扩展点，硬编码工具数量会随新增工具过时。
		return "", fmt.Errorf("未知工具: %q", name)
	}
}
