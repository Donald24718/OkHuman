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

// TestTruncationRetry 异常截断检测（2026-09-15 移植自 TS bf7090b）：
// 响应有 thinking 但无正文也无工具调用 → emit trunc_warn + 注入提示重试一次，
// 重试正常 → 本轮正常结束（重试后的正文为回复）。
func TestTruncationRetry(t *testing.T) {
	thinking := "思考了很久但忘了写正文……"
	a, _ := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "", Reasoning: &thinking}, // 第一次：异常截断
		{Type: "text", Text: "重试成功"},                   // 重试：正常回复
	})
	var warns []string
	opt := testRunOpt(&warns)
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if !strings.Contains(reply, "重试成功") {
		t.Errorf("回复应为重试结果，实际: %q", reply)
	}
	if len(warns) != 1 {
		t.Errorf("应恰好 1 条 trunc_warn，实际 %d 条: %v", len(warns), warns)
	}
}

// TestTruncationGiveUp 重试后仍截断 → 第 2 条 trunc_warn（放弃事件）后原样收尾（空正文）。
func TestTruncationGiveUp(t *testing.T) {
	thinking := "思考了很久但忘了写正文……"
	a, _ := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "", Reasoning: &thinking},
		{Type: "text", Text: "", Reasoning: &thinking}, // 重试仍截断
	})
	var warns []string
	opt := testRunOpt(&warns)
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if reply != "" {
		t.Errorf("放弃轮回复应为空，实际: %q", reply)
	}
	if len(warns) != 2 {
		t.Errorf("应有 2 条 trunc_warn，实际 %d 条: %v", len(warns), warns)
	}
}

// TestEndCheckContinue 轮末校验-继续干活（2026-09-26）：模型过早发停止信号
// （无工具调用）→ 注入校验提示 → 模型发工具调用 → 继续干活 → 再停止 → 再校验 →
// 模型仍停 → 正式收笔。断言：两次停止各校验一次、工具确实执行、回复取校验前正文
// （而非校验应答）、校验提示确实注入进上下文（第 2/4 次请求末条消息）。
func TestEndCheckContinue(t *testing.T) {
	a, fake := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "我先说完这句话"}, // 第一次"停止"（过早）
		{Type: "tool", Name: "bash", Args: map[string]interface{}{"command": "echo hi"}},
		{Type: "text", Text: "活干完了，这是最终答案"}, // 第二次"停止"（真做完）
		{Type: "text", Text: "确认没有未完成事项"},   // 校验回复（收笔，不当回复）
	})
	var checks int
	opt := testRunOptEndCheck(&checks)
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if reply != "活干完了，这是最终答案" {
		t.Errorf("回复应取校验前正文，实际: %q", reply)
	}
	if checks != 2 {
		t.Errorf("两次停止应各校验一次（2 条 end_check 事件），实际 %d 条", checks)
	}
	if len(fake.Requests) != 4 {
		t.Errorf("应共 4 次 LLM 请求（2 停止 + 2 校验），实际 %d 次", len(fake.Requests))
	}
	// 校验提示注入断言：第 2 次（index 1）与第 4 次（index 3）请求末条消息 = endCheckMsg
	for _, i := range []int{1, 3} {
		req := fake.Requests[i]
		last := req[len(req)-1]
		s, ok := last.Content.(string)
		if !ok || s != endCheckMsg {
			t.Errorf("第 %d 次请求末条消息应为校验提示，实际 role=%s content=%q", i+1, last.Role, s)
		}
	}
}

// TestEndCheckOff 轮末校验关（end_check=false）→ 旧行为：无工具调用立即收笔，
// 不注入校验提示、不多发 LLM 请求（回归保护）。
func TestEndCheckOff(t *testing.T) {
	a, fake := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "直接收笔"},
		{Type: "text", Text: "不应被消费"},
	})
	var checks int
	opt := testRunOptEndCheck(&checks)
	opt.EndCheck = false
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if reply != "直接收笔" {
		t.Errorf("回复应为第一次文本，实际: %q", reply)
	}
	if checks != 0 {
		t.Errorf("end_check 关不应有校验事件，实际 %d 条", checks)
	}
	if len(fake.Requests) != 1 {
		t.Errorf("应只有 1 次 LLM 请求，实际 %d 次", len(fake.Requests))
	}
}

func testRunOpt(warns *[]string) RunOptions {
	return RunOptions{
		DataDir:       "/tmp/okhuman-agent-test",
		ResultLimit:   50000,
		DoomWarnAfter: 3,
		FgTimeoutMS:   30000,
		Kind:          "user",
		EndCheck:      true, // 默认开（与出厂配置一致）；个别测试显式关
		OnEvent: func(e types.AgentEvent) {
			if e["type"] == "trunc_warn" {
				if d, ok := e["detail"].(string); ok {
					*warns = append(*warns, d)
				}
			}
		},
	}
}

// testRunOptEndCheck 带 end_check 事件计数的 RunOptions（EndCheck 开）
func testRunOptEndCheck(checks *int) RunOptions {
	opt := testRunOpt(&[]string{})
	opt.EndCheck = true
	orig := opt.OnEvent
	opt.OnEvent = func(e types.AgentEvent) {
		if e["type"] == "end_check" {
			*checks++
		}
		orig(e)
	}
	return opt
}

// failOnceClient 第一次 Complete/CompleteStream 返回 400（模拟坏图片 part 被
// LLM 拒载），之后透传内部 FakeLLM（2026-09-19 4xx 阶梯回写回归用）。
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

	var warns []string
	reply, err := a.Run(context.Background(), "你好", testRunOpt(&warns))
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
