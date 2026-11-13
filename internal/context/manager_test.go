package context

import (
	"fmt"
	"okhuman/internal/textutil"
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

// TestContextStatsSummaries 摘要窗口的统计口径（2026-11-05 改：单条 → 多条）：
// 计数=窗口内条数、字符/token 是全部正文之和、截断条数单独报。
func TestContextStatsSummaries(t *testing.T) {
	with := &types.SessionState{Summaries: []types.SummaryEntry{
		{Content: "第一条总结 abcdef", CreatedAt: 1, OrigChars: 22},
		{Content: "第二条 中文", CreatedAt: 2, OrigChars: 500, Truncated: true},
	}}
	st := ContextStatsOf(with, 2, 500, 5, 30000)
	if st.SummaryCount != 2 {
		t.Fatalf("SummaryCount 应为 2，实得 %d", st.SummaryCount)
	}
	want := textutil.UTF16Len("第一条总结 abcdef") + textutil.UTF16Len("第二条 中文")
	if st.SummaryChars != want {
		t.Fatalf("SummaryChars=%d 应为累加 %d", st.SummaryChars, want)
	}
	if st.SummaryTruncCnt != 1 {
		t.Fatalf("被硬截断条数应为 1，实得 %d", st.SummaryTruncCnt)
	}
	if st.SummaryLimit != 5 || st.SummaryMaxChars != 30000 {
		t.Fatalf("窗口/上限应回显配置：%+v", st)
	}
	if len(st.SummaryStats) != 2 || st.SummaryStats[1].OrigChars != 500 || !st.SummaryStats[1].Truncated {
		t.Fatalf("逐条统计缺失：%+v", st.SummaryStats)
	}
	if st.SummaryTokens == 0 || st.TotalTokens != st.MsgTokens+st.SummaryTokens {
		t.Fatalf("token 统计不一致：%+v", st)
	}
	st2 := ContextStatsOf(&types.SessionState{}, 2, 500, 5, 30000)
	if st2.SummaryCount != 0 || st2.SummaryTokens != 0 || st2.SummaryChars != 0 {
		t.Fatalf("无摘要时应全 0：%+v", st2)
	}
}

// TestSummaryFilePairing（2026-09-25，2026-11-05 改口径）：压缩后总结正文落盘
// session-summaries/，与 session-records/ 同名基座成对；两条路径存在条目字段里，
// 注入时由 RenderSummary 渲染成指针（正文与指针分开存：正文是唯一计量口径）。
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
	m.SetSessionNo(4)
	for i := 0; i < 3; i++ {
		m.AddMessage(&types.RawEntry{Role: "user", Content: "这是一条比较长的消息内容 " + strings.Repeat("x", 30)})
	}
	sess := m.Session()
	if len(sess.Summaries) < 1 {
		t.Fatalf("摘要窗口为空（压缩未触发）")
	}
	e := sess.Summaries[len(sess.Summaries)-1]
	if e.SummaryFile == "" || e.RecordFile == "" {
		t.Fatalf("条目没记下两个路径：%+v", e)
	}
	recPath, sumPath := e.RecordFile, e.SummaryFile
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
	if !strings.HasPrefix(recBase, "ctx-4-") {
		t.Fatalf("文件名会话号未用 SetSessionNo 的值（应 ctx-4-…）：%s", recPath)
	}
	// 注入的消息里必须带两条指针 + 表头（模型能自己去找原文）
	inj := ComposeMessages(sess, "sys")
	var joined string
	for _, msg := range inj {
		if msg.Role == "user" {
			joined += types.AsString(msg.Content) + "\n"
		}
	}
	suffix := fmt.Sprintf("/%d ·", len(sess.Summaries)) // 表头形如 "[历史总结] 2/2 · 覆盖 seq …"
	for _, want := range []string{"[本条覆盖的完整原文已存档：" + recPath, "[本条总结文本：" + sumPath, SummaryMarker + " ", suffix} {
		if !strings.Contains(joined, want) {
			t.Fatalf("注入的总结消息缺少 %q\n%s", want, joined)
		}
	}
	data, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("总结文本不可读：%v", err)
	}
	if !strings.Contains(string(data), "（假压缩总结）") {
		t.Fatalf("总结文本内容不对：%s", data)
	}
}

