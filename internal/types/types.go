package types

// OkHuman 核心类型（对应 TS src/types.ts + compose.ts 的会话类型）。
// 单一共享类型层：llm / context / persist / agent / server 全依赖本包，
// 与 TS 版"types.ts 是唯一类型源"同构。

import (
	"encoding/json"
	"fmt"
)

// ---------- LLM wire 格式（OpenAI 兼容 / llama.cpp /v1/chat/completions） ----------

// ToolCall 工具调用（arguments 双向 JSON 字符串）
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type Function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ContentPart 三型：text | image_url | inject_ref。
// inject_ref 仅存于会话；组装请求时解析成真实 parts（侧车文件），永不上 wire。
// ContentPart 内容 part：主进程=透传管道（2026-09-20 解耦重构）。
// 只认"带非空 type 字段的 JSON 对象"，负载原样转发给 LLM 端点（part 语义属于
// LLM，如 llama-server 的 input_video / image_url / 未来新类型，主程序零感知）；
// 附件出问题的处置不再自动降级（2026-10-18 删 4xx 阶梯）：由人显式 POST
// /strip-injects，把侧车永久回写成文本存根（inject.StripSidecar）。
// Type/Text/Ref 是解析时提取的便捷字段（inject_ref 识别、降级、字符统计、
// /history 展示用），不参与序列化。
type ContentPart struct {
	Type string          `json:"-"`
	Raw  json.RawMessage `json:"-"`
	Text string          `json:"-"` // text part 的正文
	Ref  string          `json:"-"` // inject_ref part 的引用 id
}

// MarshalJSON 原对象原样输出（KV 前缀逐字节稳定）
func (p ContentPart) MarshalJSON() ([]byte, error) {
	if len(p.Raw) == 0 {
		return nil, fmt.Errorf("ContentPart.Raw 为空（须经 TextPart/RefPart/FromPartMap 或 JSON 反序列化构造）")
	}
	return p.Raw, nil
}

// UnmarshalJSON 原字节原样保留，提取 type/text/ref
func (p *ContentPart) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("part 须为 JSON 对象: %w", err)
	}
	var t string
	if err := json.Unmarshal(m["type"], &t); err != nil || t == "" {
		return fmt.Errorf("part 须带非空 type 字段")
	}
	p.Type = t
	if v, ok := m["text"]; ok {
		_ = json.Unmarshal(v, &p.Text)
	}
	if v, ok := m["ref"]; ok {
		_ = json.Unmarshal(v, &p.Ref)
	}
	p.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// TextPart 构造 text part
func TextPart(text string) ContentPart {
	raw, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	return ContentPart{Type: "text", Text: text, Raw: raw}
}

// RefPart 构造 inject_ref part
func RefPart(id string) ContentPart {
	raw, _ := json.Marshal(map[string]string{"type": "inject_ref", "ref": id})
	return ContentPart{Type: "inject_ref", Ref: id, Raw: raw}
}

// FromPartMap 从通用 map 构造（server/persist 解析路径；map marshal 键序归一，
// 内容逐字节确定 → 前缀稳定）
func FromPartMap(m map[string]interface{}) (ContentPart, error) {
	t, ok := m["type"].(string)
	if !ok || t == "" {
		return ContentPart{}, fmt.Errorf("part 须为带非空 type 的对象")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return ContentPart{}, err
	}
	p := ContentPart{Type: t, Raw: raw}
	if v, ok := m["text"].(string); ok {
		p.Text = v
	}
	if v, ok := m["ref"].(string); ok {
		p.Ref = v
	}
	return p, nil
}

// Content 消息内容：纯文本(string) 或 content parts([]ContentPart)
type Content any

// Message LLM 请求消息
type Message struct {
	Role             string     `json:"role"`
	Content          Content    `json:"content"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       *string    `json:"tool_call_id,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
}

// Usage token 用量
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

// Response 一轮请求的聚合响应
type Response struct {
	Content          string     `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	FinishReason     *string    `json:"finish_reason,omitempty"`
	Usage            Usage      `json:"usage"`
	// BadFrames：流式响应里解析不出来的帧数（2026-10-18 复查修 2）。旧实现把畸形帧
	// 直接跳过、不留任何痕迹 → 回复少了一截无从查起。现在计数随响应带出，并在
	// 客户端日志里留痕；超阈值直接报错终止本轮（见 llm.MalformedStreamError）。
	BadFrames int `json:"bad_frames,omitempty"`
}

// Delta 流式增量
type Delta struct {
	Thinking string
	Text     string
}

// ToolSpec 内部工具定义（静态注入，数组永远不变 → KV 前缀稳定）
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  ToolParameters `json:"parameters"`
}

type ToolParameters struct {
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties"`
	Required   []string               `json:"required"`
}

// ---------- 会话状态（2026-09-07 简化：单滚动总结 + 近期原文） ----------

