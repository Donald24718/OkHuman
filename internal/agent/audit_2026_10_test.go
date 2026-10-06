package agent

// 本文件守护 2026-10-06 代码审查（报告 16 条）中确认成立、并已修复的问题。
// 每条都要求"把修复改回去、对应用例必须变红"（变异可捕获），不是走过场的冒烟。
//
//   K. DoomWarnAfter 0/负数 → isSameRunLocked panic（入口归一化 + 函数内防御）
//   L. 4xx 阶梯第三级：必须先 ForceHardTruncate 再 Compose（旧实现顺序反了 → 兜底无效）
//   M. 空响应/放弃轮不得给前端空回复
//   N. doom 警告的工具名取【触发那一刻】的名字（A,A,A,B 不能说成 B），
//      且刚发出警告的那一批不得立刻把 warned 复位（否则永远强停不了）
//   O. run_end 必须在拆监听/取消 runCtx 之后（run_end 之后不再有本轮压缩事件）
//   P. 轮内搭车按 System 字段区分系统通知与用户追加（不再靠文本前缀）
//   Q. RunOptions 入口归一化
//   R. 后台任务 StartedAt 取在工具启动前（DurationMs 不再少算前台等待）

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

// ---------- K. DoomWarnAfter 0/负数 ----------

// TestIsSameRunLockedRejectsNonPositiveN 判定函数自身不接受 n<=0：
// n=0 → tail 是空切片，取 tail[0] 必然 panic；n<0 → 下标越界。
// 变异验证：把 `n <= 0 ||` 删掉 → n<=0 两个子用例 panic（测试失败）。
func TestIsSameRunLockedRejectsNonPositiveN(t *testing.T) {
	a := newAgent(&stepClient{}, "")
	a.lastCalls = []callRec{{Name: "bash", Key: "k"}, {Name: "bash", Key: "k"}}
	if a.isSameRunLocked(0) {
		t.Errorf("❌ n=0 应判为「不是完全相同」（旧实现会 panic）")
	}
	if a.isSameRunLocked(-1) {
		t.Errorf("❌ n<0 应判为「不是完全相同」（旧实现会越界 panic）")
	}
	if !a.isSameRunLocked(2) {
		t.Errorf("n=2 且尾部两条相同 → 应为 true")
	}
}

// TestDoomWarnAfterZeroStillRuns 入口归一化：DoomWarnAfter=0 的 run 能正常跑完
// （旧实现在第一个工具调用处就 panic 掉整个实例）。
func TestDoomWarnAfterZeroStillRuns(t *testing.T) {
	cli := &stepClient{steps: []scriptStep{
		{Resp: toolResp(2, "alpha", map[string]interface{}{"command": "x"})},
		{Resp: textResp("跑完了")},
	}}
	a := newAgent(cli, "")
	opt := hardeningOpt()
	opt.DoomWarnAfter = 0
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("DoomWarnAfter=0 的 run 不应失败: %v", err)
	}
	if reply != "跑完了" {
		t.Errorf("reply = %q", reply)
	}
}

// ---------- L. 4xx 阶梯：先硬截断再组装 ----------

// recordClient 记录每次流式调用收到的消息条数（并让前 failN 次返回 400）
type recordClient struct {
	mu     sync.Mutex
	counts []int
	failN  int
	n      int
}

func (c *recordClient) Complete(ctx context.Context, msgs []types.Message, tools []types.ToolSpec) (*types.Response, error) {
	return textResp("[压缩:history] 测试压缩总结"), nil
}

func (c *recordClient) CompleteStream(ctx context.Context, msgs []types.Message, tools []types.ToolSpec, onDelta func(types.Delta), idleMs int) (*types.Response, error) {
	c.mu.Lock()
	c.counts = append(c.counts, len(msgs))
	c.n++
	n := c.n
	c.mu.Unlock()
	if n <= c.failN {
		return nil, &llm.HTTPError{Status: 400, Body: "request too large"}
	}
	return textResp("截断后正常"), nil
}

func (c *recordClient) snapshot() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.counts...)
}