// TestDumpSessionRecord（2026-10-18）：重置会话前的全文落盘。
// 重置会整体换掉 Manager，旧会话原文除压缩存档外无处可寻 → DumpSessionRecord
// 必须写出与压缩存档同口径的文件，并在文件头标明触发场景。
func TestDumpSessionRecord(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(llm.NewFakeLLM(nil), Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示词ABC", dir)
	if p, n, _ := m.DumpSessionRecord("重置会话"); p != "" || n != 0 {
		t.Fatalf("空会话不该落盘：%s n=%d", p, n)
	}
	m.AddMessage(&types.RawEntry{Role: "user", Content: "待留下的原文甲"})
	m.AddMessage(&types.RawEntry{Role: "assistant", Content: "待留下的原文乙"})
	path, n, derr := m.DumpSessionRecord("重置会话")
	if derr != nil {
		t.Fatalf("存档失败：%v", derr)
	}
	if n != 2 {
		t.Fatalf("消息条数 = %d，期望 2", n)
	}
	if !strings.HasPrefix(path, filepath.Join(dir, "session-records")) {
		t.Fatalf("落盘位置不对：%s", path)
	}
	if !strings.Contains(filepath.Base(path), "ctx-") {
		t.Errorf("文件名应与压缩存档同口径（ctx- 前缀）：%s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读全文存档失败: %v", err)
	}
	got := string(b)
	for _, want := range []string{"触发=重置会话", "待留下的原文甲", "待留下的原文乙", "系统提示词ABC"} {
		if !strings.Contains(got, want) {
			t.Errorf("全文存档缺 %q", want)
		}
	}
}

// TestAttachmentTokensAccounting 附件按配置的 token 计（context.attachment_tokens），
// 不再是"每个非文本 part 固定 1000 字符"那种写死的间接口径。
func TestAttachmentTokensAccounting(t *testing.T) {
	img, err := types.FromPartMap(map[string]interface{}{
		"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64,AAAA"},
	})
	if err != nil {
		t.Fatalf("构造 image part: %v", err)
	}
	one := types.RawEntry{Role: "user", Content: []types.ContentPart{types.TextPart("abcd"), img}}
	two := types.RawEntry{Role: "user", Content: []types.ContentPart{types.TextPart("abcd"), img, types.RefPart("r1")}}

	// cpl=2、附件 500 token：文本 "abcd"=4 字符=2 token，附件正好 500 token
	if got := EstimateMessageTokens(one, NewEst(2, 500)); got != 502 {
		t.Fatalf("附件 500 token 时单图消息应为 502，实得 %d", got)
	}
	if got := EstimateMessageTokens(two, NewEst(2, 500)); got != 1002 {
		t.Fatalf("两个附件应为 2+2×500=1002，实得 %d", got)
	}
	// 配 0 → 附件不计（合法值，不回填默认）
	if got := EstimateMessageTokens(one, NewEst(2, 0)); got != 2 {
		t.Fatalf("attachment_tokens=0 时应只剩文本 2 token，实得 %d", got)
	}
	// 换 cpl 不改变"按 token 记"的语义：附件仍是 500 token
	if got := EstimateMessageTokens(one, NewEst(4, 500)); got != 501 {
		t.Fatalf("cpl=4 时附件仍应计 500 token（文本 1 token），实得 %d", got)
	}
	// 字符预算口径（keep_recent_chars / hard_trunc_chars）也要看见附件：
	// 500 token × cpl 2 = 1000 字符 + 文本 4 字符
	if got := messageChars(one, NewEst(2, 500)); got != 1004 {
		t.Fatalf("messageChars 应为 1004，实得 %d", got)
	}
	// 负数按 0 处理，不出现负字符
	if got := messageChars(one, NewEst(2, -7)); got != 4 {
		t.Fatalf("负 attachment_tokens 应按 0 处理，实得 %d", got)
	}
}
