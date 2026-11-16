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
// → inject.StripSidecar 永久回写侧车（见 server.handleStripInjects）。

import (
	"fmt"
	"math"
	"okhuman/internal/textutil"
	"strings"
	"sync/atomic"

	"okhuman/internal/inject"
	"okhuman/internal/types"
)

// SummaryMarker 历史总结消息的 role:user 前缀（/history 与压缩存档共用识别）
const SummaryMarker = "[历史总结]"

// RenderSummary 把一条总结渲染成注入上下文的 user 消息：marker →（若被硬截断）一行提醒
// → 正文 → 被压掉的完整原文路径。只有正文参与计量（见 Manager 的 summary_max_chars）；
// 提醒与路径都不截断 —— 正文被截断了更要去读原文。
func RenderSummary(sm types.SummaryEntry) string {
	var b []string
	b = append(b, SummaryMarker)
	if sm.Truncated {
		b = append(b, fmt.Sprintf("⚠️ 本条总结是硬截断的：只保留了前面一部分，其余不在这里 —— 需要时按下面的原文路径去读。"))
	}
	b = append(b, sm.Content)
	if sm.RecordFile != "" {
		b = append(b, "[本条覆盖的完整原文已存档："+sm.RecordFile+" —— 需要细节时用 bash cat/grep 按需抓回]")
	}
	return strings.Join(b, "\n")
}

// ToolResultMarker 孤儿 tool 降级 user 文本的前缀（与旧 TS 版一致，供对比）
const ToolResultMarker = "[工具结果"

// missingToolResult 补在「声明了 tool_calls 却没有结果」那一轮后面的占位结果。
// 成因：assistant+tool_calls 先落盘、tool 结果逐条追加，进程在中间被 kill（重启/OOM）。
const missingToolResult = "（结果缺失：进程在工具执行中被中断，这条调用的结果没有落盘）"

// 全局注入解析器（server 包启动时 ConfigureInjectResolver；/reset 重建）。
// 原子指针（2026-09-14 审计修：旧裸指针在 /inject 触发的压缩（读）与 /reset
// 重建（写）并发时数据竞争）
var resolveInject atomic.Pointer[inject.Resolver]

// ConfigureInjectResolver 启动/重置时设置全局侧车解析器
func ConfigureInjectResolver(r *inject.Resolver) { resolveInject.Store(r) }

// ClearResolverCacheEntry 清全局解析器的指定 id 缓存——侧车文件被改写后必须调
// （/strip-injects 手动剥离（/drop 已删）），否则解析器内存缓存继续返回旧 parts，
// "已剥离/已删"的附件下一轮照样发出去。
func ClearResolverCacheEntry(id string) {
	if r := resolveInject.Load(); r != nil {
		r.ClearCache(id)
	}
}

