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
	return New(fake, "系统提示", cm, ""), fake // injectDir=""：既有测试不触注入回写
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

// TestInjectDemotePersist（2026-09-19）：4xx 降级成功 → 侧车回写文本提示（治本）。
// 旧行为（请求级降级，不动侧车）：坏 part 留在侧车，下轮请求原样恢复，
// 每轮都先 4xx 再降级。回写后侧车变提示，后续调用不再发坏 part。
func TestInjectDemotePersist(t *testing.T) {
	base := t.TempDir()
	injectDir := filepath.Join(base, "inject")
	if err := os.MkdirAll(injectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 坏附件侧车（假 PNG：base64 解码是 "test\n"，LLM 必拒载）
	bad := `[{"type":"image_url","image_url":{"url":"data:image/png;base64,dGVzdAo="}}]`
	if err := os.WriteFile(filepath.Join(injectDir, "inj-1.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	res := inject.NewResolver(injectDir, func(id, reason string) {})
	ctxmgr.ConfigureInjectResolver(res)
	t.Cleanup(func() { ctxmgr.ConfigureInjectResolver(inject.NewResolver(t.TempDir(), nil)) })

	inner := llm.NewFakeLLM([]llm.FakeStep{{Type: "text", Text: "降级后正常"}})
	cm := ctxmgr.NewManager(inner, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示", t.TempDir())
	// 会话里放一条带注入引用的消息（/inject 的标记消息）
	cm.AddMessage(&types.RawEntry{Role: "user", Content: []types.ContentPart{
		types.TextPart("[okattach] 附件 inj-1"),
		types.RefPart("inj-1"),
	}})
	a := New(&failOnceClient{inner: inner, fails: 1}, "系统提示", cm, injectDir)

	reply, err := a.Run(context.Background(), "你好", testRunOpt())
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if reply != "降级后正常" {
		t.Fatalf("回复 = %q，want 降级后正常", reply)
	}
	// 断言：侧车已被回写成文本提示（不再含 image_url）
	got, err := os.ReadFile(filepath.Join(injectDir, "inj-1.json"))
	if err != nil {
		t.Fatalf("侧车读取失败: %v", err)
	}
	if strings.Contains(string(got), "image_url") {
		t.Errorf("侧车应已回写为文本提示，仍含 image_url: %s", got)
	}
	if !strings.Contains(string(got), "降级为文本提示") {
		t.Errorf("侧车应含降级提示文案: %s", got)
	}
	// 断言：解析器缓存已清（重新 resolve 拿到提示而非坏 part）
	parts := res.Resolve("inj-1")
	if len(parts) != 1 || parts[0].Type != "text" {
		t.Errorf("resolve 应返回单条文本提示，实际 %+v", parts)
	}
}
