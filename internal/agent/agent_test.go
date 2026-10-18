package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/inject"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

// newTestAgent 建带 FakeLLM 的 agent（压缩阈值拉满，测试内不触发压缩）
func newTestAgent(steps []llm.FakeStep) (*Agent, *llm.FakeLLM) {
	fake := llm.NewFakeLLM(steps)
	cm := ctxmgr.NewManager(fake, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示", t0TempDir())
	return New(fake, "系统提示", cm), fake
}

func t0TempDir() string { return "/tmp/okhuman-agent-test" }
func testRunOpt() RunOptions {
	return RunOptions{
		DataDir:       "/tmp/okhuman-agent-test",
		ResultLimit:   50000,
		DoomWarnAfter: 3,
		FgTimeoutMS:   30000,
		Kind:          "user",
		// 收笔判定默认关：旧行为（无工具调用即收笔）。开启后正文必须含停止指令，
		// 多数既有夹具的第一步不带指令 → 会多一轮追问，请求数断言全得重算。
		// 收笔判定专项测试用 testRunOptStop 显式开（fake 步骤耗尽会带停止指令收笔，
		// 不会无限追问，见 llm/fake.go nextStep）。
	}
}

type failOnceClient struct {
	inner llm.LlmClient
	fails int
}

func (c *failOnceClient) failIfDue() error {
	if c.fails > 0 {
		c.fails--
		return &llm.HTTPError{Status: 400, Body: "Failed to load image or audio file"}
	}
	return nil
}

func (c *failOnceClient) Complete(ctx context.Context, messages []types.Message, tools []types.ToolSpec) (*types.Response, error) {
	if e := c.failIfDue(); e != nil {
		return nil, e
	}
	return c.inner.Complete(ctx, messages, tools)
}

func (c *failOnceClient) CompleteStream(ctx context.Context, messages []types.Message, tools []types.ToolSpec, onDelta func(types.Delta), idleMs int) (*types.Response, error) {
	if e := c.failIfDue(); e != nil {
		return nil, e
	}
	return c.inner.CompleteStream(ctx, messages, tools, onDelta, idleMs)
}

// TestLLM4xxPropagates（2026-10-18）：三级回退阶梯已删——LLM 回 4xx 原样抛出、
// 本轮终止。机器不再猜"是不是附件坏了"，也不再自动回写侧车；附件剥离改由人显式
// 按按钮（POST /strip-injects，见 server/manual_actions_test.go）。
func TestLLM4xxPropagates(t *testing.T) {
	base := t.TempDir()
	injectDir := filepath.Join(base, "inject")
	if err := os.MkdirAll(injectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := `[{"type":"image_url","image_url":{"url":"data:image/png;base64,dGVzdAo="}}]`
	if err := os.WriteFile(filepath.Join(injectDir, "inj-1.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	res := inject.NewResolver(injectDir, func(id, reason string) {})
	ctxmgr.ConfigureInjectResolver(res)
	t.Cleanup(func() { ctxmgr.ConfigureInjectResolver(inject.NewResolver(t.TempDir(), nil)) })

	inner := llm.NewFakeLLM([]llm.FakeStep{{Type: "text", Text: "这步不该被消费"}})
	cm := ctxmgr.NewManager(inner, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示", t.TempDir())
	cm.AddMessage(&types.RawEntry{Role: "user", Content: []types.ContentPart{
		types.TextPart("[okattach] 附件 inj-1"),
		types.RefPart("inj-1"),
	}})
	a := New(&failOnceClient{inner: inner, fails: 1}, "系统提示", cm)

	_, err := a.Run(context.Background(), "你好", testRunOpt())
	if err == nil {
		t.Fatal("LLM 4xx 应原样抛出（阶梯已删，没有兜底重试）")
	}
	if !strings.Contains(err.Error(), "Failed to load image") {
		t.Fatalf("错误内容不对: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(injectDir, "inj-1.json"))
	if err != nil {
		t.Fatalf("侧车读取失败: %v", err)
	}
	if !strings.Contains(string(got), "image_url") {
		t.Errorf("侧车不该被自动改写（剥离只应由人触发）: %s", got)
	}
}
