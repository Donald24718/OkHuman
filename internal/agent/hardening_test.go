package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/inject"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

// 本文件守护 agent.go 的一批健壮性修复（2026-10-05）：
//   A. 强停必须给本批【剩余】tool_calls 补合成 tool 消息（否则 assistant.tool_calls 失配）
//   B. 4xx 降级回写侧车的范围 = Compose 实际降级范围（不能再自行重算）
//   C. llmCall 任意返回路径都要释放 callCancel
//   D. 阶梯重试前重置 partial + 发 delta_reset（否则 UI 残留被丢弃的半截首流）
//   E. 强停收尾若模型仍返回空正文 → 占位文案（不能给前端空回复）
//   F. 前台超时通知的秒数取一位小数（向下取整，不夸大）
//   G. lastCalls 滑窗（无界增长 + args 可能很大）
//   H. 工具轮次上限兜底（doom 检测只认"完全相同"，模型改个参数就能绕开）
//   I. isLlm4xxErr / finishRunError 用 errors.As（抗 %w 包装）
//   J. finishStopped 的返回值 == 实际写入会话的文本
//
// 每条修复都配了变异验证：把修复改回去，对应用例必须变红。

// ---------- 测试替身 ----------

// scriptStep 一次 streamOnce 的行为：先发若干增量（可选阻塞等待 ctx 取消），
// 再返回 Resp 或 Err。
type scriptStep struct {
	Deltas []types.Delta
	Resp   *types.Response
	Err    error
	// Block=true：发完增量后阻塞到 ctx 取消（模拟生成中的流被 /stop 打断）
	Block bool
}

// stepClient 按脚本序列应答的 LLM 客户端（并发安全：多个 agent 协程可能重入）
type stepClient struct {
	mu      sync.Mutex
	steps   []scriptStep
	i       int
	Fn      func(call int) scriptStep // 优先级高于 steps（无限序列用）
	Started chan struct{}             // 首次进入流式调用时关闭（供 stop 测试同步）
	lastCtx context.Context           // 最近一次收到的 ctx（断言 cancel 是否真被调用）
}

func (c *stepClient) next() scriptStep {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fn != nil {
		c.i++
		return c.Fn(c.i)
	}
	if len(c.steps) == 0 {
		return scriptStep{Resp: textResp("（stepClient 脚本耗尽）")}
	}
	s := c.steps[0]
	c.steps = c.steps[1:]
	return s
}

func (c *stepClient) notifyStarted() {
	if c.Started == nil {
		return
	}
	select {
	case <-c.Started: // 已关闭过，不重复关
	default:
		close(c.Started)
	}
}

func (c *stepClient) remember(ctx context.Context) {
	c.mu.Lock()
	c.lastCtx = ctx
	c.mu.Unlock()
}

func (c *stepClient) LastCtx() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastCtx
}

func (c *stepClient) Complete(ctx context.Context, msgs []types.Message, tools []types.ToolSpec) (*types.Response, error) {
	c.remember(ctx)
	return textResp("[压缩:history] 测试压缩总结"), nil
}

func (c *stepClient) CompleteStream(ctx context.Context, msgs []types.Message, tools []types.ToolSpec, onDelta func(types.Delta), idleMs int) (*types.Response, error) {
	c.remember(ctx)
	c.notifyStarted()
	s := c.next()
	for _, d := range s.Deltas {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			if onDelta != nil {
				onDelta(d)
			}
		}
	}
	if s.Block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s.Err != nil {
		return nil, s.Err
	}
	return s.Resp, nil
}

func textResp(s string) *types.Response {
	return &types.Response{Content: s, FinishReason: types.StringPtr("stop")}
}

