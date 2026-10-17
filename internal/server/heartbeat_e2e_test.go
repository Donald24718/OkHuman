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
