package ipython

// 单元测试：不需要 Python 即可跑（同穿衣Nym building block）。

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAppliedLimitsFallback 未接线 Configure 时也要给出可用的超时。
//
// 这条防的是：Specs() 走"未接线兜底"路径时 defMS/maxMS 还是 0，
// 若 appliedLimits 直接返回 0 秒，模型会看到"默认 0 秒"的错误承诺。
func TestAppliedLimitsFallback(t *testing.T) {
	defSec, maxSec := appliedLimits(0, 0)
	if defSec < minTimeoutSec || maxSec < defSec {
		t.Fatalf("兜底超时非法: def=%d max=%d", defSec, maxSec)
	}
	// max 不得小于 def：否则任何请求都会立刻撞上限。
	if _, m := appliedLimits(60000, 10000); m < 60 {
		t.Fatalf("max 被错误压到 def 之下: %d", m)
	}
}

func TestSameIDTolerateFloat64(t *testing.T) {
	// JSON number 解码后恒为 float64：请求里的 7 回来变成 1.0 形态的 7.0。
	v := float64(7)
	if !sameID(v, 7) {
		t.Errorf("float64(7) 应匹配 id=7")
	}
	if sameID(v, 8) {
		t.Errorf("float64(7) 不应匹配 id=8")
	}
	if sameID(nil, 7) || sameID("x", 7) {
		t.Errorf("非法类型不应匹配任何 id")
	}
	var num json.Number
	if err := json.Unmarshal([]byte(`9`), &num); err != nil {
		t.Fatal(err)
	}
	if !sameID(num, 9) {
		t.Errorf("json.Number(9) 应匹配 id=9")
	}
	if !sameID(int64(9), 9) {
		t.Errorf("int64(9) 应匹配 id=9")
	}
}

func TestNumArgRejectsUnsafe(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		ok   bool
	}{
		{"nil", nil, false},
		{"nan", math.NaN(), false},
		{"inf", math.Inf(1), false},
		{"字符串数字", "3.5", true},
		{"非数字串", "abc", false},
		{"超大整数", int64(1) << 54, false},
		{"普通整数", 42, true},
		{"布尔(非数值)", true, false},
	}
	for _, c := range cases {
		_, ok := numArg(c.in)
		if ok != c.ok {
			t.Errorf("%s: ok=%v，期望 %v", c.name, ok, c.ok)
		}
	}
}

func TestTimeoutFromClamps(t *testing.T) {
	defMS.Store(60000)
	maxMS.Store(120000)
	defer func() { defMS.Store(0); maxMS.Store(0) }()

	if got := timeoutFrom(map[string]interface{}{}); got != 60*time.Second {
		t.Errorf("缺省超时 = %v，期望 60s", got)
	}
	if got := timeoutFrom(map[string]interface{}{"timeout_seconds": float64(5)}); got != 5*time.Second {
		t.Errorf("显式 5s → %v", got)
	}
	// 超上限必须被夹住，而不是报错（模型只是想表达"尽量久"）。
	if got := timeoutFrom(map[string]interface{}{"timeout_seconds": float64(99999)}); got != 120*time.Second {
		t.Errorf("超上限应夹到 120s，实际 %v", got)
	}
	if got := timeoutFrom(map[string]interface{}{"timeout_seconds": float64(0.1)}); got != time.Duration(minTimeoutSec)*time.Second {
		t.Errorf("低于下限应升到 1s，实际 %v", got)
	}
	// 非法类型退回默认，不能 panic。
	if got := timeoutFrom(map[string]interface{}{"timeout_seconds": "abc"}); got != 60*time.Second {
		t.Errorf("非法类型应退回默认，实际 %v", got)
	}
}

