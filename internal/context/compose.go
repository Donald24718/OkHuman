package context

// 请求组装（2026-08-18）：把会话状态转成发给 LLM 的请求。
// - summary → 第 2 条 user 消息（marker 前缀），保持 role 交替合法
// - 孤儿 tool（无对应 assistant.tool_calls）→ 降级 user 文本
// - inject_ref → 经全局侧车解析器换成真实 parts（compose 时读文件 + 缓存）
//
// 上下文注入（2026-09-11）：/inject 的 base64 parts 存侧车文件，会话只存
// 轻量 inject_ref。本模块是唯一的"引用→真实 parts"解析点：LLM 永远看不到
// 引用，只见真实附件；侧车写入后不可变 → 解析确定性 → KV 前缀逐 token 稳定。
// demote 回退（2026-09-13）：LLM 4xx（上下文超长 413/415 或 base64 图撑爆
// 请求 400）→ 注入附件降级为文本存根再试：
//   - newest：只降最近一条带注入的消息（更早的图保真 → KV 前缀尽量不动）
//   - all：全部降级

import (
	"fmt"
	"math"
	"sync/atomic"

	"okhuman/internal/inject"
	"okhuman/internal/types"
)

// SummaryMarker 历史总结消息的 role:user 前缀（/history 与压缩存档共用识别）
const SummaryMarker = "[历史总结]"

// ToolResultMarker 孤儿 tool 降级 user 文本的前缀（与 8452 树一致，供对比）
const ToolResultMarker = "[工具结果"

// 全局注入解析器（server 包启动时 ConfigureInjectResolver；/reset 重建）。
// 原子指针（2026-09-14 审计修：旧裸指针在 /inject 触发的压缩（读）与 /reset
// 重建（写）并发时数据竞争）
var resolveInject atomic.Pointer[inject.Resolver]

// ConfigureInjectResolver 启动/重置时设置全局侧车解析器
func ConfigureInjectResolver(r *inject.Resolver) { resolveInject.Store(r) }

// ClearResolverCacheEntry 清全局解析器的指定 id 缓存（侧车被改写后调用，如 4xx
// 降级回写；2026-09-19）——否则解析器内存缓存继续返回旧（坏）parts。
func ClearResolverCacheEntry(id string) {
	if r := resolveInject.Load(); r != nil {
		r.ClearCache(id)
	}
}

// ComposeOptions demote 模式（""=正常 / "newest"=只降最近注入 / "all"=全降）
type ComposeOptions struct {
	DemoteInjects string
}

// demote 模式取值（2026-10-05 提为常量：agent 侧的回退阶梯与此处各写一份字符串
// 字面量时，两边漂移过一次——详见 ComposeResult 注释）
const (
	DemoteNone   = ""
	DemoteNewest = "newest"
	DemoteAll    = "all"
)

// ComposeResult Compose 的产物（2026-10-05）：
//
//	DemotedInjectIDs 是本次【真正被降级】的注入 id——由组装过程就地记下，
//	而非调用方按"哪些消息带附件"自行重算。降级范围只在组装里定义一次，
//	调用方（agent 4xx 阶梯的侧车回写）只能消费它。
//	历史 bug：调用方重算时用错了 mode 字符串，导致 demote-newest 成功后
//	把会话里【全部】附件永久回写成文本提示（更早的好附件被不可逆降级）。
type ComposeResult struct {
	Messages         []types.Message
	DemotedInjectIDs []string
}

// ComposeMessages 会话状态 → LLM 请求 + 本次实际降级的注入 id（纯函数）
func ComposeMessages(session *types.SessionState, systemPrompt string, opts *ComposeOptions) ComposeResult {
	// 孤儿 tool 判定：tool_call_id 不在任何 assistant.tool_calls 里
	callIds := map[string]bool{}
	for i := range session.Messages {
		for _, tc := range session.Messages[i].ToolCalls {
			callIds[tc.ID] = true
		}
	}
	newestInjectIdx := -1
	if opts != nil && opts.DemoteInjects == DemoteNewest {
		for i := len(session.Messages) - 1; i >= 0; i-- {
			if session.Messages[i].HasInjectRef() {
				newestInjectIdx = i
				break
			}
		}
	}
	demoted := []string{}
	seen := map[string]bool{}
	noteDemoted := func(parts []types.ContentPart) {
		for _, p := range parts {
			if p.Type == "inject_ref" && p.Ref != "" && !seen[p.Ref] {
				seen[p.Ref] = true
				demoted = append(demoted, p.Ref)
			}
		}
	}
	out := []types.Message{{Role: "system", Content: systemPrompt}}
	if session.Summary != nil {
		out = append(out, types.Message{Role: "user", Content: SummaryMarker + "\n" + session.Summary.Content})
	}
	for i, e := range session.Messages {
		if e.Role == "tool" {
			tcid := ""
			if e.ToolCallID != nil {
				tcid = *e.ToolCallID
			}
			if callIds[tcid] {
				out = append(out, types.Message{Role: "tool", ToolCallID: &tcid, Content: e.Content})
			} else {
				// 孤儿 tool → 降级 user 文本，保证 role 交替合法
				label := tcid
				if label == "" {
					label = "unknown"
				}
				out = append(out, types.Message{Role: "user", Content: fmt.Sprintf("%s %s]\n%s", ToolResultMarker, label, types.AsString(e.Content))})
			}
		} else {
			demote := (opts != nil && opts.DemoteInjects == DemoteAll) ||
				(opts != nil && opts.DemoteInjects == DemoteNewest && i == newestInjectIdx)
			m := types.Message{Role: e.Role, Content: e.Content}
			if parts := types.AsParts(e.Content); parts != nil {
				if demote {
					noteDemoted(parts) // 就地记录，是降级范围的唯一真相来源
				}
				m.Content = resolveContentParts(parts, demote)
			}
			if e.Role == "assistant" && len(e.ToolCalls) > 0 {
				m.ToolCalls = e.ToolCalls
			}
			if e.Role == "assistant" && e.ReasoningContent != nil {
				m.ReasoningContent = e.ReasoningContent
			}
			out = append(out, m)
		}
	}
	return ComposeResult{Messages: out, DemotedInjectIDs: demoted}
}

