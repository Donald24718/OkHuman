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
	"regexp"
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

var fakeCompressScopeRe = regexp.MustCompile(`^\s*\[压缩:(single|t1|t2|history)\]`)

// compressScope 末条消息的压缩 scope（非压缩请求 → "", false）
// 压缩请求特征：末尾消息以 [压缩:scope] 开头（与主循环 tools 一致，2026-08-30 改）。
func (f *FakeLLM) compressScope(messages []types.Message) (string, bool) {
	if len(messages) == 0 {
		return "", false
	}
	last := messages[len(messages)-1]
	s, ok := last.Content.(string)
	if !ok {
		return "", false
	}
	m := fakeCompressScopeRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
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

// nextStep 消费一个主循环步骤（耗尽 → 占位文本步）
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

// Complete 非流式（压缩请求走压缩路径；主循环按序消费步骤）
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
	if _, ok := f.compressScope(messages); ok {
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
		runes := []rune(s) // 按字符（rune）切块，对齐 TS slice 的 UTF-16 口径
		for p := 0; p < len(runes); p += 4 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			end := p + 4
			if end > len(runes) {
				end = len(runes)
			}
			chunk := string(runes[p:end])
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
