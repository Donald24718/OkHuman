package context

import (
	"strings"
	"testing"

	"okhuman/internal/llm"
	"okhuman/internal/types"
)

// newWindowManager 造一个必然触发压缩的 Manager（极小阈值 + 自定义窗口/上限）
func newWindowManager(t *testing.T, steps []llm.FakeStep, keep, maxChars int) *Manager {
	t.Helper()
	return NewManager(llm.NewFakeLLM(steps), Cfg{
		MaxTokens:       10,
		KeepRecentChars: 5,
		HardTruncChars:  100,
		StreamIdleMS:    200,
		CharsPerToken:   1,
		KeepSummaries:   keep,
		SummaryMaxChars: maxChars,
	}, "sys", t.TempDir())
}

func addLong(m *Manager, n int) {
	for i := 0; i < n; i++ {
		m.AddMessage(&types.RawEntry{Role: "user", Content: "较长的消息内容 " + strings.Repeat("x", 40)})
	}
}

// TestSummaryHardTruncate：正文超上限 → 硬截断 + 条目记 Truncated/OrigChars，
// 注入时开头就提醒模型"这条是硬截断的"。提醒行本身也计入上限。
func TestSummaryHardTruncate(t *testing.T) {
	long := strings.Repeat("甲", 3000)
	m := newWindowManager(t, []llm.FakeStep{{Type: "compress", Scope: "history", Text: long}}, 5, 1200)
	addLong(m, 3)
	sess := m.Session()
	if len(sess.Summaries) < 1 {
		t.Fatalf("没有摘要（压缩未触发）")
	}
	e := sess.Summaries[len(sess.Summaries)-1]
	if !e.Truncated {
		t.Fatalf("未标 Truncated：%+v", e)
	}
	if e.OrigChars != utf16Len(long) {
		t.Fatalf("OrigChars=%d 应为 %d", e.OrigChars, utf16Len(long))
	}
	if got := utf16Len(e.Content); got > 1200 {
		t.Fatalf("正文（含提醒行）%d 超上限 1200", got)
	}
	if !strings.Contains(e.Content, "硬截断") {
		t.Fatalf("正文里没有截断提醒：%q", e.Content)
	}
	inj := ComposeMessages(sess, "sys")
	var joined string
	for _, msg := range inj {
		if msg.Role == "user" {
			joined += types.AsString(msg.Content) + "\n"
		}
	}
	if !strings.Contains(joined, "⚠️ 本条总结是硬截断的") {
		t.Fatalf("注入时未提醒模型这是硬截断摘要：%q", joined)
	}
}

// TestSummaryWindowDrop：keep_summaries=2 → 多次压缩后窗口里只剩最新 2 条，
// 且顺序是旧→新（丢弃最旧的，不做任何合并）。
func TestSummaryWindowDrop(t *testing.T) {
	var evs []CompressEvent
	m := newWindowManager(t, []llm.FakeStep{{Type: "compress", Scope: "history", Text: "（假压缩总结）"}}, 2, 30000)
	m.AddCompressListener(func(e CompressEvent) { evs = append(evs, e) })
	addLong(m, 12)
	sess := m.Session()
	if len(sess.Summaries) > 2 {
		t.Fatalf("窗口超过 keep_summaries=2：%d 条", len(sess.Summaries))
	}
	if len(evs) < 3 {
		t.Fatalf("压了不到 3 次（%d 次），测不出丢弃", len(evs))
	}
	if len(sess.Summaries) != 2 {
		t.Fatalf("应保留 2 条，实得 %d", len(sess.Summaries))
	}
	if sess.Summaries[0].SeqTo >= sess.Summaries[1].SeqTo {
		t.Fatalf("窗口顺序应为旧→新：%+v %+v", sess.Summaries[0], sess.Summaries[1])
	}
	droppedSeen := false
	for _, e := range evs {
		if strings.Contains(e.Detail, "丢弃最旧一条") {
			droppedSeen = true
		}
	}
	if !droppedSeen {
		t.Fatalf("丢弃没有进事件日志（UI 会看不见）")
	}
}

// TestSummaryThinkingNotCounted：压缩轮的思考不计入正文长度，也不进摘要正文。
func TestSummaryThinkingNotCounted(t *testing.T) {
	think := strings.Repeat("思考", 2000)
	m := newWindowManager(t, []llm.FakeStep{{
		Type: "compress", Scope: "history", Text: "短正文", Reasoning: &think,
	}}, 5, 1000)
	addLong(m, 3)
	sess := m.Session()
	if len(sess.Summaries) < 1 {
		t.Fatalf("没有摘要（压缩未触发）")
	}
	e := sess.Summaries[len(sess.Summaries)-1]
	if e.OrigChars != utf16Len("短正文") {
		t.Fatalf("OrigChars=%d，思考被计进正文了（应为 %d）", e.OrigChars, utf16Len("短正文"))
	}
	if strings.Contains(e.Content, "思考") {
		t.Fatalf("正文里混进了思考：%q", e.Content)
	}
	if e.Truncated {
		t.Fatalf("思考很长不应触发截断：%+v", e)
	}
}

// TestNormalizeCfg：user.json 里写了非法值（0/负数）时退回安全默认 ——
// 否则 keep_summaries=0 会变成"无限窗口"、summary_max_chars=0 会变成"不截断"。
func TestNormalizeCfg(t *testing.T) {
	c := NormalizeCfg(Cfg{KeepSummaries: 0, SummaryMaxChars: -1})
	if c.KeepSummaries != 5 || c.SummaryMaxChars != 30000 {
		t.Fatalf("未归一：%+v", c)
	}
	c2 := NormalizeCfg(Cfg{KeepSummaries: 3, SummaryMaxChars: 2000})
	if c2.KeepSummaries != 3 || c2.SummaryMaxChars != 2000 {
		t.Fatalf("合法值被改了：%+v", c2)
	}
}