// TestHardTruncateHappensBeforeCompose 4xx 阶梯第三级（truncate）必须让"重试的
// 请求"真的变短。旧实现先 Compose、后 ForceHardTruncate：截断只改 cm 内部状态，
// 组装好的 messages 切片不会回溯变短 → 重试发的仍是被拒的那份超长请求。
// 变异验证：把 ForceHardTruncate 挪回 Compose 之后 → 第二次请求条数不变 → 变红。
func TestHardTruncateHappensBeforeCompose(t *testing.T) {
	cli := &recordClient{failN: 1}
	cm := ctxmgr.NewManager(cli, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示", t.TempDir())
	// 塞 10 条 2 万字符的消息（总量 20 万字符，远超 HardTruncChars=5 万）
	big := strings.Repeat("字", 20000)
	for i := 0; i < 10; i++ {
		cm.AddMessage(&types.RawEntry{Role: "user", Content: big})
	}
	a := New(cli, cm, "")
	if _, err := a.Run(context.Background(), "你好", hardeningOpt()); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	counts := cli.snapshot()
	if len(counts) < 2 {
		t.Fatalf("应有至少 2 次请求（首次 4xx + 截断重试），实际 %d 次", len(counts))
	}
	if counts[1] >= counts[0] {
		t.Errorf("❌ 截断重试的请求没有变短（%d → %d）：硬截断一定发生在组装之后了，兜底无效",
			counts[0], counts[1])
	}
}

// ---------- M. 空响应兜底 ----------

// TestEmptyTextReplyFallsBackToPlaceholder 模型既没正文也没工具调用（连 thinking
// 都没有 → 不触发异常截断重试）→ 不得返回空串给前端。
func TestEmptyTextReplyFallsBackToPlaceholder(t *testing.T) {
	a := newAgent(&stepClient{steps: []scriptStep{{Resp: textResp("")}}}, "")
	reply, err := a.Run(context.Background(), "你好", hardeningOpt())
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if strings.TrimSpace(reply) == "" {
		t.Errorf("❌ 空响应不应返回空回复（前端空气泡）")
	}
	sess := a.cm.Session()
	last := ""
	for _, m := range sess.Messages {
		if m.Role == "assistant" {
			last = types.AsString(m.Content)
		}
	}
	if last != reply {
		t.Errorf("❌ 返回值 %q 与落会话文本 %q 不同源", reply, last)
	}
}

// ---------- N. doom 警告名 + warned 复位 ----------

func callOf(id, name, arg string) types.ToolCall {
	b, _ := json.Marshal(map[string]interface{}{"command": arg})
	return types.ToolCall{ID: id, Type: "function", Function: types.Function{Name: name, Arguments: string(b)}}
}

func callsResp(calls ...types.ToolCall) *types.Response {
	return &types.Response{FinishReason: types.StringPtr("tool_calls"), ToolCalls: calls}
}

// TestDoomWarnNamesTriggeringTool 一批 A,A,A,B：重复的是 A，警告必须点名 A
// （旧实现在批末取 lastCalls 最后一条 → 说成 B）。
//
// 同批末尾换个调用（B）也不该把刚发出的警告作废：旧实现在 A,A,A,B 这一批里
// 先置 warned=true、转身又因尾部序列被打断把它清掉，于是模型只要每批末尾换个
// 调用就永远停在警告档、永远不会被强停（下一批 A,A,A 只再警告一次）。
// 变异验证：任一条回退 → doom_warn 点名 B 或 doom_stop 不出现。
func TestDoomWarnNamesTriggeringTool(t *testing.T) {
	same := func(id string) types.ToolCall { return callOf(id, "alpha", "same-arg") }
	cli := &stepClient{steps: []scriptStep{
		// 第 1 批：A,A,A,B —— 第 3 个 A 触发「连续 3 次完全相同」
		{Resp: callsResp(same("a1"), same("a2"), same("a3"), callOf("b1", "beta", "other"))},
		// 第 2 批：又是 3 个 A → 警告后仍重复 → 强停
		{Resp: callsResp(same("a4"), same("a5"), same("a6"))},
		{Resp: textResp("（收尾回答）")},
	}}
	a := newAgent(cli, "")
	var evs eventRecorder
	var warnDetail, stopDetail string
	opt := hardeningOpt()
	opt.OnEvent = func(e types.AgentEvent) {
		evs.record(e)
		switch e["type"] {
		case "doom_warn":
			warnDetail, _ = e["detail"].(string)
		case "doom_stop":
			stopDetail, _ = e["detail"].(string)
		}
	}
	if _, err := a.Run(context.Background(), "你好", opt); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if evs.count("doom_warn") != 1 {
		t.Fatalf("应恰好 1 条 doom_warn，实际 %d", evs.count("doom_warn"))
	}
	if !strings.Contains(warnDetail, "alpha") || strings.Contains(warnDetail, "beta") {
		t.Errorf("❌ 警告应点名重复的那个工具 alpha，实际: %q", warnDetail)
	}
	if evs.count("doom_stop") != 1 {
		t.Errorf("❌ 警告后仍重复 A 应强停本轮（旧实现因同批复位永远停不下来），实际 %d 条；stop=%q",
			evs.count("doom_stop"), stopDetail)
	}
	assertToolCallsPaired(t, a.cm.Session())
}

// ---------- O. run_end 之后不再有本轮事件 ----------

// TestRunEndEmittedAfterCleanup run_end 必须是收尾序列的最后一步：拆掉压缩监听
// 之后再发。旧实现先发 run_end、后 offCompress —— 期间若还有压缩流出，前端
// 已认定本轮结束却又收到本轮的 compress 事件，轮次状态被打乱。
// 变异验证：把 emit(run_end) 挪回 defer 第一行 → run_end 之后又出现 compress → 变红。
func TestRunEndEmittedAfterCleanup(t *testing.T) {
	a := newAgent(&stepClient{steps: []scriptStep{{Resp: textResp("你好")}}}, "")
	var evs eventRecorder
	opt := hardeningOpt()
	opt.OnEvent = func(e types.AgentEvent) {
		evs.record(e)
		// run_end 的此刻同步触发一次压缩事件：若 agent 的压缩监听还挂着，
		// 它会出现在 run_end 之后（旧行为）
		if e["type"] == "run_end" {
			a.cm.ForceHardTruncate()
		}
	}
	if _, err := a.Run(context.Background(), "你好", opt); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	endIdx := -1
	for i, x := range evs.types {
		if x == "run_end" {
			endIdx = i
			break
		}
	}
	if endIdx < 0 {
		t.Fatalf("没有收到 run_end 事件")
	}
	for _, x := range evs.types[endIdx+1:] {
		if x == "compress" || x == "compress_delta" {
			t.Errorf("❌ run_end 之后仍出现 %s 事件（收尾顺序错了：应先拆监听再发 run_end）", x)
		}
	}
	if endIdx != len(evs.types)-1 {
		t.Errorf("❌ run_end 之后还有其他事件: %v", evs.types[endIdx+1:])
	}
}

// ---------- P. 轮内搭车：结构化区分系统通知 / 用户追加 ----------

// TestPiggybackUsesStructuredKind 用户追加消息即使以"后台通知"的那一句中文开头，
// 也必须加【用户追加】前缀；System=true 的通知则原样注入。
// 变异验证：退回 strings.HasPrefix 判定 → 第一个子用例变红。
func TestPiggybackUsesStructuredKind(t *testing.T) {
	cases := []struct {
		name      string
		msg       PendingMessage
		wantPlain bool // true = 期望原样（不加前缀）
	}{
		{name: "用户消息冒用通知前缀", msg: PendingMessage{Text: bgNoticePrefix + "——这是用户自己写的", System: false}, wantPlain: false},
		{name: "系统通知", msg: PendingMessage{Text: bgNoticePrefix + "——你此前调用 bash", System: true}, wantPlain: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAgent(&stepClient{steps: []scriptStep{
				{Resp: callsResp(callOf("c1", "alpha", "x"))},
				{Resp: textResp("收到")},
			}}, "")
			served := false
			opt := hardeningOpt()
			opt.TakePendingUsers = func() []PendingMessage {
				if served {
					return nil
				}
				served = true
				return []PendingMessage{c.msg}
			}
			if _, err := a.Run(context.Background(), "你好", opt); err != nil {
				t.Fatalf("run 失败: %v", err)
			}
			found := ""
			for _, m := range a.cm.Session().Messages {
				if m.Role == "user" && strings.Contains(types.AsString(m.Content), c.msg.Text) {
					found = types.AsString(m.Content)
				}
			}
			if found == "" {
				t.Fatalf("❌ 搭车消息没有注入会话")
			}
			marked := strings.HasPrefix(found, "【用户追加】")
			if c.wantPlain && marked {
				t.Errorf("❌ 系统通知不应加【用户追加】前缀：%q", found)
			}
			if !c.wantPlain && !marked {
				t.Errorf("❌ 用户追加消息必须加【用户追加】前缀（旧实现靠文本前缀判断会漏）：%q", found)
			}
		})
	}
}