// RawEntry 会话内一条逻辑消息
type RawEntry struct {
	Role             string     `json:"role"`
	Content          Content    `json:"content"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       *string    `json:"tool_call_id,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
}

// TEntry 滚动 summary
type TEntry struct {
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

// SessionState 会话运行态（旧→新）：[system] → [summary(0/1)] → [messages]
type SessionState struct {
	ID       int        `json:"id"`
	Summary  *TEntry    `json:"summary"`
	Messages []RawEntry `json:"messages"`
}

// ---------- 事件流 ----------

// AgentEvent 事件（JSON 对象；map 形态保证与 TS 版逐字段同形）
type AgentEvent map[string]interface{}

// ---------- 工具执行结果 ----------

// ToolOutcome 元工具一次执行的结构化结果（2026-10-25）。
//
// Text = 回填给模型的完整文本：失败时**仍然带 tools.ToolFailPrefix 前缀**，
// 模型侧看到的字符串与旧实现逐字相同（KV 缓存前缀、既有认知都不变）。
// OK   = 工具层是否成功（参数合法、工具存在、未 panic）。
//
// OK 的语义边界（刻意与旧 HasPrefix 判定一致，不改）：bash 命令**退出码非 0
// 仍是 OK=true** —— 命令失败是正常输出，不是工具层故障。要不要把退出码纳入
// 失败，是另一个待裁定的决定，不由本类型偷偷改变。
//
// 为什么要这个类型：旧实现把成败编进文本前缀，消费方用
// strings.HasPrefix(result, ToolFailPrefix) 反推 → 结果正文恰好以那句话开头
// 就被误判（成功报成失败 → agent 重跑命令 → 重复副作用）。判定改走字段，
// 文本前缀退化为纯展示。
type ToolOutcome struct {
	Text string
	OK   bool
}

// ---------- 实例消息队列 ----------

// 队列条目的语义类型。
const (
	QueueKindUser     = "user"      // 用户消息（/chat、/enqueue、心跳、定时任务）
	QueueKindBGNotice = "bg_notice" // 后台任务完成通知
)

// QueuedMessage 实例消息队列的一条（2026-10-25）：取代旧的裸 string。
//
// 旧实现里"用户追加消息"与"后台完成通知"混在同一条 []string 队列里，
// 消费方只能靠 strings.HasPrefix(text, "（系统通知：后台任务完成") 猜语义
// ——文案一改判定就静默失效，正文恰好以那句话开头就被贴错标签。
// 语义挪进 Kind 字段后，Text 是什么都不影响判定。
//
// 对外契约不变：/queue、/status、/events hello 里的 queued 仍是字符串数组
// （见 server.queuedTexts）。
type QueuedMessage struct {
	Text string `json:"text"`
	Kind string `json:"kind"`
}

// ---------- 后台任务 ----------

// PendingBackgroundTask 后台任务（settle 前字段未定）
type PendingBackgroundTask struct {
	CallID     string
	ToolName   string
	StartedAt  int64
	Settled    bool
	SettledAt  int64
	DurationMs int64
	OK         bool
	Result     string
	// Notified settle 通知已入队（2026-09-14：与用户追加同一队列）
	Notified bool
}

// BackgroundStartArgs 工具转后台时的回调参数
type BackgroundStartArgs struct {
	CallID    string
	ToolName  string
	Args      interface{}
	StartedAt int64
	Promise   chan ToolOutcome // 结果通道（永不关闭，恰好一个值）
	// OnSettled settle 时的回调（2026-09-14）：编排器在任务 settle 后调用，
	// 由调用方（server.runOpt）实现：格式化通知 → 入消息队列 + 唤醒 drain
	OnSettled func(*PendingBackgroundTask)
}

// ---------- 小工具 ----------

// StringPtr 字符串指针（JSON omitempty 可空字段用）
func StringPtr(s string) *string { return &s }

// ContentIsParts 内容是否 parts 数组
func ContentIsParts(c Content) bool {
	_, ok := c.([]ContentPart)
	return ok
}

// AsParts 取 parts（非数组返回 nil）
func AsParts(c Content) []ContentPart {
	if p, ok := c.([]ContentPart); ok {
		return p
	}
	return nil
}

// AsString 取字符串（非 string 返回 ""）
func AsString(c Content) string {
	if s, ok := c.(string); ok {
		return s
	}
	return ""
}

// HasInjectRef 消息内容是否含附件引用
func (e *RawEntry) HasInjectRef() bool {
	for _, p := range AsParts(e.Content) {
		if p.Type == "inject_ref" {
			return true
		}
	}
	return false
}

// ContentJSON 内容序列化（/history 端点原样回传用）
func ContentJSON(c Content) interface{} { return c }

var _ = json.Marshal

// ToolCallIDOrUnknown tool 消息的 call id（缺失时 "unknown"，会话记录用）
func (e *RawEntry) ToolCallIDOrUnknown() string {
	if e.ToolCallID != nil && *e.ToolCallID != "" {
		return *e.ToolCallID
	}
	return "unknown"
}
