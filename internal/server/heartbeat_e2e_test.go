package server

import (
	"testing"
	"time"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

// TestHeartbeatFiresAndResets 空闲心跳端到端（2026-10-17）：FakeLLM 全链路——
// 把 LastRunEnd 拨到 61 秒前（阈值 1 分钟）→ 心跳必须真的入队并开启一轮 run
// （会话里出现心跳提示词的 user 消息 + 模型回复），且触发后计时归零、不会在
// 后续 tick 反复入队（否则 agent 会变成自循环烧 LLM）。
func TestHeartbeatFiresAndResets(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	cfg.Heartbeat = config.HeartbeatCfg{Enabled: true, IntervalMin: 1, Prompt: "（心跳测试提示词）"}
	fake := llm.NewFakeLLM([]llm.FakeStep{
		{Type: "text", Text: "（心跳轮）无待办，状态正常。"},
		{Type: "text", Text: "不该出现的第二轮"},
	})
	a := CreateAgentState(cfg, fake, "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)

	// 拨到"上次 run 结束在 61 秒前"
	st.LastRunEnd.Store(time.Now().UnixMilli() - 61000)
	stop := startHeartbeat(st, a, 20)
	defer stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(a.CM().Session().Messages) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	msgs := a.CM().Session().Messages
	if len(msgs) < 2 {
		t.Fatalf("心跳未开启一轮 run：会话消息数=%d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("首条应为心跳提示词（user），实际 role=%s", msgs[0].Role)
	}
	if s, ok := msgs[0].Content.(string); !ok || s != "（心跳测试提示词）" {
		t.Errorf("心跳提示词未原样入队：%v", msgs[0].Content)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("第二条应为模型回复（assistant），实际 role=%s", msgs[1].Role)
	}

	// 触发后计时已重置：距 now 应远小于阈值
	if idle := time.Now().UnixMilli() - st.LastRunEnd.Load(); idle > 5000 {
		t.Errorf("触发后未重置计时：idle=%dms", idle)
	}

	// 不得反复触发：再等 300ms（15 个 tick），消息数不应增长
	before := len(msgs)
	time.Sleep(300 * time.Millisecond)
	if got := len(a.CM().Session().Messages); got != before {
		t.Errorf("心跳重复触发：消息数 %d → %d（同一空闲期只应触发一次）", before, got)
	}
}

// TestHeartbeatDisabledNoRun 开关关 → 即使空闲超阈值也不得开启 run。
func TestHeartbeatDisabledNoRun(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	cfg.Heartbeat = config.HeartbeatCfg{Enabled: false, IntervalMin: 1, Prompt: "（不该出现）"}
	fake := llm.NewFakeLLM([]llm.FakeStep{{Type: "text", Text: "不该出现"}})
	a := CreateAgentState(cfg, fake, "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	st.LastRunEnd.Store(time.Now().UnixMilli() - 61000)
	stop := startHeartbeat(st, a, 20)
	defer stop()
	time.Sleep(300 * time.Millisecond)
	if n := len(a.CM().Session().Messages); n != 0 {
		t.Errorf("开关关却开启了 run：消息数=%d", n)
	}
}

// TestHeartbeatBackoffDelaysNextFire 空转避让端到端（2026-10-22）：
// 一轮"零工具调用"的心跳跑完 → 空转计数 +1 → 生效间隔翻倍 → 同样的空闲时长
// 第二次不该触发；够到翻倍后的阈值才再触发，计数继续涨。
// 这是唯一能证明"退避真的少烧了一次 LLM"的测法（纯函数测只证判定式）。
func TestHeartbeatBackoffDelaysNextFire(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	// 基准 1 分钟，封顶 8 分钟：1 → 2 → 4 → 8
	cfg.Heartbeat = config.HeartbeatCfg{Enabled: true, IntervalMin: 1, Backoff: true, BackoffMaxMin: 8, Prompt: "（心跳测试提示词）"}
	fake := llm.NewFakeLLM([]llm.FakeStep{
		{Type: "text", Text: "（心跳轮 1）无待办。"},
		{Type: "text", Text: "（心跳轮 2）无待办。"},
		{Type: "text", Text: "不该出现的第三轮"},
	})
	a := CreateAgentState(cfg, fake, "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)

	// 第 1 轮：streak=0 → 生效间隔 1 分钟，拨 61 秒应触发
	st.LastRunEnd.Store(time.Now().UnixMilli() - 61000)
	stop := startHeartbeat(st, a, 20)
	defer stop()
	if !waitMsgs(a, 2, 3*time.Second) {
		t.Fatalf("第一轮心跳未触发：消息数=%d", len(a.CM().Session().Messages))
	}
	// 注意同步点：消息是在 run 里追加的，空转计数是 run 返回后 runDrain 收尾时
	// 才更新的——拿消息数当信号会读到旧值（第一版就栽在这，假失败）。
	if !waitStreak(st, 1, 3*time.Second) {
		t.Fatalf("零工具调用的心跳后空转计数应=1，实际 %d", st.HbNoopStreak.Load())
	}

	// 第 2 次：streak=1 → 生效间隔 2 分钟。同样拨 61 秒，必须不触发。
	n0 := len(a.CM().Session().Messages)
	st.LastRunEnd.Store(time.Now().UnixMilli() - 61000)
	time.Sleep(400 * time.Millisecond) // 20 个 tick，足够触发却没触发
	if got := len(a.CM().Session().Messages); got != n0 {
		t.Errorf("退避没生效：61 秒空闲（生效阈值 2 分钟）却触发了，消息数 %d → %d", n0, got)
	}
	if got := st.HbNoopStreak.Load(); got != 1 {
		t.Errorf("未触发时计数不应变：期望 1，实际 %d", got)
	}

	// 第 3 次：拨 121 秒 ≥ 2 分钟 → 应触发，计数涨到 2
	st.LastRunEnd.Store(time.Now().UnixMilli() - 121000)
	if !waitMsgs(a, n0+2, 3*time.Second) {
		t.Fatalf("到 2 分钟阈值仍未触发：消息数=%d", len(a.CM().Session().Messages))
	}
	if !waitStreak(st, 2, 3*time.Second) {
		t.Errorf("第二轮空转后计数应=2，实际 %d", st.HbNoopStreak.Load())
	}
}

// TestHeartbeatToolCallResetsStreak 心跳轮里模型真的动手了（有工具调用）→
// 这轮不是空转，计数必须归零，间隔回到基准。
func TestHeartbeatToolCallResetsStreak(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	cfg.Heartbeat = config.HeartbeatCfg{Enabled: true, IntervalMin: 1, Backoff: true, BackoffMaxMin: 240, Prompt: "（心跳测试提示词）"}
	fake := llm.NewFakeLLM([]llm.FakeStep{
		{Type: "tool", Name: "bash", Args: map[string]interface{}{"command": "echo hb-work"}},
		{Type: "text", Text: "查完了，一切正常。"},
	})
	a := CreateAgentState(cfg, fake, "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	st.HbNoopStreak.Store(3) // 假装已经空转三轮 → 生效间隔 1*2^3 = 8 分钟
	// 所以空闲要拨够 8 分钟才会触发（拨 61 秒不该触发——那正是退避在起作用）
	st.LastRunEnd.Store(time.Now().UnixMilli() - 9*60*1000)
	stop := startHeartbeat(st, a, 20)
	defer stop()

	if !waitMsgs(a, 2, 5*time.Second) {
		t.Fatalf("心跳未触发（消息数=%d）", len(a.CM().Session().Messages))
	}
	if !waitStreak(st, 0, 5*time.Second) {
		t.Errorf("有工具调用的心跳应把空转计数归零，实际 %d", st.HbNoopStreak.Load())
	}
}

// waitMsgs 等会话消息数达到 n（超时返回 false）
func waitMsgs(a *AgentState, n int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(a.CM().Session().Messages) >= n {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// waitStreak 等空转计数达到 n（同上：不能拿消息数当同步点）
func waitStreak(st *AppState, n int64, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if st.HbNoopStreak.Load() == n {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