func TestMaterializeLauncherIdempotent(t *testing.T) {
	p1, err := MaterializeLauncher()
	if err != nil {
		t.Fatalf("MaterializeLauncher: %v", err)
	}
	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("launcher 未落盘: %v", err)
	}
	data, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	// 内嵌内容不能是空的：embed 失败会得到空切片，进而让 python 起一个空脚本 →
	// 表现为"握手超时"，很难排查，因此在源头拦住。
	if !strings.Contains(string(data), "OkHuman IPython launcher") {
		t.Errorf("落盘的 launcher 内容异常（前 80 字符：%q）", string(data[:minInt(80, len(data))]))
	}
	p2, err := MaterializeLauncher()
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Errorf("重复调用应返回同一路径：%q vs %q", p1, p2)
	}
	// 具现化目录必须是私有子目录，不能是 os.TempDir() 根
	// （记忆里的坑：Windows 上它与 Git Bash 的 /tmp 是同一目录，
	//  模型一句 rm -rf /tmp/* 会把它清空）。
	if filepathDir(p1) == os.TempDir() {
		t.Errorf("launcher 落在 TempDir 根目录：%s", p1)
	}
}

func filepathDir(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return p
	}
	return p[:i]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- 输出格式化

func TestFormatResultSeparatesInterruptFromUserError(t *testing.T) {
	const timeout = 2 * time.Second

	// 超时中断：绝不能被说成"用户代码出错"。
	inter := ExecResult{ErrName: KeyboardInterruptName, ErrValue: "", Elapsed: 2100 * time.Millisecond}
	got := FormatResult(inter, timeout)
	if !strings.Contains(got, "被中断") {
		t.Errorf("中断结果未说明被中断：\n%s", got)
	}
	if strings.Contains(got, "出错: KeyboardInterrupt") {
		t.Errorf("中断被误报成用户代码出错：\n%s", got)
	}
	if !strings.Contains(got, "变量保留") {
		t.Errorf("未告知变量已保留（这是本方案的核心卖点）：\n%s", got)
	}

	// 用户代码出错：必须照实报错，不能被吞。
	cnt := 3
	userErr := ExecResult{ErrName: "ValueError", ErrValue: "boom", Count: &cnt, Elapsed: time.Millisecond}
	got = FormatResult(userErr, timeout)
	if !strings.Contains(got, "出错: ValueError: boom") {
		t.Errorf("未照实报出用户代码异常：\n%s", got)
	}
	if strings.Contains(got, "被中断") {
		t.Errorf("用户代码出错被误标成中断：\n%s", got)
	}
}

func TestFormatResultHardKillAdmitsVariableLoss(t *testing.T) {
	// 硬杀必须如实告知变量丢失——隐瞒会让模型以为状态还在，
	// 下一句直接引用旧变量拿到 NameError。
	killed := ExecResult{Killed: true, Elapsed: 6 * time.Second}
	got := FormatResult(killed, 2*time.Second)
	if !strings.Contains(got, "变量已丢失") {
		t.Errorf("硬杀未如实告知变量丢失：\n%s", got)
	}
	if strings.Contains(got, "变量保留") {
		t.Errorf("硬杀却声称变量保留：\n%s", got)
	}
}

func TestFormatResultPrefersPlainText(t *testing.T) {
	res := ExecResult{Result: map[string]string{
		"text/plain": "42",
		"text/html":  "<b>42</b>",
	}, Elapsed: time.Millisecond}
	got := FormatResult(res, time.Second)
	if !strings.Contains(got, "42") {
		t.Errorf("未内联 text/plain：\n%s", got)
	}
	if strings.Contains(got, "<b>42</b>") {
		t.Errorf("有 text/plain 时不该用 HTML：\n%s", got)
	}
}

// tinyPNGBase64 一个 69 字节的合法 1x1 PNG（用于落盘路径的单测，不需要 Python）。
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGNgYGAAAAAEAAH2FzhVAAAAAElFTkSuQmCC"

// withArtifactDir 把落盘目录指到测试临时目录，测试结束恢复默认。
func withArtifactDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetArtifactDir(dir)
	t.Cleanup(func() { SetArtifactDir("") })
	return dir
}

