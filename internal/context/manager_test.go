package context

import (
	"os"
	"path/filepath"
	"strings"
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

// TestContextStatsSummaryCount 回归（2026-09-14）：Summary 计数曾硬编码 0，
// 导致 WebUI 已压缩仍显示 [summary ×0] / 滚动总结"无"。
func TestContextStatsSummaryCount(t *testing.T) {
	// 有 summary → Summary=1，且 token/char 统计非零
	with := &types.SessionState{
		Summary: &types.TEntry{Content: "历史总结内容 12345", CreatedAt: 1},
	}
	st := ContextStatsOf(with, 2)
	if st.Summary != 1 {
		t.Fatalf("有 summary 时 Summary 应为 1，实得 %d", st.Summary)
	}
	if st.SummaryChars != utf16Len("历史总结内容 12345") {
		t.Fatalf("SummaryChars = %d，期望 %d", st.SummaryChars, utf16Len("历史总结内容 12345"))
	}
	if st.SummaryTokens == 0 || st.TotalTokens != st.MsgTokens+st.SummaryTokens {
		t.Fatalf("token 统计不一致：%+v", st)
	}
	// 无 summary → Summary=0
	without := &types.SessionState{}
	st2 := ContextStatsOf(without, 2)
	if st2.Summary != 0 || st2.SummaryTokens != 0 || st2.SummaryChars != 0 {
		t.Fatalf("无 summary 时应全 0：%+v", st2)
	}
}

// extractBetween 取 s 中 pre 与 post 之间的子串（测试指针路径提取用）
func extractBetween(s, pre, post string) string {
	i := strings.Index(s, pre)
	if i < 0 {
		return ""
	}
	rest := s[i+len(pre):]
	j := strings.Index(rest, post)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestSummaryFilePairing（2026-09-25）：压缩后总结文本落盘 session-summaries/，
// 与 session-records/ 存档同名基座成对（不同文件夹）；summary 指针含两个路径。
func TestSummaryFilePairing(t *testing.T) {
	dir := t.TempDir()
	fake := llm.NewFakeLLM([]llm.FakeStep{
		{Type: "compress", Scope: "history", Text: "（假压缩总结）"},
	})
	m := NewManager(fake, Cfg{
		MaxTokens:       10,
		KeepRecentChars: 5,
		HardTruncChars:  100,
		StreamIdleMS:    200,
		CharsPerToken:   1,
	}, "sys", dir)
	for i := 0; i < 3; i++ {
		m.AddMessage(&types.RawEntry{Role: "user", Content: "这是一条比较长的消息内容 " + strings.Repeat("x", 30)})
	}
	sess := m.Session()
	if sess.Summary == nil {
		t.Fatal("无 summary（压缩未触发）")
	}
	ref := sess.Summary.Content
	recPath := extractBetween(ref, "[完整原文已存档：", " ——")
	sumPath := extractBetween(ref, "[压缩后总结文本：", " ——")
	if recPath == "" || sumPath == "" {
		t.Fatalf("指针缺路径（rec=%q sum=%q）：\n%s", recPath, sumPath, ref)
	}
	if !strings.HasPrefix(recPath, filepath.Join(dir, "session-records")) {
		t.Fatalf("存档路径不在 session-records/：%s", recPath)
	}
	if !strings.HasPrefix(sumPath, filepath.Join(dir, "session-summaries")) {
		t.Fatalf("总结路径不在 session-summaries/：%s", sumPath)
	}
	recBase := strings.TrimSuffix(filepath.Base(recPath), ".txt")
	sumBase := strings.TrimSuffix(filepath.Base(sumPath), ".summary.txt")
	if recBase != sumBase {
		t.Fatalf("基座名不成对：rec=%s sum=%s", recPath, sumPath)
	}
	data, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("总结文件未落盘：%v", err)
	}
	body := string(data)
	if !strings.Contains(body, "（假压缩总结）") {
		t.Fatalf("总结文件内容不符：%s", body)
	}
	if !strings.Contains(body, "配对完整存档："+recPath) {
		t.Fatalf("总结文件头缺配对存档指引：%s", body)
	}
}