// ---------- Q. 入口归一化 ----------

// TestNormalizeRunOptions 外部配置给到 0/负数时的兜底值。
// 变异验证：删掉任一分支 → 对应用例变红。
//
// 2026-10-06 二次反思：归一化会【改写用户显式写的值】，所以这里同时断言
// "哪些项刻意不归一化"——DoomWarnAfter 与 DataDir 的 0/1/空串是合法用户意图
// （关闭死循环检测 / 无处落盘），改掉它们等于替用户做决定。
func TestNormalizeRunOptions(t *testing.T) {
	got := normalizeRunOptions(RunOptions{})
	if got.ResultLimit != defaultResultLimit {
		t.Errorf("ResultLimit = %d，want %d", got.ResultLimit, defaultResultLimit)
	}
	if got.FgTimeoutMS <= 0 {
		t.Errorf("FgTimeoutMS = %d（0 会让工具结果与'转后台'随机二选一）", got.FgTimeoutMS)
	}
	// 刻意不归一化：DoomWarnAfter 0 = 关闭检测，DataDir 空 = 不落盘
	if got.DoomWarnAfter != 0 {
		t.Errorf("DoomWarnAfter 应原样保留 0（= 关闭检测，不得偷偷改成 3），实际 %d", got.DoomWarnAfter)
	}
	if got.DataDir != "" {
		t.Errorf("DataDir 空应原样保留（= 禁用落盘，不得退到临时目录），实际 %q", got.DataDir)
	}
	keep := normalizeRunOptions(RunOptions{DoomWarnAfter: 5, ResultLimit: 100, FgTimeoutMS: 1000, DataDir: "/x"})
	if keep.DoomWarnAfter != 5 || keep.ResultLimit != 100 || keep.FgTimeoutMS != 1000 || keep.DataDir != "/x" {
		t.Errorf("合法值被改动: %+v", keep)
	}
}