// TestSaveArtifactsWritesRealPNG 图片必须落成**真二进制**，不是 base64 文本副本。
//
// 守护的失败模式：落盘时忘记 base64 解码 → 文件里躺着一段 base64 字符，
// 模型拿到路径也读不出图，而且这种 bug 肉眼查日志看不出来（路径、大小都对）。
func TestSaveArtifactsWritesRealPNG(t *testing.T) {
	withArtifactDir(t)
	res := ExecResult{Result: map[string]string{"image/png": tinyPNGBase64}}
	saveArtifacts(&res)

	if len(res.Saved) != 1 {
		t.Fatalf("应落盘 1 件，实际 %d", len(res.Saved))
	}
	a := res.Saved[0]
	if a.MIME != "image/png" || a.Bytes != 69 {
		t.Errorf("落盘元数据不对: %+v", a)
	}
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatalf("读回落盘文件失败: %v", err)
	}
	if string(raw[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Errorf("落盘内容不是 PNG（前 8 字节=%x），多半是忘了 base64 解码", raw[:8])
	}
	if _, left := res.Result["image/png"]; left {
		t.Errorf("已落盘的 MIME 必须从 result 摘掉，否则尾部提示会自相矛盾")
	}
	if !strings.HasSuffix(a.Path, ".png") {
		t.Errorf("扩展名应反映 MIME，实际 %s", a.Path)
	}
}