// resolveContentParts 把 content parts 里的 inject_ref 解析成真实 parts（带缓存）；
// demote 时注入引用降级为文本存根
func resolveContentParts(parts []types.ContentPart, demote bool) []types.ContentPart {
	out := []types.ContentPart{}
	for _, p := range parts {
		if p.Type == "inject_ref" {
			resolved := []types.ContentPart(nil)
			if r := resolveInject.Load(); r != nil {
				resolved = r.Resolve(p.Ref)
			}
			if demote && resolved != nil {
				out = append(out, types.TextPart(fmt.Sprintf("（附件 %s 本轮未发送，仅以文本提示保留）", p.Ref)))
			} else if resolved != nil {
				out = append(out, resolved...)
			} else {
				out = append(out, types.TextPart(fmt.Sprintf("（附件 %s 已清理或丢失）", p.Ref)))
			}
		} else {
			out = append(out, p)
		}
	}
	return out
}

// utf16Len 字符数（UTF-16 code units，对齐 TS text.length 口径——所有预算/
// 统计统一用它，中文 BMP 字符各计 1、代理对计 2）
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// contentChars 内容字符数（string=长度；parts=文本总长 + 每图/每引用固定 1000）
func contentChars(c types.Content) int {
	if s, ok := c.(string); ok {
		return utf16Len(s)
	}
	n := 0
	for _, p := range types.AsParts(c) {
		if p.Type == "text" {
			n += utf16Len(p.Text)
		} else {
			n += 1000
		}
	}
	return n
}

// messageChars 消息字符数（content + tool_calls name+args + reasoning）
func messageChars(m types.RawEntry) int {
	n := contentChars(m.Content)
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	if m.ReasoningContent != nil {
		n += utf16Len(*m.ReasoningContent)
	}
	return n
}

// sessionChars 会话总字符数（全部消息，含 summary）
func sessionChars(session *types.SessionState) int {
	n := 0
	for _, e := range session.Messages {
		n += messageChars(e)
	}
	if session.Summary != nil {
		n += utf16Len(session.Summary.Content)
	}
	return n
}

// EstimateTokens 字符数估算 token（token≈chars/chars_per_token，向上取整）
func EstimateTokens(text string, cpl float64) int {
	if cpl <= 0 {
		cpl = 1.5
	}
	return int(math.Ceil(float64(utf16Len(text)) / cpl))
}

// EstimateMessageTokens 单条消息估算 token（assistant 加 tool_calls name+args 与 reasoning）
func EstimateMessageTokens(m types.RawEntry, cpl float64) int {
	chars := contentChars(m.Content)
	if m.Role == "assistant" {
		for _, tc := range m.ToolCalls {
			chars += utf16Len(tc.Function.Name) + utf16Len(tc.Function.Arguments)
		}
		if m.ReasoningContent != nil {
			chars += utf16Len(*m.ReasoningContent)
		}
	}
	if cpl <= 0 {
		cpl = 1.5
	}
	return int(math.Ceil(float64(chars) / cpl))
}

// ContextStats /health 的上下文统计
type ContextStats struct {
	Messages      int `json:"messages"`
	Summary       int `json:"summary"`
	MsgTokens     int `json:"msg_tokens"`
	SummaryTokens int `json:"summary_tokens"`
	TotalTokens   int `json:"total_tokens"`
	MsgChars      int `json:"msg_chars"`
	SummaryChars  int `json:"summary_chars"`
}

// ContextStatsOf 会话状态 → /health 统计
func ContextStatsOf(session *types.SessionState, cpl float64) ContextStats {
	msgTokens := 0
	msgChars := 0
	for _, e := range session.Messages {
		msgTokens += EstimateMessageTokens(e, cpl)
		msgChars += messageChars(e)
	}
	var summaryTokens, summaryChars, summaryCount int
	if session.Summary != nil {
		summaryCount = 1 // 2026-09-14 修：原硬编码 0，导致 WebUI 已压缩仍显示 [summary ×0]
		summaryTokens = EstimateTokens(session.Summary.Content, cpl)
		summaryChars = utf16Len(session.Summary.Content)
	}
	return ContextStats{
		Messages:      len(session.Messages),
		Summary:       summaryCount,
		MsgTokens:     msgTokens,
		SummaryTokens: summaryTokens,
		TotalTokens:   msgTokens + summaryTokens,
		MsgChars:      msgChars,
		SummaryChars:  summaryChars,
	}
}
