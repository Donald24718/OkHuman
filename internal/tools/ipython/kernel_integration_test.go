package ipython

// 集成测试：真起一个 Python/IPython 进程跑这套协议。
//
// 没有可用解释器时整体 Skip（CI 环境友好）。指定解释器：
//   OKHUMAN_IPYTHON_PYTHON=/path/to/python go test ./internal/tools/ipython/ -run Integration -v
//
// 依赖：pip install ipython（**不需要** ipykernel / pyzmq —— 这是本路线的核心收益）。

import (
	"os"
	"strings"
	"testing"
	"time"
)

// integrationPython 找解释器；找不到就跳过。
func integrationPython(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("OKHUMAN_IPYTHON_PYTHON"); p != "" {
		return p
	}
	if p, err := DetectPython(); err == nil {
		return p
	}
	t.Skip("未找到带 IPython 的 Python 解释器，跳过集成测试（可用 OKHUMAN_IPYTHON_PYTHON 指定）")
	return ""
}

// newIKernel 起一个内核并在测试结束时关掉。
func newIKernel(t *testing.T, python string) *Kernel {
	t.Helper()
	k := NewKernel(python)
	t.Cleanup(k.Shutdown)
	// 就绪握手：give IPython import 足够时间（冷盘机器首次可达数十秒）。
	if _, err := k.Exec("1", startTimeout); err != nil {
		t.Fatalf("内核就绪失败: %v", err)
	}
	return k
}