// TestSaveArtifactsSVGRawText SVG 是纯文本，按 base64 解码会失败（实测约定）。
func TestSaveArtifactsSVGRawText(t *testing.T) {
	withArtifactDir(t)
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><circle cx="5" cy="5" r="4"/></svg>`
	res := ExecResult{Result: map[string]string{"image/svg+xml": svg}}
	saveArtifacts(&res)

	if len(res.Saved) != 1 {
		t.Fatalf("SVG 应落盘，实际 %d", len(res.Saved))
	}
	raw, err := os.ReadFile(res.Saved[0].Path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if string(raw) != svg {
		t.Errorf("SVG 被当成 base64 处理了: %q", string(raw[:40]))
	}
}

// TestSaveArtifactsKeepsBadBase64InPlace 落盘失败不能把内容静默弄丢。
func TestSaveArtifactsKeepsBadBase64InPlace(t *testing.T) {
	withArtifactDir(t)
	res := ExecResult{Result: map[string]string{"image/png": "not-valid-base64!!!"}}
	saveArtifacts(&res)

	if len(res.Saved) != 0 {
		t.Errorf("非法 base64 不该落盘: %+v", res.Saved)
	}
	if v, ok := res.Result["image/png"]; !ok || v != "not-valid-base64!!!" {
		t.Errorf("落盘失败必须把内容留在原处，实际 %v", res.Result)
	}
	// 留在原处 → 尾部提示仍会如实说明，模型不至于完全失去线索。
	if got := FormatResult(res, time.Second); !strings.Contains(got, "image/png") {
		t.Errorf("落盘失败时图片 MIME 被静默吞掉：\n%s", got)
	}
}

// TestFormatResultShowsArtifactPath 落盘后给模型的是**路径**，不是过期建议。
func TestFormatResultShowsArtifactPath(t *testing.T) {
	withArtifactDir(t)
	res := ExecResult{Result: map[string]string{"image/png": tinyPNGBase64}, Elapsed: time.Millisecond}
	saveArtifacts(&res)
	got := FormatResult(res, time.Second)

	if !strings.Contains(got, res.Saved[0].Path) {
		t.Errorf("返回值里没有落盘路径，模型拿不到图：\n%s", got)
	}
	if !strings.Contains(got, "已落盘") {
		t.Errorf("缺少落盘区段：\n%s", got)
	}
	// 图片已自动落盘，再建议 savefig 就是过期且矛盾的指引。
	if strings.Contains(got, "savefig") {
		t.Errorf("图片已自动落盘，不该再建议手动 savefig：\n%s", got)
	}
}

// TestFormatResultRendersDisplays display() 的内容必须出现在返回值里。
//
// 守护的失败模式：IPython 的默认 DisplayPublisher 会把富对象退化成
// `<IPython.core.display.HTML object>` 一行 repr，模型什么都看不到。
func TestFormatResultRendersDisplays(t *testing.T) {
	res := ExecResult{
		Displays: []map[string]string{
			{"text/html": "<b>hi</b>", "text/plain": "<IPython.core.display.HTML object>"},
			{"text/html": "<i>two</i>"},
		},
		Elapsed: time.Millisecond,
	}
	got := FormatResult(res, time.Second)
	if !strings.Contains(got, "display[1]") || !strings.Contains(got, "display[2]") {
		t.Errorf("多次 display() 未逐条列出：\n%s", got)
	}
	if !strings.Contains(got, "<b>hi</b>") {
		t.Errorf("富内容未出现在输出里（退化成了 repr？）：\n%s", got)
	}
}

// TestPickResultTextSkipsDegenerateRepr 占位 repr 必须给真正的富内容让位。
//
// 实测依据：display(HTML(...)) 的 text/plain 是 `<IPython.core.display.HTML
// object>`，照"优先纯文本"的旧规矩选它，模型就只看到这一行。
// 变异测试：让 isDegenerateRepr 恒返回 false，这条会红。
func TestPickResultTextSkipsDegenerateRepr(t *testing.T) {
	cases := []struct {
		name   string
		result map[string]string
		want   string
	}{
		{"占位 repr 让位给 HTML", map[string]string{
			"text/plain": "<IPython.core.display.HTML object>",
			"text/html":  "<b>hi</b>",
		}, "<b>hi</b>"},
		{"没有 HTML 时占位 repr 也别丢", map[string]string{
			"text/plain": "<__main__.Foo object at 0x7f>",
		}, "<__main__.Foo object at 0x7f>"},
		{"多行是真内容，不让位", map[string]string{
			"text/plain": "<class 'dict'>\n{'a': 1}",
			"text/html":  "<table/>",
		}, "<class 'dict'>"},
		{"普通纯文本不受影响", map[string]string{
			"text/plain": "42",
			"text/html":  "<b>42</b>",
		}, "42"},
	}
	for _, c := range cases {
		if got := pickResultText(c.result); !strings.Contains(got, c.want) {
			t.Errorf("%s: pickResultText = %q，期望含 %q", c.name, got, c.want)
		}
	}
}

// TestHumanBytes 大小单位不能错档。
//
// 实测踩到：19139 字节被报成 "18.7 MB"（单位表比除法多走了一档）。
// 这种错日志里看不出来，却会让模型据此判断"图太大别读"。
func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{19139, "18.7 KB"},
		{2_000_000, "1.9 MB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q，期望 %q", c.n, got, c.want)
		}
	}
}

// TestArtifactDirWiring 落盘根目录必须真的用上接线进来的目录。
func TestArtifactDirWiring(t *testing.T) {
	dir := withArtifactDir(t)
	res := ExecResult{Result: map[string]string{"image/png": tinyPNGBase64}}
	saveArtifacts(&res)
	if len(res.Saved) != 1 {
		t.Fatalf("未落盘: %+v", res)
	}
	want := filepath.Join(dir, "ipython-output") + string(os.PathSeparator)
	if !strings.HasPrefix(res.Saved[0].Path, want) {
		t.Errorf("落盘位置不在接线的目录下：%s（期望前缀 %s）", res.Saved[0].Path, want)
	}
}

func TestFormatResultTruncatesHugeResult(t *testing.T) {
	big := strings.Repeat("あ", 30000) // 用多字节字符：按字节截断会切出半个码点
	res := ExecResult{Result: map[string]string{"text/plain": big}, Elapsed: time.Millisecond}
	got := FormatResult(res, time.Second)
	if len([]rune(got)) >= 30000 {
		t.Errorf("超长结果未被截断，长度=%d", len([]rune(got)))
	}
	if !strings.Contains(got, "结果截断") {
		t.Errorf("截断未说明：\n%s", got[:80])
	}
}

