// FakeLLM：测试用确定性 LLM（不烧 token）。
// 行为由脚本化步骤队列驱动：
//
//	step {Type:"text", Text}            → 普通回复
//	step {Type:"tool", Name, Args}      → 发起工具调用
//	step {Type:"compress", Text, Scope} → 压缩请求的总结输出（由 agent 内部压缩路径触发，
//	                                       按请求特征区分：messages 末尾以 [压缩:scope] 开头）
//
// 2026-09-07 简化：单一压缩场景 scope="history"（取代旧 single/t1/t2）。
package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"okhuman/internal/textutil"
	"sync"
	"time"

	"okhuman/internal/types"
)

// FakeStep 脚本化步骤
type FakeStep struct {
	Type      string                 // text | tool | compress
	Text      string                 // text: 回复正文；compress: 总结（"" = 用默认模板）
	Reasoning *string                // 思考增量（可选）
	Name      string                 // tool: 工具名
	Args      map[string]interface{} // tool: 工具参数
	Scope     string                 // compress: history
	Deltas    []types.Delta          // compress: 流式增量（断言压缩事件流用；nil = 不流式）
}

// FakeLLM 确定性 LLM（实现 LlmClient 接口）
type FakeLLM struct {
	mu           sync.Mutex
	steps        []FakeStep
	Requests     [][]types.Message // 所有收到的请求（深拷贝，断言用）
	ChunkDelayMS int               // 流式增量间隔 ms（测试可调，默认 0 = 立即）
	PreDelayMS   int               // 首块前延迟 ms（模拟真实 LLM prefill/思考耗时，测试可调，默认 0）
}

// NewFakeLLM 新建
func NewFakeLLM(steps []FakeStep) *FakeLLM {
	return &FakeLLM{steps: steps, Requests: [][]types.Message{}}
}

// compressScope 末条消息的压缩 scope（非压缩请求 → "", false）。
// 解析在 types 里与生成方共用一份字面量（见 types.CompressMarkerPrefix）。
func (f *FakeLLM) compressScope(messages []types.Message) (string, bool) {
	if len(messages) == 0 {
		return "", false
	}
	s, ok := messages[len(messages)-1].Content.(string)
	if !ok {
		return "", false
	}
	return types.CompressScopeOf(s)
}

func fakeUsage() types.Usage {
	return types.Usage{PromptTokens: 10, CompletionTokens: 5, CachedTokens: 0}
}

// newFakeID 工具调用 id（fake_<6 位随机>，对应 TS Math.random().toString(36).slice(2, 8)）
func newFakeID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "fake_" + hex.EncodeToString(b)
}

func deepCopyMessages(ms []types.Message) []types.Message {
	b, _ := json.Marshal(ms)
	var out []types.Message
	_ = json.Unmarshal(b, &out)
	if out == nil {
		out = []types.Message{}
	}
	return out
}

// nextStep 消费一个主循环步骤（耗尽 → 交一个纯文本步，不带 tool_calls）。
// 为什么这样就能收笔：run 循环是"模型不再要工具就结束"，纯文本步 → 本轮自然结束。
// 旧的"必须带停止指令"要求（2026-10-17）已随停止指令机制同日整体移除，那段
// "无限追问 + OOM"的推理不再成立；夹具少给步骤现在表现为断言失败，而不是挂死。
func (f *FakeLLM) nextStep() FakeStep {
	if len(f.steps) == 0 {
		return FakeStep{Type: "text", Text: "（FakeLLM 步骤耗尽）"}
	}
	s := f.steps[0]
	f.steps = f.steps[1:]
	return s
}

// buildResponse 步骤 → 聚合响应
func (f *FakeLLM) buildResponse(step FakeStep) *types.Response {
	switch step.Type {
	case "text":
		return &types.Response{Content: step.Text, ReasoningContent: step.Reasoning, FinishReason: types.StringPtr("stop"), Usage: fakeUsage()}
	case "tool":
		args, _ := json.Marshal(step.Args)
		return &types.Response{
			Content:          "",
			ReasoningContent: step.Reasoning,
			FinishReason:     types.StringPtr("tool_calls"),
			ToolCalls: []types.ToolCall{
				{ID: newFakeID(), Type: "function", Function: types.Function{Name: step.Name, Arguments: string(args)}},
			},
			Usage: fakeUsage(),
		}
	default:
		return &types.Response{Content: "（FakeLLM compress 步骤被主循环消费）", FinishReason: types.StringPtr("stop"), Usage: fakeUsage()}
	}
}

