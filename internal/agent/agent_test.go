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

// TestThinkingOnlyStopLoop 有 thinking、正文空、无 tool_calls（旧"异常截断"独立分支
// 的场景，2026-10-13 撤掉该分支）→ 由收笔判定接管：注入停止指令提示词重问，模型
// 补出正文并带停止指令才收笔。断言：追问 1 次、共 2 次 LLM 请求、回复为补出的正文、
// 且追问那次的上下文里已带着上一条的 reasoning_content（模型不必重想）。
func TestThinkingOnlyStopLoop(t *testing.T) {
	thinking := "思考了很久但忘了写正文……"
	a, fake := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "", Reasoning: &thinking}, // 只有思考：无正文无工具调用
		{Type: "text", Text: "补上正文 [[[[STOP]]]]"},
	})
	var prompts int
	reply, err := a.Run(context.Background(), "你好", testRunOptStop(&prompts))
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if prompts != 1 {
		t.Errorf("应由收笔判定追问 1 次，实际 %d 次", prompts)
	}
	if len(fake.Requests) != 2 {
		t.Errorf("应共 2 次 LLM 请求，实际 %d 次", len(fake.Requests))
	}
	if reply != "补上正文 [[[[STOP]]]]" {
		t.Errorf("回复应为补出的正文，实际: %q", reply)
	}
	var seen bool
	for _, m := range fake.Requests[1] {
		if m.Role == "assistant" && m.ReasoningContent != nil && *m.ReasoningContent == thinking {
			seen = true
		}
	}
	if !seen {
		t.Errorf("追问时上一条思考应已在上下文里（模型不必重想）")
	}
}

// TestStopLoopContinue 收笔判定-继续干活（2026-10-12，替换旧 end_check）：模型无
// 工具调用但正文没有停止指令 → 注入提示词 → 模型改发工具调用 → 继续干活 →
// 再结束时带上停止指令 → 正式收笔（回复原样保留指令，不剥）。断言：注入 1 次
// 提示词、工具确实执行、共 3 次请求、第 2 次请求末条 = stopPromptMsg、回复原样含指令。
func TestStopLoopContinue(t *testing.T) {
	a, fake := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "我先说完这句话"}, // 无停止指令 → 被追问
		{Type: "tool", Name: "bash", Args: map[string]interface{}{"command": "echo hi"}},
		{Type: "text", Text: "活干完了，这是最终答案 [[[[STOP]]]]"}, // 带指令 → 收笔
	})
	var prompts int
	opt := testRunOptStop(&prompts)
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if reply != "活干完了，这是最终答案 [[[[STOP]]]]" {
		t.Errorf("回复应原样含停止指令（不剥），实际: %q", reply)
	}
	if prompts != 1 {
		t.Errorf("应有 1 条 end_check 事件，实际 %d 条", prompts)
	}
	if len(fake.Requests) != 3 {
		t.Errorf("应共 3 次 LLM 请求（追问 + 工具后 + 收笔），实际 %d 次", len(fake.Requests))
	}
	req := fake.Requests[1]
	last := req[len(req)-1]
	s, ok := last.Content.(string)
	if !ok || s != stopPromptMsg {
		t.Errorf("第 2 次请求末条应为停止指令提示词，实际 role=%s content=%q", last.Role, s)
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
	opt := testRunOptStop(&checks)
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

func testRunOpt() RunOptions {
	return RunOptions{
		DataDir:       "/tmp/okhuman-agent-test",
		ResultLimit:   50000,
		DoomWarnAfter: 3,
		FgTimeoutMS:   30000,
		Kind:          "user",
		// 收笔判定默认关：旧行为（无工具调用即收笔）。停止指令是无上限循环，
		// 夹具步骤耗尽后 fake 会持续返回"步骤耗尽"文本 → 不显式开就会无限转。
		// 收笔判定专项测试用 testRunOptStop 显式开。
		EndCheck: false,
	}
}

// testRunOptStop 带 stop_prompt 事件计数的 RunOptions（收笔判定开）
func testRunOptStop(prompts *int) RunOptions {
	opt := testRunOpt()
	opt.EndCheck = true
	opt.OnEvent = func(e types.AgentEvent) {
		// end_check 事件追问与收笔都发 → 只数"重问"那类，断言才对应追问次数
		if e["type"] == "end_check" {
			if d, ok := e["detail"].(string); ok && strings.Contains(d, "重问") {
				*prompts++
			}
		}
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

// TestNoBackgroundGateStillAsks 闸门已撤（2026-10-12 用户定）：有未 settle 的
// 后台任务时也不再直接收笔——正文没有停止指令就照常追问。断言：追问 1 次、
// 2 次请求、不再有 end_check_skipped 事件。
func TestNoBackgroundGateStillAsks(t *testing.T) {
	a, fake := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "下载在后台跑，我收笔等通知"},
		{Type: "text", Text: "那我补上停止指令 [[[[STOP]]]]"},
		{Type: "text", Text: "不应被消费"},
	})
	var prompts, skips int
	opt := testRunOptStop(&prompts)
	orig := opt.OnEvent
	opt.OnEvent = func(e types.AgentEvent) {
		if e["type"] == "end_check_skipped" {
			skips++
		}
		orig(e)
	}
	if _, err := a.Run(context.Background(), "你好", opt); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if prompts != 1 {
		t.Errorf("撤闸门后应照常追问，实际 end_check %d 条", prompts)
	}
	if skips != 0 {
		t.Errorf("不该再有 end_check_skipped 事件，实际 %d 条", skips)
	}
	if len(fake.Requests) != 2 {
		t.Errorf("应 2 次请求，实际 %d 次", len(fake.Requests))
	}
}

