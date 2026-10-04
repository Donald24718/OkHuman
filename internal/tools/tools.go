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

// ExecuteTool 执行工具（唯一 bash；错误以返回 error 抛出，由 agent 并入字符串）
func ExecuteTool(name string, args map[string]interface{}) (string, error) {
	switch name {
	case "bash":
		return toolBash(args) // 参数错误 → error，由 agent 并入 ToolFailPrefix 字符串（与 TS throw→catch 等价）
	default:
		return "", fmt.Errorf("未知工具: %s（唯一元工具是 bash）", name)
	}
}
