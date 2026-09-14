package types

// OkHuman 核心类型（对应 TS src/types.ts + compose.ts 的会话类型）。
// 单一共享类型层：llm / context / persist / agent / server 全依赖本包，
// 与 TS 版"types.ts 是唯一类型源"同构。

import "encoding/json"

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
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	Ref      string    `json:"ref,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
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

func NewSession(id int) *SessionState {
	return &SessionState{ID: id, Messages: []RawEntry{}}
}

// ---------- 事件流 ----------

// AgentEvent 事件（JSON 对象；map 形态保证与 TS 版逐字段同形）
type AgentEvent map[string]interface{}

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
	Promise   chan string // 结果通道（永不关闭，恰好一个值）
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