func TestExecRejectsEmptyCode(t *testing.T) {
	// 空代码会以无意义的 Out[N] 静默"成功"，令模型误判已执行。
	SetSessionKey("")
	orig := enabled.Swap(false)
	defer enabled.Store(orig)
	if _, err := Exec(map[string]interface{}{"code": "  "}); err == nil {
		t.Errorf("空 code 应报错")
	}
	if _, err := Exec(map[string]interface{}{}); err == nil {
		t.Errorf("缺 code 应报错")
	}
	// 未启用时的报错必须说清怎么修，而不是含糊失败。
	if _, err := Exec(map[string]interface{}{"code": "1"}); err == nil {
		t.Errorf("未启用时应报错")
	} else if !strings.Contains(err.Error(), "ipython_python") {
		t.Errorf("未启用的报错缺少可行动指引：%v", err)
	}
}

// TestResetSessionSwitchesKey 会话切换必须真的换掉隔离键。
//
// 这条守护的是 2026-10-05 查出的那个 bug：SetSessionKey 全仓无人调用，
// /reset 后模型以为从零开始，实际旧会话的变量全在内核里活着。
// 变异测试：把 ResetSession 改成空实现，这条会红。
func TestResetSessionSwitchesKey(t *testing.T) {
	defer SetSessionKey("") // 恢复全局状态，别污染同包其它测试

	SetSessionKey("s1")
	ResetSession("s1")
	if got := currentSessionKey(); got != "s1" {
		t.Errorf("同一会话重复切换应保持 key 不变（避免打断在跑的 cell），实际 %q", got)
	}

	ResetSession("s2")
	if got := currentSessionKey(); got != "s2" {
		t.Errorf("ResetSession 后隔离键应为 s2，实际 %q", got)
	}

	// 换键必须作废旧键的内核：内核池里不再有旧 key。
	for _, k := range manager.Keys() {
		if k == "s1" {
			t.Errorf("旧会话的内核未被清除：%v", manager.Keys())
		}
	}
}

// TestStripANSI 剥掉 IPython traceback 的颜色码。
//
// 实测依据：run_cell 出错时 stdout 里是 `\x1b[31m---...---\x1b[39m` 这样的
// 带色 traceback，直接喂模型是噪声还占 token。
func TestStripANSI(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"\x1b[31mValueError\x1b[39m: boom", "ValueError: boom"},
		{"\x1b[0;31mred\x1b[0m plain", "red plain"},
		{"无转义序列", "无转义序列"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripANSI(c.in); got != c.want {
			t.Errorf("stripANSI(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// 光标类序列（CSI ... 字母）也要一并剥掉。
	if got := stripANSI("a\x1b[2Kb"); got != "ab" {
		t.Errorf("光标序列未剥掉：%q", got)
	}
}

// TestSummaryLine 探测失败的错误要压成一行摘要（这段文本会显示在 WebUI 配置页）。
func TestSummaryLine(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"多行 traceback 取最后一个非空行",
			"Traceback (most recent call last):\n  File \"<string>\", line 1\nModuleNotFoundError: No module named 'IPython'\n",
			"ModuleNotFoundError: No module named 'IPython'"},
		{"尾部空行不算摘要",
			"line one\nRealError: bad\n\n  \n",
			"RealError: bad"},
		{"空输入", "", ""},
	}
	for _, c := range cases {
		if got := summaryLine(c.in); got != c.want {
			t.Errorf("%s: summaryLine(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	// 超长单行异常必须截断，否则会把配置页撑爆。
	long := strings.Repeat("x", 500)
	got := summaryLine(long)
	if len(got) > 210 {
		t.Errorf("超长未截断：len=%d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断未加省略号：%q", got[:20])
	}
}
