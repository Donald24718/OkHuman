package context

import (
	"sync"
	"testing"

	"okhuman/internal/llm"
	"okhuman/internal/types"
)

// 审计 H2 回归（2026-09-14）：/inject 与在飞 run 并发 AddMessage 时，
// 压缩必须串行（changeMu），session 不得出现撕裂/错切。
// 极小 max_tokens → 每条 AddMessage 都触发压缩；10 路并发后：
// seq 恰好 +10（每条消息都入账）、消息数 ≥1（keep=1 保底）、无 panic。
// 配合 -race 运行可抓旧的并发 append/切批竞态。
func TestConcurrentAddMessageCompression(t *testing.T) {
	fake := llm.NewFakeLLM([]llm.FakeStep{
		{Type: "compress", Scope: "history", Text: "（假压缩总结）"},
	})
	m := NewManager(fake, Cfg{
		MaxTokens:       10, // 极小 → 每次都触发压缩
		KeepRecentChars: 5,
		HardTruncChars:  100,
		StreamIdleMS:    200,
		CharsPerToken:   1,
	}, "sys", t.TempDir())
	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := "消息"
			for k := 0; k < i+10; k++ {
				s += string(rune('a' + i))
			}
			m.AddMessage(&types.RawEntry{Role: "user", Content: s})
		}(i)
	}
	wg.Wait()
	if got := m.GetSeq(); got != n {
		t.Fatalf("seq=%d want %d（每条消息必须恰好入账一次）", got, n)
	}
	sess := m.Session()
	if len(sess.Messages) < 1 {
		t.Fatalf("消息全部丢失：Messages 为空且无 summary 兜底")
	}
	for i, e := range sess.Messages {
		if e.Role == "" {
			t.Fatalf("消息 %d role 为空（session 撕裂）", i)
		}
	}
}