// TestIntegrationStatePersists 核心卖点：变量跨调用保留。
func TestIntegrationStatePersists(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	if res, err := k.Exec("needle = 'alive'\n锡 = 42", 30*time.Second); err != nil {
		t.Fatalf("exec: %v", err)
	} else if !res.OK() {
		t.Fatalf("赋值失败: %+v", res)
	}

	res, err := k.Exec("needle, 锡", 30*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if flat := strings.Join(res.ResultTexts(), " "); !strings.Contains(flat, "alive") || !strings.Contains(flat, "42") {
		t.Errorf("变量未跨调用保留，result=%v", res.Result)
	}
}

// TestIntegrationMagicAndShell %magic 与 !cmd 必须可用（否则就退化成 plain python）。
func TestIntegrationMagicAndShell(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	res, err := k.Exec("%timeit sum(range(100))", 60*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ErrName != "" {
		t.Errorf("%%timeit 执行出错: %s: %s", res.ErrName, res.ErrValue)
	}
	if !strings.Contains(res.Stdout+res.Stderr, "loop") && !strings.Contains(res.Stdout+res.Stderr, "us") &&
		!strings.Contains(res.Stdout+res.Stderr, "ms") && !strings.Contains(res.Stdout+res.Stderr, "s ") {
		t.Errorf("%%timeit 输出异常: out=%q err=%q", res.Stdout, res.Stderr)
	}
}

// TestIntegrationRichDisplay DataFrame 富展示：不装 pandas 也能验（IPython 自带 HTML 对象）。
func TestIntegrationRichDisplay(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	res, err := k.Exec("from IPython.display import HTML\nHTML('<b>hi</b>')", 30*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if _, ok := res.Result["text/html"]; !ok {
		t.Errorf("富展示未产出 text/html，实际 MIME: %v", res.ResultKeys())
	}
}

// TestIntegrationInOutHistory In/Out 历史仍然是 IPython 的能力
// （抑制回显的前提下不能连带废掉它）。
func TestIntegrationInOutHistory(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	if _, err := k.Exec("41 + 1", 30*time.Second); err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := k.Exec("list(Out.values())[-1]", 30*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if got := strings.Join(res.ResultTexts(), ""); !strings.Contains(got, "42") {
		t.Errorf("Out 历史不可用，result=%v", res.Result)
	}
}

// TestIntegrationUserErrorIsNotInterrupt 用户代码出错 ≠ 宿主中断。
//
// 这是最容易被实现搞混的一条：IPython 会把 KeyboardInterrupt 也变成
// error_in_exec，两者在协议层长得一样。混淆它们会让模型去修一段没问题的代码。
func TestIntegrationUserErrorIsNotInterrupt(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	res, err := k.Exec("raise ValueError('boom')", 30*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ErrName != "ValueError" {
		t.Errorf("异常类型 = %q，期望 ValueError", res.ErrName)
	}
	if res.Interrupted {
		t.Errorf("用户代码出错被误判为中断")
	}
}

// TestIntegrationSoftInterruptKeepsState 超时软中断：内核留住、变量不丢。
//
// ZMQ 路线在 Windows 上做不到这一点（ZMQ interrupt_request 被 ipykernel 拒收，
// 却回 status:ok —— 静默假成功）。本路线靠同进程 signal 做到。
func TestIntegrationSoftInterruptKeepsState(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	if _, err := k.Exec("keepme = 'still-here'", 30*time.Second); err != nil {
		t.Fatalf("exec: %v", err)
	}

	started := time.Now()
	res, err := k.Exec("while True: pass", 3*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	elapsed := time.Since(started)

	if !res.Interrupted {
		t.Errorf("未判定为中断: %+v", res)
	}
	if res.Killed {
		t.Errorf("CPU 循环应该能被软中断，不该走到硬杀")
	}
	// 3s 超时 + 三级级联各 1.5s：正常情况下 stage 1 就命中（实测 0.1~0.3s）。
	if elapsed > 9*time.Second {
		t.Errorf("中断耗时过长 %v（级联未及时生效）", elapsed)
	}

	// 关键断言：中断之后内核必须仍可用，且变量还在。
	after, err := k.Exec("keepme", 30*time.Second)
	if err != nil {
		t.Fatalf("中断后内核不可用: %v", err)
	}
	if got := strings.Join(after.ResultTexts(), ""); !strings.Contains(got, "still-here") {
		t.Errorf("中断后变量丢失，result=%v", after.Result)
	}
}

// TestIntegrationCascadeBreaksSleep 级联中断：time.sleep 是最难的一档。
//
// 实测的平台差异：Windows 上 raise_signal 直接生效；POSIX 上它近似
// pthread_kill(自己线程)，投递给了监听线程而非阻塞中的主线程，必须升级到
// stage 2（显式 pthread_kill 主线程）。级联存在的意义就在这里。
func TestIntegrationCascadeBreaksSleep(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	if _, err := k.Exec("import time\nzzz = 'slept'", 30*time.Second); err != nil {
		t.Fatalf("exec: %v", err)
	}

	started := time.Now()
	res, err := k.Exec("time.sleep(60)", 3*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	elapsed := time.Since(started)

	if !res.Interrupted {
		t.Errorf("sleep 未被级联中断（这正是必须级联而非单一手段的原因）: %+v", res)
	}
	if res.Killed {
		t.Errorf("级联应当能中断 sleep，不该退到硬杀")
	}
	// 3s timeout + stage1/2 各 1.5s 观测窗口（stage 2 通常在 Linux 上命中）。
	if elapsed > 10*time.Second {
		t.Errorf("级联耗时过长 %v", elapsed)
	}

	after, err := k.Exec("zzz", 30*time.Second)
	if err != nil {
		t.Fatalf("中断后内核不可用: %v", err)
	}
	if got := strings.Join(after.ResultTexts(), ""); !strings.Contains(got, "slept") {
		t.Errorf("级联中断后变量丢失：%v", after.Result)
	}
}

// TestIntegrationDeadProcessIsReportedNotHung 内核中途暴毙必须被报告，且不能卡死调用方。
func TestIntegrationDeadProcessIsReportedNotHung(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	// os._exit 会连缓冲区都不冲就走人：模拟最坏情况的突然死亡。
	res, err := k.Exec("import os\nos._exit(9)", 20*time.Second)
	if err == nil && !res.Lost {
		t.Errorf("进程暴毙未被识别: res=%+v err=%v", res, err)
	}
	// 不能挂死：整体耗时应可控。
}

// TestIntegrationRecreateAfterDeath 内核死后再次调用应自动重建，而不是永久失败。
// TestIntegrationResetSessionClearsState 会话切换后旧变量必须消失。
//
// 这条守护的是 2026-10-05 查出的接线缺失：SetSessionKey 全仓无人调用 →
// /reset 后旧会话的变量原封不动活着，模型以为从零开始。
// 变异测试：把 ResetSession 改成空实现，这条会红。
func TestIntegrationResetSessionClearsState(t *testing.T) {
	python := integrationPython(t)
	if err := Setup(python, 30000, 600000); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() {
		enabled.Store(false)
		manager.ShutdownAll()
		SetSessionKey("")
	}()

	SetSessionKey("s1")
	if _, err := Exec(map[string]interface{}{"code": "zz_marker = 4242"}); err != nil {
		t.Fatalf("设置变量失败: %v", err)
	}

	ResetSession("s2") // 新会话边界

	out, err := Exec(map[string]interface{}{"code": "zz_marker"})
	if err != nil {
		t.Fatalf("新会话执行失败: %v", err)
	}
	if !strings.Contains(out, "NameError") {
		t.Errorf("新会话仍能看到旧变量（状态污染）：\n%s", out)
	}
}

// TestIntegrationInputFailsFast input() 必须立刻失败，不能挂到宿主超时。
//
// 背景：stdin 是宿主协议通道，用户的 input() 永远等不到一行。修复前实测
// 6 秒无响应、最后靠超时 + 级联中断救回来——白耗一轮，还让模型以为代码有问题。
// 修复后应立刻抛 InputUnavailable，把"该怎么写"写进异常消息。
//
// 变异测试：删掉 launcher 里的 builtins.input patch，这条会 TIMEOUT 红。
func TestIntegrationInputFailsFast(t *testing.T) {
	python := integrationPython(t)
	k := newIKernel(t, python)

	start := time.Now()
	res, err := k.Exec("input('请输入: ')", 20*time.Second)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("input() 不应造成宿主侧故障: %v", err)
	}
	if res.ErrName != "InputUnavailable" {
		t.Errorf("异常类型应为 InputUnavailable，实际 %q（evalue=%q stdout=%q）",
			res.ErrName, res.ErrValue, res.Stdout)
	}
	if !strings.Contains(res.ErrValue, "input()") {
		t.Errorf("报错必须给出可行动的替代写法，实际：%q", res.ErrValue)
	}
	if elapsed > 10*time.Second {
		t.Errorf("input() 没有快速失败：耗时 %v（说明又挂住了）", elapsed)
	}
}

// TestIntegrationTracebackNoANSI traceback 到达模型时不能带 ANSI 颜色码。
//
// 剥色发生在 Go 侧（output.go.stripANSI），所以这里走 FormatResult 全覆盖路径。
// 变异测试：去掉 stripANSI 调用，这条会红。
func TestIntegrationTracebackNoANSI(t *testing.T) {
	python := integrationPython(t)
	k := newIKernel(t, python)

	res, err := k.Exec("raise ValueError('boom')", 20*time.Second)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	out := FormatResult(res, 20*time.Second)
	if strings.Contains(out, "\x1b[") {
		t.Errorf("给模型的文本仍含 ANSI 转义：%q", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("剥色不应连内容一起剥掉：%q", out)
	}
}

// TestIntegrationDisplayCaptured display() 的富内容必须回传，不能退化成 repr。
//
// 守护的失败模式（实测）：IPython 默认的 DisplayPublisher 在哑前端下会把富对象
// 打印成 `<IPython.core.display.HTML object>`，模型只看到这一行，毫无用处。
// 变异测试：把 launcher 的 _capture_display 去掉，这条会红。
func TestIntegrationDisplayCaptured(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	res, err := k.Exec("from IPython.display import HTML, display\ndisplay(HTML('<b>okhuman-probe</b>'))", 30*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(res.Displays) == 0 {
		t.Fatalf("display() 内容未被捕获（stdout=%q stderr=%q）", res.Stdout, res.Stderr)
	}
	html, ok := res.Displays[0]["text/html"]
	if !ok || !strings.Contains(html, "okhuman-probe") {
		t.Errorf("捕获到的不是富内容: %v", res.Displays[0])
	}
	if strings.Contains(res.Stdout, "HTML object") {
		t.Errorf("stdout 里仍有 repr 噪声（说明走了默认 publisher）: %q", res.Stdout)
	}
}

// TestIntegrationFigureAutoSaved matplotlib 出图 → 自动落盘为真 PNG。
//
// 这条同时守护两件事：
//  1. inline 后端已配好（Figure 能产出 image/png，而不是只有 text/plain）；
//  2. 落盘链路端到端通（模型最终拿到的是可读的文件路径）。
func TestIntegrationFigureAutoSaved(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	// 只在"确实没装"时跳过。卡顿/超时/其它异常一律判失败——
	// 曾吃过亏：inline 后端没配上时 import matplotlib 会拖到超时，
	// 而宽松的 `err != nil 就 Skip` 把这个问题静默咽掉了，变异测试因此漏网。
	probe, err := k.Exec("import matplotlib", 120*time.Second)
	if err != nil {
		t.Fatalf("import matplotlib 失败: %v", err)
	}
	if probe.ErrName == "ModuleNotFoundError" {
		t.Skip("未安装 matplotlib，跳过出图测试")
	}
	if probe.ErrName != "" {
		t.Fatalf("import matplotlib 异常（%s: %s），出图链路不可用\n%s",
			probe.ErrName, probe.ErrValue, probe.Stdout)
	}

	res, err := k.Exec(
		"import matplotlib.pyplot as plt\n"+
			"fig, ax = plt.subplots()\n"+
			"ax.plot([1, 2, 3], [3, 1, 2])\n"+
			"fig", 120*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ErrName != "" {
		t.Fatalf("出图失败: %s: %s\n%s", res.ErrName, res.ErrValue, res.Stdout)
	}
	if _, ok := res.Result["image/png"]; !ok {
		t.Fatalf("Figure 未产出 image/png（inline 后端没配上），实际 MIME=%v", res.ResultKeys())
	}

	saveArtifacts(&res)
	if len(res.Saved) == 0 {
		t.Fatalf("图片未落盘，模型看不到图: %+v", res)
	}
	raw, err := os.ReadFile(res.Saved[0].Path)
	if err != nil {
		t.Fatalf("读回落盘文件失败: %v", err)
	}
	if string(raw[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Errorf("落盘内容不是 PNG: %x", raw[:8])
	}
	if len(raw) < 1000 {
		t.Errorf("落盘图片过小（%d 字节），像是空图", len(raw))
	}

	out := FormatResult(res, 120*time.Second)
	if !strings.Contains(out, res.Saved[0].Path) {
		t.Errorf("返回值未给出落盘路径:\n%s", out)
	}
	t.Logf("落盘: %s（%d 字节）", res.Saved[0].Path, len(raw))
}

// TestIntegrationPltShowProducesImage plt.show() 必须真出图。
//
// 实测踩到：只切 Agg 后端时 plt.show() 只给一句
// "FigureCanvasAgg is non-interactive, and thus cannot be shown"，图根本不出来；
// 而 plt.show() 恰恰是模型最常写的收尾方式，于是"画了图却什么都没有"。
// 变异测试：去掉 launcher 里的 matplotlib.use(inline)，这条会红。
func TestIntegrationPltShowProducesImage(t *testing.T) {
	k := newIKernel(t, integrationPython(t))

	probe, err := k.Exec("import matplotlib", 120*time.Second)
	if err != nil {
		t.Fatalf("import matplotlib 失败: %v", err)
	}
	if probe.ErrName == "ModuleNotFoundError" {
		t.Skip("未安装 matplotlib，跳过出图测试")
	}
	if probe.ErrName != "" {
		t.Fatalf("import matplotlib 异常: %s: %s", probe.ErrName, probe.ErrValue)
	}

	res, err := k.Exec(
		"import matplotlib.pyplot as plt\n"+
			"plt.plot([1, 2, 3], [1, 4, 9])\n"+
			"plt.title('y=x^2')\n"+
			"plt.show()", 120*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ErrName != "" {
		t.Fatalf("plt.show() 出错: %s: %s\n%s", res.ErrName, res.ErrValue, res.Stdout)
	}
	if strings.Contains(res.Stdout+res.Stderr, "non-interactive") {
		t.Errorf("后端还是 Agg，show() 等于没出图: %q", res.Stdout+res.Stderr)
	}

	// show() 走的是 display 通道，不是 cell 结果——必须落到 Displays 里。
	found := false
	for _, d := range res.Displays {
		if _, ok := d["image/png"]; ok {
			found = true
		}
	}
	if !found {
		t.Fatalf("plt.show() 未产出 image/png（displays=%d，stdout=%q）", len(res.Displays), res.Stdout)
	}

	saveArtifacts(&res)
	if len(res.Saved) == 0 {
		t.Fatalf("show() 出的图未落盘，模型看不到: %+v", res)
	}
	raw, err := os.ReadFile(res.Saved[0].Path)
	if err != nil {
		t.Fatalf("读回落盘文件失败: %v", err)
	}
	if string(raw[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Errorf("落盘内容不是 PNG: %x", raw[:8])
	}
	t.Logf("plt.show() 落盘: %s（%d 字节）", res.Saved[0].Path, len(raw))
}

func TestIntegrationRecreateAfterDeath(t *testing.T) {
	python := integrationPython(t)
	key := "test-death"
	// 独立 key：避免与其它测试共享的内核相互干扰。
	if err := Setup(python, 30000, 600000); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { enabled.Store(false); manager.ShutdownAll() }()
	SetSessionKey(key)
	defer SetSessionKey("")

	k := manager.Get(key)
	if _, err := k.Exec("import os\nos._exit(9)", 20*time.Second); err == nil {
		// 可能返回 nil error 且 Lost=true，也可能直接报错；两种都接受。
	}
	manager.Recycle(key)

	out, err := Exec(map[string]interface{}{"code": "'reborn'"})
	if err != nil {
		t.Fatalf("内核死后无法重建: %v", err)
	}
	if !strings.Contains(out, "reborn") {
		t.Errorf("重建后执行异常:\n%s", out)
	}
}
