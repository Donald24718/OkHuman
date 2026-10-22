package context

// 请求组装（2026-08-18）：把会话状态转成发给 LLM 的请求。
// - summary → 第 2 条 user 消息（marker 前缀），保持 role 交替合法
// - 孤儿 tool（无对应 assistant.tool_calls）→ 降级 user 文本
// - inject_ref → 经全局侧车解析器换成真实 parts（compose 时读文件 + 缓存）
//
// 上下文注入（2026-09-11）：/inject 的 base64 parts 存侧车文件，会话只存
// 轻量 inject_ref。本模块是唯一的"引用→真实 parts"解析点：LLM 永远看不到
// 引用，只见真实附件；侧车写入后不可变 → 解析确定性 → KV 前缀逐 token 稳定。
// 附件降级（2026-09-13 的"4xx 请求级降级"）已于 2026-10-18 随三级回退阶梯一并
// 删除：触发条件写死 400/413/415 是按 llama.cpp 行为定的，换后端不成立。
// 现在附件的剥离只有显式路径：WebUI「剥离全部注入附件」→ POST /strip-injects
// → inject.PersistDemotion 永久回写侧车（见 server.handleStripInjects）。

import (
	"fmt"
	"math"
	"sync/atomic"

	"okhuman/internal/inject"
	"okhuman/internal/types"
)

// SummaryMarker 历史总结消息的 role:user 前缀（/history 与压缩存档共用识别）
const SummaryMarker = "[历史总结]"

// ToolResultMarker 孤儿 tool 降级 user 文本的前缀（与旧 TS 版一致，供对比）
const ToolResultMarker = "[工具结果"

// 全局注入解析器（server 包启动时 ConfigureInjectResolver；/reset 重建）。
// 原子指针（2026-09-14 审计修：旧裸指针在 /inject 触发的压缩（读）与 /reset
// 重建（写）并发时数据竞争）
var resolveInject atomic.Pointer[inject.Resolver]

// ConfigureInjectResolver 启动/重置时设置全局侧车解析器
func ConfigureInjectResolver(r *inject.Resolver) { resolveInject.Store(r) }

// ClearResolverCacheEntry 清全局解析器的指定 id 缓存——侧车文件被改写后必须调
// （/strip-injects 手动剥离、/drop 删侧车），否则解析器内存缓存继续返回旧 parts，
// "已剥离/已删"的附件下一轮照样发出去。
func ClearResolverCacheEntry(id string) {
	if r := resolveInject.Load(); r != nil {
		r.ClearCache(id)
	}
}

// ComposeMessages 会话状态 → LLM 请求（纯函数）
func ComposeMessages(session *types.SessionState, systemPrompt string) []types.Message {
	// 孤儿 tool 判定：tool_call_id 不在任何 assistant.tool_calls 里
	callIds := map[string]bool{}
	for i := range session.Messages {
		for _, tc := range session.Messages[i].ToolCalls {
			callIds[tc.ID] = true
		}
	}
	out := []types.Message{{Role: "system", Content: systemPrompt}}
	if session.Summary != nil {
		out = append(out, types.Message{Role: "user", Content: SummaryMarker + "\n" + session.Summary.Content})
	}
	for _, e := range session.Messages {
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
			m := types.Message{Role: e.Role, Content: e.Content}
			if parts := types.AsParts(e.Content); parts != nil {
				m.Content = resolveContentParts(parts)
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
	return out
}

// resolveContentParts 把 content parts 里的 inject_ref 解析成真实 parts（带缓存）。
// 侧车已被剥离（/strip-injects）→ 解析出来就是文本提示；侧车丢失 → 丢失存根。
func resolveContentParts(parts []types.ContentPart) []types.ContentPart {
	out := []types.ContentPart{}
	for _, p := range parts {
		if p.Type == "inject_ref" {
			resolved := []types.ContentPart(nil)
			if r := resolveInject.Load(); r != nil {
				resolved = r.Resolve(p.Ref)
			}
			if resolved != nil {
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

// Est 估算口径：字符↔token 比例 + 单个附件的 token 成本。
// 附件成本在配置里就是按 token 写的（context.attachment_tokens），这里换算成字符
// 只是为了和"字符预算"（keep_recent_chars / hard_trunc_chars）同一把尺子。
type Est struct {
	Cpl          float64
	AttachTokens int
}

// NewEst 从配置造估算口径（cpl<=0 退回 1.5，负附件按 0）。
func NewEst(cpl float64, attachTokens int) Est {
	if cpl <= 0 {
		cpl = 1.5
	}
	if attachTokens < 0 {
		attachTokens = 0
	}
	return Est{Cpl: cpl, AttachTokens: attachTokens}
}

// attachChars 一个非文本 part 折算成多少字符。
func (e Est) attachChars() int {
	return int(math.Ceil(float64(e.AttachTokens) * e.Cpl))
}

// contentChars 内容字符数（string=长度；parts=文本总长 + 每个非文本 part 按配置折算）
func contentChars(c types.Content, e Est) int {
	if s, ok := c.(string); ok {
		return utf16Len(s)
	}
	n := 0
	for _, p := range types.AsParts(c) {
		if p.Type == "text" {
			n += utf16Len(p.Text)
		} else {
			n += e.attachChars()
		}
	}
	return n
}

// messageChars 消息字符数（content + tool_calls name+args + reasoning）
func messageChars(m types.RawEntry, e Est) int {
	n := contentChars(m.Content, e)
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	if m.ReasoningContent != nil {
		n += utf16Len(*m.ReasoningContent)
	}
	return n
}

// sessionChars 会话总字符数（全部消息，含 summary）
func sessionChars(session *types.SessionState, est Est) int {
	n := 0
	for _, e := range session.Messages {
		n += messageChars(e, est)
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
func EstimateMessageTokens(m types.RawEntry, e Est) int {
	chars := contentChars(m.Content, e)
	if m.Role == "assistant" {
		for _, tc := range m.ToolCalls {
			chars += utf16Len(tc.Function.Name) + utf16Len(tc.Function.Arguments)
		}
		if m.ReasoningContent != nil {
			chars += utf16Len(*m.ReasoningContent)
		}
	}
	cpl := e.Cpl
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
func ContextStatsOf(session *types.SessionState, cpl float64, attachTokens int) ContextStats {
	est := NewEst(cpl, attachTokens)
	msgTokens := 0
	msgChars := 0
	for _, e := range session.Messages {
		msgTokens += EstimateMessageTokens(e, est)
		msgChars += messageChars(e, est)
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
