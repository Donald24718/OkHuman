package persist

import (
	"os"
	"path/filepath"
	"testing"

	"okhuman/internal/types"
)

// TestSaveLoadSummariesRoundTrip：摘要窗口写进去读回来，逐字段一致（旧→新顺序保留）。
func TestSaveLoadSummariesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := &Snapshot{No: 3, Seq: 17, State: &types.SessionState{ID: 3, Summaries: []types.SummaryEntry{
		{Content: "第一条正文", CreatedAt: 111, SeqFrom: 1, SeqTo: 5, OrigChars: 5, Truncated: false,
			SummaryFile: "/x/ctx-3-a.summary.txt", RecordFile: "/x/ctx-3-a.txt"},
		{Content: "第二条正文（被截断）", CreatedAt: 222, SeqFrom: 6, SeqTo: 9, OrigChars: 4000, Truncated: true,
			SummaryFile: "/x/ctx-3-b.summary.txt", RecordFile: "/x/ctx-3-b.txt"},
	}, Messages: []types.RawEntry{{Role: "user", Content: "在"}}}}
	if err := p.SaveSession(in); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSessionFile(dir)
	if err != nil || got == nil {
		t.Fatalf("读不回来：err=%v got=%v", err, got)
	}
	if len(got.State.Summaries) != 2 {
		t.Fatalf("条数不对：%d", len(got.State.Summaries))
	}
	s := got.State.Summaries[1]
	if s.Content != "第二条正文（被截断）" || !s.Truncated || s.OrigChars != 4000 ||
		s.SeqFrom != 6 || s.SeqTo != 9 || s.CreatedAt != 222 || s.RecordFile != "/x/ctx-3-b.txt" {
		t.Fatalf("字段丢失/错位：%+v", s)
	}
}

// TestOldSingleSummaryFieldIgnored：旧文件里的单条 "summary" 字段 —— 不做兼容、
// 不迁移、也不报错：加载结果是"零条摘要"。这是刻意的静默降级，
// 原文仍在 session-records/，所以数据不丢。
func TestOldSingleSummaryFieldIgnored(t *testing.T) {
	dir := t.TempDir()
	old := `{"no":2,"seq":8,"state":{"id":2,"summary":{"content":"旧的滚动总结","created_at":123},` +
		`"messages":[{"role":"user","content":"一条旧消息"}]}}`
	if err := os.WriteFile(filepath.Join(dir, SessionFile), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSessionFile(dir)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got == nil {
		t.Fatal("整份被拒载了（旧字段应被忽略，消息要留住）")
	}
	if len(got.State.Summaries) != 0 {
		t.Fatalf("旧 summary 不该被读成摘要：%+v", got.State.Summaries)
	}
	if len(got.State.Messages) != 1 {
		t.Fatalf("消息应照常加载：%d 条", len(got.State.Messages))
	}
}

// TestBadSummariesRejected：summaries 项形状异常 → 整份拒载（与 validEntry 同口径，
// 宁可空启动也不要半份上下文）。
func TestBadSummariesRejected(t *testing.T) {
	dir := t.TempDir()
	bad := `{"no":1,"seq":3,"state":{"id":1,"summaries":[{"content":123}],` +
		`"messages":[{"role":"user","content":"x"}]}}`
	if err := os.WriteFile(filepath.Join(dir, SessionFile), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadSessionFile(dir); got != nil {
		t.Fatalf("坏条目应整份拒载，实得 %+v", got)
	}
}