// compressResponse 压缩请求响应（按 scope 取步骤；无 scope 匹配时退回任意 compress 步骤）
func (f *FakeLLM) compressResponse(scope string) *types.Response {
	step, ok := findCompressStep(f.steps, scope)
	text := fmt.Sprintf("[压缩:%s] FakeLLM 压缩总结（保留关键信息，长度可控）", scope)
	if ok && step.Text != "" {
		text = step.Text
	}
	return &types.Response{Content: text, FinishReason: types.StringPtr("stop"), Usage: fakeUsage()}
}

func findCompressStep(steps []FakeStep, scope string) (FakeStep, bool) {
	for _, s := range steps {
		if s.Type == "compress" && s.Scope == scope {
			return s, true
		}
	}
	for _, s := range steps {
		if s.Type == "compress" {
			return s, true
		}
	}
	return FakeStep{}, false
}

// sleepCtx 带 ctx 检查的睡眠（cancel → 立即返回 ctx.Err()，模拟客户端断连）
func sleepCtx(ctx context.Context, ms int) error {
	if ms <= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Complete 是 fake 自己的非流式出口：压缩请求统一走这里（Requests 只记一次），
// 也有测试直接调。它**不是 LlmClient 契约的一部分**（契约只有 CompleteStream）。
func (f *FakeLLM) Complete(ctx context.Context, messages []types.Message, tools []types.ToolSpec) (*types.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, deepCopyMessages(messages))
	if scope, ok := f.compressScope(messages); ok {
		return f.compressResponse(scope), nil
	}
	return f.buildResponse(f.nextStep()), nil
}

// CompleteStream 流式：按 4 字符一块发 reasoning → content 增量（模拟 llama.cpp 行为），
// 返回与 Complete 一致的聚合响应。ctx cancel → 立即抛错（模拟客户端断连）。
func (f *FakeLLM) CompleteStream(ctx context.Context, messages []types.Message, tools []types.ToolSpec, onDelta func(types.Delta), idleMs int) (*types.Response, error) {
	// 压缩请求委托给 Complete（保持既有 FakeLLM 压缩行为 + 单次记录）。
	// 判断放在 Requests 记录之前：压缩请求只由 Complete 记一次，避免 CompleteStream
	// 与 Complete 各记一次 → 破坏 Requests 长度断言（2026-09-01，压缩改流式后）。
	if scope, ok := f.compressScope(messages); ok {
		// 压缩请求也走流式：按脚本吐增量（OnDelta → 压缩事件流），响应仍由 Complete 出
		// （Requests 仍只记一次，既有断言不破）。
		if step, ok2 := findCompressStep(f.steps, scope); ok2 && onDelta != nil {
			for _, d := range step.Deltas {
				onDelta(d)
			}
		}
		return f.Complete(ctx, messages, tools)
	}
	f.mu.Lock()
	f.Requests = append(f.Requests, deepCopyMessages(messages))
	step := f.nextStep()
	f.mu.Unlock()

	if err := sleepCtx(ctx, f.PreDelayMS); err != nil { // prefill 模拟（测试用）
		return nil, err
	}
	think := ""
	if step.Reasoning != nil {
		think = *step.Reasoning
	}
	text := ""
	if step.Type == "text" {
		text = step.Text
	}
	// 逐块（4 字符）增量，块间 sleep ChunkDelayMS（模拟生成耗时，/stop 测试用）
	emit := func(s string, asText bool) error {
		// 按 UTF-16 code units 切 4 单元一块（全系统统一口径；按 rune 切时 emoji 只算 1，
		// 块长与真实客户端看到的长度不一致）。代理对不会被切断。
		for _, chunk := range textutil.UTF16Chunks(s, 4) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if asText {
				onDelta(types.Delta{Text: chunk})
			} else {
				onDelta(types.Delta{Thinking: chunk})
			}
			if err := sleepCtx(ctx, f.ChunkDelayMS); err != nil {
				return err
			}
		}
		return nil
	}
	if think != "" {
		if err := emit(think, false); err != nil {
			return nil, err
		}
	}
	if text != "" {
		if err := emit(text, true); err != nil {
			return nil, err
		}
	}
	return f.buildResponse(step), nil
}