// ---- 停止指令收笔协议（2026-10-12 用户定，替换旧 end_check）----

func TestIsStopSignalTable(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"末尾带指令", "答案在此 [[[[STOP]]]]", true},
		{"末尾带指令加尾随换行", "答案在此 [[[[STOP]]]]\n\n", true},
		{"正文只有指令", "[[[[STOP]]]]", true},
		{"指令在正文中间也算（2026-10-12 放宽）", "我先说 [[[[STOP]]]] 再讲别的", true},
		{"指令在开头也算", "[[[[STOP]]]] 后面还有话", true},
		{"无指令", "普通收笔", false},
		{"近似串不算", "答案 [[[[STOP]]", false},
		{"空正文不算", "", false},
	}
	for _, c := range cases {
		if got := isStopSignal(c.content); got != c.want {
			t.Errorf("%s: isStopSignal(%q)=%v，期望 %v", c.name, c.content, got, c.want)
		}
	}
}

// TestStopSignalInReasoningNotCounted 停止指令只认正文：思考内容里带指令、正文
// 没有 → 不算收笔，照常追问（2026-10-12 用户定）。
func TestStopSignalInReasoningNotCounted(t *testing.T) {
	reason := "我想收笔了 [[[[STOP]]]]"
	a, fake := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "正文里没有指令", Reasoning: &reason},
		{Type: "text", Text: "补上指令 [[[[STOP]]]]"},
	})
	var prompts int
	opt := testRunOptStop(&prompts)
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if prompts != 1 {
		t.Errorf("思考里的指令不该算收笔，实际 end_check %d 条", prompts)
	}
	if reply != "补上指令 [[[[STOP]]]]" {
		t.Errorf("回复应取补指令那轮正文，实际 %q", reply)
	}
	if len(fake.Requests) != 2 {
		t.Errorf("应 2 次请求，实际 %d 次", len(fake.Requests))
	}
}

// TestStopLoopNoLimitKeepsAsking 无上限（2026-10-12 用户定）：连续多轮不给停止
// 指令 → 每轮都注入提示词重问，直到给出为止。
func TestStopLoopNoLimitKeepsAsking(t *testing.T) {
	a, fake := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "第一次不给指令"},
		{Type: "text", Text: "第二次还不给"},
		{Type: "text", Text: "第三次仍不给"},
		{Type: "text", Text: "第四次给 [[[[STOP]]]]"},
	})
	var prompts int
	opt := testRunOptStop(&prompts)
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if prompts != 3 {
		t.Errorf("前三轮应各追问一次（共 3 次），实际 %d 次", prompts)
	}
	if len(fake.Requests) != 4 {
		t.Errorf("应 4 次请求，实际 %d 次", len(fake.Requests))
	}
	if reply != "第四次给 [[[[STOP]]]]" {
		t.Errorf("回复应取收笔轮正文（原样），实际 %q", reply)
	}
}

// TestStopLoopPiggyback 搭车（2026-10-12 修）：收笔判定追问期间入队的用户消息
// 必须在下一次 LLM 调用前注入进上下文——旧实现只在工具循环里取队列，导致
// 收笔判定期间用户插不进话。断言：追问后那次请求里出现【用户追加】消息。
func TestStopLoopPiggyback(t *testing.T) {
	a, fake := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "我要收笔"}, // 无指令 → 追问
		{Type: "text", Text: "收到追加，收笔 [[[[STOP]]]]"},
	})
	queue := []string{"再补一句：注意保留日志"}
	var prompts int
	opt := testRunOptStop(&prompts)
	opt.TakePendingUsers = func() []string {
		q := queue
		queue = nil
		return q
	}
	if _, err := a.Run(context.Background(), "你好", opt); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if prompts != 1 {
		t.Errorf("应追问 1 次，实际 %d 条 end_check", prompts)
	}
	req := fake.Requests[1]
	found := false
	for _, m := range req {
		if str, ok := m.Content.(string); ok && strings.Contains(str, "【用户追加】再补一句") {
			found = true
		}
	}
	if !found {
		t.Errorf("追问后的请求里应含搭车的用户追加消息，实际末三条: %v", len(req))
	}
}