// ComposeMessages 会话状态 → LLM 请求（纯函数）
func ComposeMessages(session *types.SessionState, systemPrompt string) []types.Message {
	// 两个方向的残缺各要一条路径处理：
	//  - tool 消息的 call_id 不在任何 assistant.tool_calls 里 → 降级成 user 文本（保 role 交替合法）；
	//  - assistant 声明了 tool_calls 却没有对应 tool 结果 → 补一条占位结果。后者是 OpenAI
	//    协议的硬要求（每个 tool_call 必须有对应 tool 消息，否则整个请求被拒），且没有它
	//    模型就看不见「这次调用发生过」，会把同一条命令再跑一遍。
	callIds := map[string]bool{}
	answered := map[string]bool{}
	for i := range session.Messages {
		for _, tc := range session.Messages[i].ToolCalls {
			callIds[tc.ID] = true
		}
		if session.Messages[i].Role == "tool" && session.Messages[i].ToolCallID != nil {
			answered[*session.Messages[i].ToolCallID] = true
		}
	}
	out := []types.Message{{Role: "system", Content: systemPrompt}}
	for _, sm := range session.Summaries {
		out = append(out, types.Message{Role: "user", Content: RenderSummary(sm)})
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
			for _, tc := range e.ToolCalls {
				if answered[tc.ID] {
					continue
				}
				id := tc.ID
				out = append(out, types.Message{Role: "tool", ToolCallID: &id, Content: missingToolResult})
			}
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

// Est 估算口径：字符↔token 比例 + 单个附件的 token 成本。
// 附件成本在配置里就是按 token 写的（context.attachment_tokens），这里换算成字符
// 只是为了和"字符预算"（keep_recent_chars / hard_trunc_chars）同一把尺子。
type Est struct {
	Cpl          float64
	AttachTokens int
}

// defaultCharsPerToken + cplOrDefault：字符↔token 的默认换算比例只在这里定义一次
// （原先 NewEst / EstimateTokens / EstimateMessageTokens 各写了一份 1.5）。
const defaultCharsPerToken = 1.5

func cplOrDefault(cpl float64) float64 {
	if cpl <= 0 {
		return defaultCharsPerToken
	}
	return cpl
}

// NewEst 从配置造估算口径（cpl 非正数退回默认，负附件按 0）。
func NewEst(cpl float64, attachTokens int) Est {
	cpl = cplOrDefault(cpl)
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
		return textutil.UTF16Len(s)
	}
	n := 0
	for _, p := range types.AsParts(c) {
		if p.Type == "text" {
			n += textutil.UTF16Len(p.Text)
		} else {
			n += e.attachChars()
		}
	}
	return n
}

// MessageChars 单条消息的字符数——对外也只这一把尺子。
// 曾经展示层（/status 的 raw_tail）自己另算一遍、附件按写死的 1000 字符计，
// 而这里按配置 context.attachment_tokens 换算：同一次响应里两个数就漂移了
// （2026-11-12 审查）。要字符数就调这个，别再在调用方拼一遍。
func (e Est) MessageChars(m types.RawEntry) int { return messageChars(m, e) }

// messageChars 消息字符数（content + tool_calls name+args + reasoning）
func messageChars(m types.RawEntry, e Est) int {
	n := contentChars(m.Content, e)
	for _, tc := range m.ToolCalls {
		n += textutil.UTF16Len(tc.Function.Name) + textutil.UTF16Len(tc.Function.Arguments)
	}
	if m.ReasoningContent != nil {
		n += textutil.UTF16Len(*m.ReasoningContent)
	}
	return n
}

// sessionChars 会话总字符数（全部消息，含 summary）
func sessionChars(session *types.SessionState, est Est) int {
	n := 0
	for _, e := range session.Messages {
		n += messageChars(e, est)
	}
	for _, sm := range session.Summaries {
		n += textutil.UTF16Len(sm.Content)
	}
	return n
}

// EstimateTokens 字符数估算 token（token≈chars/chars_per_token，向上取整）
func EstimateTokens(text string, cpl float64) int {
	return int(math.Ceil(float64(textutil.UTF16Len(text)) / cplOrDefault(cpl)))
}

// EstimateMessageTokens 单条消息估算 token = 字符数 / cpl。
// 字符计数一律走 messageChars —— 同一个口径只留一处。此前这里自己又写了一遍，
// 且只在 role=="assistant" 时计 tool_calls/reasoning、tool_calls 还按字节计（2026-11-12
// 四方审查发现）：两条路径已经漂移，压缩预算循环走的那条会把中文参数多算 3 倍。
// 现在非 assistant 却带 tool_calls 的条目也会被计入 —— 宁可多算也不少算。
func EstimateMessageTokens(m types.RawEntry, e Est) int {
	return int(math.Ceil(float64(messageChars(m, e)) / cplOrDefault(e.Cpl)))
}

// ContextStats /health 的上下文统计
// SummaryStat 单条摘要在状态页上的展示项
type SummaryStat struct {
	SeqFrom    int    `json:"seq_from"`
	SeqTo      int    `json:"seq_to"`
	CreatedAt  int64  `json:"created_at"`
	Chars      int    `json:"chars"`
	OrigChars  int    `json:"orig_chars"`
	Truncated  bool   `json:"truncated"`
	RecordFile string `json:"record_file"`
}

type ContextStats struct {
	Messages        int           `json:"messages"`
	MaxTokens       int           `json:"max_tokens"` // 生效的 context.max_tokens（WebUI 用量条一直读这个字段，此前不存在）
	SummaryCount    int           `json:"summary_count"`
	SummaryLimit    int           `json:"summary_limit"`     // context.keep_summaries
	SummaryMaxChars int           `json:"summary_max_chars"` // 单条正文硬上限
	SummaryTruncCnt int           `json:"summary_truncated"` // 其中被硬截断的条数
	SummaryStats    []SummaryStat `json:"summaries"`
	MsgTokens       int           `json:"msg_tokens"`
	SummaryTokens   int           `json:"summary_tokens"`
	TotalTokens     int           `json:"total_tokens"`
	MsgChars        int           `json:"msg_chars"`
	SummaryChars    int           `json:"summary_chars"`
}

// Stats /health 与 /status 的上下文统计：会话与**生效**配置在同一把锁里一次取走。
// 原先由 server 传 cfg.Context.*（config 原值）进来，keep_summaries 配成 5 就回显 5，
// 而窗口实际是 1——两处口径不一致（2026-11-16 收到这里，调用方不再自己拼配置）。
func (m *Manager) Stats() ContextStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := ContextStatsOf(m.session, m.cfg.CharsPerToken, m.cfg.AttachmentTokens, m.cfg.KeepSummaries, m.cfg.SummaryMaxChars)
	st.MaxTokens = m.cfg.MaxTokens
	return st
}

// ContextStatsOf 会话状态 → /health 统计
func ContextStatsOf(session *types.SessionState, cpl float64, attachTokens int, keepSummaries, summaryMaxChars int) ContextStats {
	est := NewEst(cpl, attachTokens)
	msgTokens := 0
	msgChars := 0
	for _, e := range session.Messages {
		msgTokens += EstimateMessageTokens(e, est)
		msgChars += messageChars(e, est)
	}
	var summaryTokens, summaryChars, truncCnt int
	stats := make([]SummaryStat, 0, len(session.Summaries))
	for _, sm := range session.Summaries {
		ch := textutil.UTF16Len(sm.Content)
		summaryChars += ch
		summaryTokens += EstimateTokens(sm.Content, cpl)
		if sm.Truncated {
			truncCnt++
		}
		stats = append(stats, SummaryStat{SeqFrom: sm.SeqFrom, SeqTo: sm.SeqTo, CreatedAt: sm.CreatedAt,
			Chars: ch, OrigChars: sm.OrigChars, Truncated: sm.Truncated, RecordFile: sm.RecordFile})
	}
	return ContextStats{
		Messages: len(session.Messages), SummaryCount: len(session.Summaries),
		SummaryLimit: keepSummaries, SummaryMaxChars: summaryMaxChars,
		SummaryTruncCnt: truncCnt, SummaryStats: stats,
		MsgTokens: msgTokens, SummaryTokens: summaryTokens,
		TotalTokens: msgTokens + summaryTokens,
		MsgChars:    msgChars, SummaryChars: summaryChars,
	}
}
