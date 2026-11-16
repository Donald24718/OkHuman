package context

import (
	"okhuman/internal/textutil"
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
	if e.OrigChars != textutil.UTF16Len(long) {
		t.Fatalf("OrigChars=%d 应为 %d", e.OrigChars, textutil.UTF16Len(long))
	}
	if got := textutil.UTF16Len(e.Content); got > 1200 {
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

// TestSingleSummaryReplaced：多次压缩后始终只有 1 条总结，且它的 seq 区间向前扩张
// （新总结覆盖全场、替换旧的那条——旧 cfc5315^ 语义，不是并列窗口）。
func TestSingleSummaryReplaced(t *testing.T) {
	var evs []CompressEvent
	m := newWindowManager(t, []llm.FakeStep{{Type: "compress", Scope: "history", Text: "（假压缩总结）"}}, 1, 30000)
	m.AddCompressListener(func(e CompressEvent) { evs = append(evs, e) })
	addLong(m, 12)
	sess := m.Session()
	if len(sess.Summaries) != 1 {
		t.Fatalf("单条总结语义下应恰好 1 条，实得 %d", len(sess.Summaries))
	}
	if len(evs) < 3 {
		t.Fatalf("压了不到 3 次（%d 次），测不出替换", len(evs))
	}
	if sess.Summaries[0].SeqFrom != 1 {
		t.Fatalf("新总结应从最早一段起算，实得 %+v", sess.Summaries[0])
	}
}

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
	if e.OrigChars != textutil.UTF16Len("短正文") {
		t.Fatalf("OrigChars=%d，思考被计进正文了（应为 %d）", e.OrigChars, textutil.UTF16Len("短正文"))
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
	if c.KeepSummaries != 1 || c.SummaryMaxChars != 30000 {
		t.Fatalf("未归一：%+v", c)
	}
	c2 := NormalizeCfg(Cfg{KeepSummaries: 3, SummaryMaxChars: 2000})
	if c2.KeepSummaries != 1 || c2.SummaryMaxChars != 2000 {
		t.Fatalf("窗口应固定为 1（单条总结语义）：%+v", c2)
	}
}