// TestResultLimitZeroNotTruncatedToNothing ResultLimit=0 时（配置被误写成 0），
// 工具结果不该被"截断"成只剩一句提示的空壳。
// 变异验证：去掉 normalizeRunOptions 的 ResultLimit 分支 → 结果只剩截断提示 → 变红。
func TestResultLimitZeroNotTruncatedToNothing(t *testing.T) {
	requireBash(t)
	cli := &stepClient{steps: []scriptStep{
		{Resp: callsResp(callOf("c1", "bash", "echo hello-normalized"))},
		{Resp: textResp("收到")},
	}}
	a := newAgent(cli, "")
	opt := hardeningOpt()
	opt.ResultLimit = 0
	if _, err := a.Run(context.Background(), "跑个命令", opt); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	found := ""
	for _, m := range a.cm.Session().Messages {
		if m.Role == "tool" {
			found = types.AsString(m.Content)
		}
	}
	if !strings.Contains(found, "hello-normalized") {
		t.Errorf("❌ ResultLimit=0 把工具结果截没了：%q", found)
	}
	if strings.Contains(found, "已截断") {
		t.Errorf("❌ ResultLimit=0 不应触发截断：%q", found)
	}
}

// ---------- R. 后台任务 StartedAt ----------

// TestBackgroundStartedAtCoversForegroundWait 转后台时上报的 StartedAt 必须是
// 工具真正启动的时刻，而不是"前台超时那一刻"——后台耗时长 = settle - StartedAt，
// 旧实现永远少算一整段 FgTimeoutMS。
// 变异验证：把 startedAt 挪回 fgTimer 分支内 → 与 tool_call 事件时刻差 ≈ 超时时长 → 变红。
func TestBackgroundStartedAtCoversForegroundWait(t *testing.T) {
	requireBash(t)
	const fgMS = 400
	var (
		mu        sync.Mutex
		callAt    time.Time
		startedAt int64
		got       bool
	)
	cli := &stepClient{steps: []scriptStep{
		{Resp: callsResp(callOf("c1", "bash", "sleep 2"))},
		{Resp: textResp("收到")},
	}}
	a := newAgent(cli, "")
	opt := hardeningOpt()
	opt.FgTimeoutMS = fgMS
	opt.OnEvent = func(e types.AgentEvent) {
		if e["type"] == "tool_call" {
			mu.Lock()
			callAt = time.Now()
			mu.Unlock()
		}
	}
	opt.OnBackgroundStart = func(args types.BackgroundStartArgs) {
		mu.Lock()
		startedAt = args.StartedAt
		got = true
		mu.Unlock()
	}
	if _, err := a.Run(context.Background(), "跑个慢命令", opt); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !got {
		t.Fatalf("❌ 命令没转后台（前台超时未触发），无法断言 StartedAt")
	}
	diff := time.Duration(startedAt-callAt.UnixMilli()) * time.Millisecond
	if diff < 0 {
		diff = -diff
	}
	// 容差取前台超时的一半：旧实现差 ≈ fgMS（超时才取），新实现差 ≈ 0
	if diff > time.Duration(fgMS/2)*time.Millisecond {
		t.Errorf("❌ StartedAt 距工具启动 %vms（前台超时 %dms）——计时是在超时那一刻才取的，后台耗时少算了一段",
			diff.Milliseconds(), fgMS)
	}
}

// requireBash 本机没有 bash 时跳过（工具层只注册了 bash，慢命令/真实结果依赖它）
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("本机无 bash，跳过：%v", err)
	}
}