// toolResp 构造带 n 个并行工具调用的响应（id 确定性：便于断言配对）
func toolResp(n int, name string, args map[string]interface{}) *types.Response {
	b, _ := json.Marshal(args)
	calls := make([]types.ToolCall, 0, n)
	for i := 0; i < n; i++ {
		calls = append(calls, types.ToolCall{
			ID:       fmt.Sprintf("call_%s_%d", name, i),
			Type:     "function",
			Function: types.Function{Name: name, Arguments: string(b)},
		})
	}
	return &types.Response{FinishReason: types.StringPtr("tool_calls"), ToolCalls: calls}
}

func newAgent(client llm.LlmClient, injectDir string) *Agent {
	cm := ctxmgr.NewManager(client, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示", filepath.Join(os.TempDir(), "okhuman-agent-hardening"))
	return New(client, cm, injectDir)
}

// eventRecorder 记录事件序列（类型 + 少量关键字段）
type eventRecorder struct {
	types []string
}

func (r *eventRecorder) record(e types.AgentEvent) {
	r.types = append(r.types, fmt.Sprintf("%v", e["type"]))
}

func (r *eventRecorder) count(t string) int {
	n := 0
	for _, x := range r.types {
		if x == t {
			n++
		}
	}
	return n
}

func hardeningOpt() RunOptions {
	return RunOptions{
		DataDir:       filepath.Join(os.TempDir(), "okhuman-agent-hardening"),
		ResultLimit:   50000,
		DoomWarnAfter: 3,
		FgTimeoutMS:   30000,
		Kind:          "user",
	}
}

// assertToolCallsPaired 不变式：每条 assistant(tool_calls) 之后必须紧跟为其
// 每个 call id 补齐的 tool 消息（中间不得插入非 tool 角色）。
// 违反会让严格校验的 OpenAI 兼容端点直接 400。
func assertToolCallsPaired(t *testing.T, sess *types.SessionState) {
	t.Helper()
	pending := map[string]bool{}
	has := false
	for _, m := range sess.Messages {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				if has && len(pending) > 0 {
					t.Errorf("❌ assistant.tool_calls 之后 tool 消息不完整：缺 %v", keysOf(pending))
				}
				pending = map[string]bool{}
				has = true
				for _, tc := range m.ToolCalls {
					pending[tc.ID] = true
				}
			}
		case "tool":
			if m.ToolCallID != nil {
				if !pending[*m.ToolCallID] {
					t.Errorf("❌ 出现孤儿 tool 消息（无对应 tool_call）: %s", *m.ToolCallID)
				}
				delete(pending, *m.ToolCallID)
			}
		default:
			if has && len(pending) > 0 {
				t.Errorf("❌ assistant.tool_calls 之后在未补齐 tool 消息前插入了 %s 消息：缺 %v", m.Role, keysOf(pending))
			}
			has = false
			pending = map[string]bool{}
		}
	}
	if len(pending) > 0 {
		t.Errorf("❌ 会话末尾残留无配对 tool_call_id: %v", keysOf(pending))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------- A. 强停补齐 tool 消息（守护协议配对）----------

func TestDoomStopPairsAllToolResults(t *testing.T) {
	same := map[string]interface{}{"command": "echo hi"}
	cli := &stepClient{steps: []scriptStep{
		{Resp: toolResp(3, "bash", same)}, // 第 1 批：3 个相同 → 触发 doom_warn
		{Resp: toolResp(3, "bash", same)}, // 第 2 批：第 1 个就 warned&&sameRun → 强停
		{Resp: textResp("（收尾回答）")},
	}}
	a := newAgent(cli, "")
	var evs eventRecorder
	opt := hardeningOpt()
	opt.OnEvent = evs.record

	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if reply != "（收尾回答）" {
		t.Errorf("reply = %q", reply)
	}
	assertToolCallsPaired(t, a.cm.Session())
	if n := evs.count("doom_stop"); n != 1 {
		t.Errorf("应恰好 1 条 doom_stop，实际 %d", n)
	}
	// 第 2 批被强停的调用没有真正执行工具（tool_call 事件只应来自第 1 批）
	if n := evs.count("tool_call"); n != 3 {
		t.Errorf("tool_call 事件应只有第 1 批的 3 条，实际 %d", n)
	}
}

// ---------- B. 降级回写范围 == Compose 实际降级范围 ----------

func TestInjectFallbackPersistsOnlyDemoted(t *testing.T) {
	base := t.TempDir()
	injectDir := filepath.Join(base, "inject")
	if err := os.MkdirAll(injectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const oldID, newID = "inj-old", "inj-new"
	png := `[{"type":"image_url","image_url":{"url":"data:image/png;base64,dGVzdAo="}}]`
	for _, id := range []string{oldID, newID} {
		if err := os.WriteFile(filepath.Join(injectDir, id+".json"), []byte(png), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := inject.NewResolver(injectDir, func(id, reason string) {})
	ctxmgr.ConfigureInjectResolver(res)
	t.Cleanup(func() { ctxmgr.ConfigureInjectResolver(inject.NewResolver(t.TempDir(), nil)) })

	inner := llm.NewFakeLLM([]llm.FakeStep{{Type: "text", Text: "降级后正常"}})
	a := newAgent(&failOnceClient{inner: inner, fails: 1}, injectDir)
	a.cm.AddMessage(&types.RawEntry{Role: "user", Content: []types.ContentPart{
		types.TextPart("[okattach] 附件 " + oldID), types.RefPart(oldID),
	}})
	a.cm.AddMessage(&types.RawEntry{Role: "user", Content: []types.ContentPart{
		types.TextPart("[okattach] 附件 " + newID), types.RefPart(newID),
	}})

	if _, err := a.Run(context.Background(), "你好", hardeningOpt()); err != nil {
		t.Fatalf("run 失败: %v", err)
	}

	newSide, err := os.ReadFile(filepath.Join(injectDir, newID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(newSide), "降级为文本提示") {
		t.Errorf("最近一条的附件应被降级回写，实际: %s", newSide)
	}
	oldSide, err := os.ReadFile(filepath.Join(injectDir, oldID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(oldSide), "image_url") {
		t.Errorf("❌ 更早的附件不该被降级（demote-newest 只应覆盖最近一条），实际被改写为: %s", oldSide)
	}
}

// ---------- C. llmCall 任意路径都要释放 callCancel ----------

// TestCallCancelReleasedOnAllPaths 直接调 llmCall（父 ctx 用 Background，排除
// "轮末 runCancel 顺带取消掉子 ctx"带来的假绿），断言两件事：
//   1. 交给客户端的那个 callCtx 确实已被 cancel（不只是字段被清）
//   2. a.callCancel 已归零（/stop 不会拿着过期 cancel 去调）
// 变异验证：删掉 llmCall 里的 `defer callCancel()` → 本例必须变红。
func TestCallCancelReleasedOnAllPaths(t *testing.T) {
	cases := []struct {
		name  string
		steps []scriptStep
		fn    func(int) scriptStep
	}{
		{name: "成功", steps: []scriptStep{{Resp: textResp("ok")}}},
		{name: "非4xx错误", steps: []scriptStep{{Err: errors.New("网络炸了")}}},
		{name: "阶梯耗尽", fn: func(int) scriptStep {
			return scriptStep{Err: &llm.HTTPError{Status: 400, Body: "request too large"}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := &stepClient{steps: c.steps, Fn: c.fn}
			a := newAgent(cli, "")
			_, _ = a.llmCall(context.Background(), func(types.AgentEvent) {}, &TokenTotals{})

			if ctx0 := cli.LastCtx(); ctx0 != nil && ctx0.Err() == nil {
				t.Errorf("❌ llmCall 返回后没调用 cancel：该路径泄漏了 context.WithCancel 的子节点")
			}
			a.mu.Lock()
			leaked := a.callCancel
			a.mu.Unlock()
			if leaked != nil {
				t.Errorf("❌ llmCall 返回后 a.callCancel 未清理")
			}
		})
	}
}

// ---------- D. 重试前重置 partial + 发 delta_reset ----------

func TestPartialResetBeforeLadderRetry(t *testing.T) {
	var got []string
	cli := &stepClient{steps: []scriptStep{
		// 首次：先流出半截正文再 400（服务端在 SSE 中途回错误）
		{Deltas: []types.Delta{{Text: "A"}, {Text: "B"}},
			Err: &llm.HTTPError{Status: 400, Body: "request too large"}},
		{Deltas: []types.Delta{{Text: "X"}, {Text: "Y"}}, Resp: textResp("XY")},
	}}
	a := newAgent(cli, "")
	opt := hardeningOpt()
	opt.OnEvent = func(e types.AgentEvent) {
		switch e["type"] {
		case "delta":
			if s, ok := e["text"].(string); ok {
				got = append(got, s)
			}
		case "delta_reset":
			got = append(got, "<reset>")
		}
	}
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if reply != "XY" {
		t.Errorf("reply = %q, want XY", reply)
	}
	want := []string{"A", "B", "<reset>", "X", "Y"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("❌ 增量序列异常：%v，want %v（重置前必须发 delta_reset，否则 UI 残留被丢弃的半截内容）", got, want)
	}
}

// ---------- E. 强停收尾空正文 → 占位文案 ----------

func TestStrongStopEmptyReplyFallsBackToPlaceholder(t *testing.T) {
	same := map[string]interface{}{"command": "echo hi"}
	cli := &stepClient{steps: []scriptStep{
		{Resp: toolResp(3, "bash", same)},
		{Resp: toolResp(3, "bash", same)},
		// 强停后模型"仍然只给工具调用、没有正文"——历史实现会给前端一条空回复
		{Resp: &types.Response{FinishReason: types.StringPtr("tool_calls"),
			ToolCalls: toolResp(1, "bash", map[string]interface{}{"command": "echo again"}).ToolCalls}},
	}}
	a := newAgent(cli, "")
	var evs eventRecorder
	opt := hardeningOpt()
	opt.OnEvent = evs.record

	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if strings.TrimSpace(reply) == "" {
		t.Errorf("❌ 强停收尾不应返回空回复（前端会显示空气泡）")
	}
	assertToolCallsPaired(t, a.cm.Session())
	if evs.count("doom_tail_pending") != 1 {
		t.Errorf("模型强停后仍返回 tool_calls 时，应发 1 条 doom_tail_pending 事件，实际 %d", evs.count("doom_tail_pending"))
	}
}

// ---------- F. 前台超时通知的秒数（一位小数，向下取整）----------

func TestFgTimeoutNoticeSeconds(t *testing.T) {
	cases := []struct{ ms int; want string }{
		{999, "0.9"}, {1000, "1.0"}, {1500, "1.5"}, {1900, "1.9"}, {1999, "1.9"}, {30000, "30.0"},
	}
	for _, c := range cases {
		got := fgTimeoutNotice(c.ms)
		if !strings.Contains(got, "超过 "+c.want+" 秒") {
			t.Errorf("fgTimeoutNotice(%d) = %q，want 含 %q", c.ms, got, "超过 "+c.want+" 秒")
		}
	}
}

// ---------- G. lastCalls 滑窗 ----------

func TestLastCallsWindowBounded(t *testing.T) {
	// 一条响应里 12 个参数互不相同的调用 → 不触发 doom，但会向 lastCalls 追加 12 条
	args := map[string]interface{}{"command": strings.Repeat("x", 2000)}
	calls := make([]types.ToolCall, 0, 12)
	for i := 0; i < 12; i++ {
		b, _ := json.Marshal(map[string]interface{}{"command": fmt.Sprintf("echo %d", i)})
		calls = append(calls, types.ToolCall{ID: fmt.Sprintf("c%d", i), Type: "function",
			Function: types.Function{Name: "bash", Arguments: string(b)}})
	}
	_ = args
	cli := &stepClient{steps: []scriptStep{
		{Resp: &types.Response{FinishReason: types.StringPtr("tool_calls"), ToolCalls: calls}},
		{Resp: textResp("done")},
	}}
	a := newAgent(cli, "")
	opt := hardeningOpt()
	opt.DoomWarnAfter = 3
	if _, err := a.Run(context.Background(), "你好", opt); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	a.mu.Lock()
	n := len(a.lastCalls)
	a.mu.Unlock()
	if n > opt.DoomWarnAfter+1 {
		t.Errorf("❌ lastCalls 应有界（≤ DoomWarnAfter+1=%d），实际 %d 条", opt.DoomWarnAfter+1, n)
	}
}

// ---------- H. 轮次上限兜底（doom 检测兜不住"每次改一点参数"）----------

func TestToolRoundLimitStopsRunawayLoop(t *testing.T) {
	step := 0
	// 客户端在第 21 轮才给正文——这样即使 runLoop 没有轮次上限，最多也就跑到 20 轮，
	// 不会把测试卡成超时（上限失效时能观测到"执行了 20 次工具"而非挂死）。
	cli := &stepClient{Fn: func(int) scriptStep {
		step++
		if step > 20 {
			return scriptStep{Resp: textResp("自然结束")}
		}
		// 每轮参数都不同（时间戳/行号式微调）→ §8 doom 检测永远不成立
		return scriptStep{Resp: toolResp(1, "bash", map[string]interface{}{"command": fmt.Sprintf("echo %d", step)})}
	}}
	a := newAgent(cli, "")
	var evs eventRecorder
	opt := hardeningOpt()
	opt.MaxToolRounds = 3
	opt.OnEvent = evs.record

	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if strings.TrimSpace(reply) == "" {
		t.Errorf("❌ 触达轮次上限后应有兜底回复，实际为空")
	}
	if n := evs.count("tool_call"); n != opt.MaxToolRounds {
		t.Errorf("应恰好执行 %d 次工具调用后收尾，实际 %d", opt.MaxToolRounds, n)
	}
	if evs.count("round_limit") != 1 {
		t.Errorf("应发 1 条 round_limit 事件，实际 %d", evs.count("round_limit"))
	}
	assertToolCallsPaired(t, a.cm.Session())
}

// ---------- I. errors.As 抗 %w 包装 ----------

func TestIsLlm4xxErrUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("上层又包了一层: %w", &llm.HTTPError{Status: 413, Body: "too large"})
	if !isLlm4xxErr(wrapped) {
		t.Errorf("❌ isLlm4xxErr 应穿透 %%w 包装识别 4xx")
	}
	if isLlm4xxErr(fmt.Errorf("包一层: %w", &llm.HTTPError{Status: 500, Body: "boom"})) {
		t.Errorf("5xx 不应判为 4xx")
	}
	if isLlm4xxErr(errors.New("普通错误")) {
		t.Errorf("普通错误不应判为 4xx")
	}
}

// ---------- J. finishStopped 返回值 == 落会话的文本 ----------

func TestStoppedReplyMatchesStoredMessage(t *testing.T) {
	started := make(chan struct{})
	cli := &stepClient{Started: started, steps: []scriptStep{{Block: true}, {Resp: textResp("用不到")}}}
	a := newAgent(cli, "")
	go func() {
		<-started
		a.Stop() // 在任何增量流出前按 /stop → partial 为空
	}()
	reply, err := a.Run(context.Background(), "你好", hardeningOpt())
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if strings.TrimSpace(reply) == "" {
		t.Errorf("❌ 被 /stop 打断后 Run 返回空串，与落会话的占位文本不一致（HTTP /chat 的 reply 字段会拿到空值）")
	}
	sess := a.cm.Session()
	var last string
	for _, m := range sess.Messages {
		if m.Role == "assistant" {
			last = types.AsString(m.Content)
		}
	}
	if reply != last {
		t.Errorf("❌ 返回值 %q 与会话里存的 %q 不一致", reply, last)
	}
}
