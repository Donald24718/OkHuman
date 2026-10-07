//go:build windows

package tools

// Windows 执行器端到端集成测试：用**真实 Git Bash** 走 defaultExecutor（Job 三级策略）。
//
// 与 executor_test.go 的分工：
//   - executor_test.go：平台无关，用 fakeExecutor 锁死**双条件判定**逻辑（无需 bash）；
//   - 本文件：平台相关，用真实 bash 验证**执行域本身**（启动、管道、超时杀整树、
//     路径含空格、环境注入、大输出不死锁）。
//
// 需要本机有可用 bash（Git for Windows / WSL）。无 bash 时 findBash 返回错误，各用例
// 会以 t.Fatalf 失败——这是**正确**的：Windows 验收本就要求 bash 在场（AGENTS.md）。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runReal 用 defaultExecutor 跑一段脚本，返回执行信息与两路输出。
func runReal(t *testing.T, script string, timeoutSec int) (ExecInfo, string, string) {
	t.Helper()
	dir := t.TempDir()
	sp := filepath.Join(dir, "s.sh")
	if err := os.WriteFile(sp, []byte(script), 0o644); err != nil {
		t.Fatalf("写脚本失败: %v", err)
	}
	var out, errb bytes.Buffer
	info, err := (defaultExecutor{}).Run(sp, timeoutSec, &out, &errb)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	return info, out.String(), errb.String()
}

func TestE2E_BasicEcho(t *testing.T) {
	bp, err := findBash()
	if err != nil {
		t.Fatalf("findBash: %v", err)
	}
	t.Logf("bash 路径: %s", bp)
	info, out, errs := runReal(t, "#!/bin/bash\necho hello-okhuman\necho err-line 1>&2\nexit 0\n", 10)
	t.Logf("info=%+v out=%q err=%q", info, out, errs)
	if !strings.Contains(out, "hello-okhuman") {
		t.Errorf("stdout 缺 hello-okhuman: %q", out)
	}
	if !strings.Contains(errs, "err-line") {
		t.Errorf("stderr 缺 err-line: %q", errs)
	}
	if info.Code != 0 {
		t.Errorf("期望退出码 0，得 %d", info.Code)
	}
	if info.Killed || info.Signaled {
		t.Errorf("不应被杀/异常: %+v", info)
	}
}

func TestE2E_ExitCode(t *testing.T) {
	info, _, _ := runReal(t, "#!/bin/bash\nexit 42\n", 10)
	if info.Code != 42 {
		t.Errorf("期望退出码 42，得 %d", info.Code)
	}
}

func TestE2E_TimeoutKillTree(t *testing.T) {
	// 启动一个后台孙进程（sleep），然后主进程挂起 → 超时应杀整树。
	info, _, _ := runReal(t, "#!/bin/bash\nsleep 300 &\nsleep 300\n", 2)
	t.Logf("info=%+v", info)
	if !info.Killed || !info.Signaled {
		t.Errorf("超时应 killed+signaled: %+v", info)
	}
	if info.Reason != ExitTimedOut {
		t.Errorf("期望 ExitTimedOut，得 %s", info.Reason)
	}
}

func TestE2E_PathWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "has space dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(dir, "my script.sh")
	if err := os.WriteFile(sp, []byte("#!/bin/bash\necho spaced-ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	info, err := (defaultExecutor{}).Run(sp, 10, &out, &errb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("info=%+v out=%q err=%q", info, out.String(), errb.String())
	if !strings.Contains(out.String(), "spaced-ok") {
		t.Errorf("含空格路径失败: out=%q err=%q", out.String(), errb.String())
	}
}

func TestE2E_NoPathConvEnv(t *testing.T) {
	// 验证 MSYS_NO_PATHCONV 确实注入。
	_, out, _ := runReal(t, "#!/bin/bash\necho \"NPC=[$MSYS_NO_PATHCONV]\"\necho \"EXCL=[$MSYS2_ARG_CONV_EXCL]\"\n", 10)
	t.Logf("out=%q", out)
	if !strings.Contains(out, "NPC=[1]") {
		t.Errorf("MSYS_NO_PATHCONV 未注入: %q", out)
	}
	if !strings.Contains(out, "EXCL=[*]") {
		t.Errorf("MSYS2_ARG_CONV_EXCL 未注入: %q", out)
	}
}

func TestE2E_SurvivesManyCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过重复调用压力测试")
	}
	// 连续 30 次调用，确认无崩溃/无退化（句柄泄漏由外部工具观测，见方案 §11.2）。
	for i := 0; i < 30; i++ {
		info, out, _ := runReal(t, "#!/bin/bash\necho x\n", 10)
		if info.Code != 0 || !strings.Contains(out, "x") {
			t.Fatalf("第 %d 次调用异常: info=%+v out=%q", i, info, out)
		}
	}
}

func TestE2E_LargeOutput(t *testing.T) {
	// 大量输出（>1MB）验证管道不死锁。
	info, out, _ := runReal(t, "#!/bin/bash\nfor i in $(seq 1 20000); do echo \"line $i 0123456789012345678901234567890123456789\"; done\n", 30)
	t.Logf("info=%+v len(out)=%d", info, len(out))
	if len(out) < 500000 {
		t.Errorf("输出过短，疑似管道死锁: %d", len(out))
	}
	if info.Code != 0 {
		t.Errorf("退出码 %d", info.Code)
	}
}

// ============================================================================
// 缺陷回归守卫（2026-10-04 bug 猎捕新增）
// 以下三个用例锁定本轮修复的两个真实缺陷，防止复发。
// ============================================================================

// TestE2E_BackgroundTaskOutputNotLost 缺陷守卫①：后台任务输出不得丢失。
//
// 缺陷（修复前实测）：`wait()` 返回（顶层退出）后立即 `CloseHandle(outR)`，
// 而后台任务仍持有写端 → copyHandleTo 的 ReadFile 提前报错返回 → 输出丢失。
// 实测：100 行只收到 1 行（丢失 99）。
//
// 根因是**关闭顺序颠倒**：必须先 `wg.Wait()`（读到 EOF）再关读端。
// 本用例若失败，说明"先关读端再 wg.Wait"的老写法被恢复。
//
// 注意：脚本顶层**等待后台任务**（`wait`），使写端在顶层退出时已全释放——
// 这规避了"顶层退出但后台仍活"的挂起（那是 Unix 同源的已知行为，另见下）。
func TestE2E_BackgroundTaskOutputNotLost(t *testing.T) {
	// 关键设计：**顶层立即退出，后台任务在退出后仍写一小会儿**（共约 0.25s）。
	// 这个"窄窗口"才能同时满足两个条件：
	//   (a) 触发缺陷——顶层已退，后台仍持写端（老代码会在此刻关读端丢数据）；
	//   (b) 不长时间挂起——后台任务很快自行结束，写端随后释放。
	// 若用 `wait` 让顶层等后台，缺陷窗口被抹掉，测试会**假绿**（已实测：变异不报红）。
	// 用 `sleep 0.1` 制造"顶层退出后仍有写端存活"的窗口，总时长约 0.2s
	// （MSYS 的 sleep 精度约 15ms，故不用更小值；也避免拖慢 go test ./...）。
	script := "#!/bin/bash\n" +
		"for i in $(seq 1 50); do echo bg-$i; done &\n" +
		"( sleep 0.1; for i in $(seq 51 100); do echo bg-$i; done ) &\n" +
		"exit 0\n"
	info, out, _ := runReal(t, script, 20)
	t.Logf("info=%+v len(out)=%d", info, len(out))
	lines := strings.Count(out, "\n")
	if lines != 100 {
		t.Errorf("后台任务输出丢失：期望 100 行，实收 %d 行（关闭顺序缺陷复发？）", lines)
	}
	if !strings.Contains(out, "bg-100\n") {
		t.Errorf("末行 bg-100 缺失，输出尾部被截断")
	}
}

// TestE2E_LargeOutputExactIntegrity 缺陷守卫②：大输出必须"逐字节完整"，不只是"够长"。
//
// 与既有 TestE2E_LargeOutput（只查 `len(out) < 500000`）互补：那个是"不死锁"的弱断言，
// 本用例是"不丢字节"的强断言——首行/末行/总行数都要精确。
func TestE2E_LargeOutputExactIntegrity(t *testing.T) {
	const n = 10000
	script := "#!/bin/bash\n" +
		"for i in $(seq 1 " + itoaTest(n) + "); do printf 'L%05d-abcdefghijklmnopqrstuvwxyz0123456789\\n' $i; done\n"
	info, out, _ := runReal(t, script, 30)
	wantLines := n
	gotLines := strings.Count(out, "\n")
	t.Logf("info=%+v len(out)=%d wantLines=%d gotLines=%d", info, len(out), wantLines, gotLines)
	if gotLines != wantLines {
		t.Errorf("大输出行数不精确：期望 %d，实收 %d（丢字节）", wantLines, gotLines)
	}
	if !strings.HasPrefix(out, "L00001-") {
		t.Errorf("首行损坏: %q", headOfTest(out, 60))
	}
	if !strings.HasSuffix(out, "L10000-abcdefghijklmnopqrstuvwxyz0123456789\n") {
		t.Errorf("末行损坏: %q", tailOfTest(out, 60))
	}
}

// TestE2E_NonBMPEnvNotCorrupted 缺陷守卫③：非 BMP 字符的环境变量不得损坏。
//
// 缺陷（修复前实测）：buildWindowsEnvBlock 用 `uint16(r)` 逐 rune 强转，
// 未做 UTF-16 代理对编码：
//   - emoji U+1F33F → 收到 \uf33f（高代理位丢失，静默乱码）；
//   - CJK 扩展 B U+20000 → uint16(0x20000)=0，注入裸 NUL →
//     CreateProcess 报 "The parameter is incorrect."（**进程直接起不来**）。
//
// 修法：改用 utf16.Encode([]rune(s))。本用例若失败，说明该修法被回退。
func TestE2E_NonBMPEnvNotCorrupted(t *testing.T) {
	cases := []struct{ name, value string }{
		{"ASCII", "hello"},
		{"BMP中文", "中文测试"},
		{"emoji", "🌿🍀"},
		{"CJK扩展B", "𠀀𠀁"},
		{"混合", "a🌿中文𠀀b"},
	}
	for _, c := range cases {
		env := setEnvVar([]string{"PATH=C://Windows", "SystemRoot=C://Windows"}, "OKHUMAN_PROBE", c.value)
		dir := t.TempDir()
		sp := filepath.Join(dir, "s.sh")
		if err := os.WriteFile(sp, []byte("#!/bin/bash\nprintf '%s' \"$OKHUMAN_PROBE\"\n"), 0o644); err != nil {
			t.Fatalf("写脚本失败: %v", err)
		}
		var out, errb bytes.Buffer
		if _, err := (defaultExecutor{}).RunWithEnv(sp, 10, &out, &errb, env); err != nil {
			t.Errorf("[%s] RunWithEnv 失败（非 BMP 环境块导致 CreateProcess 拒绝？）: %v", c.name, err)
			continue
		}
		if out.String() != c.value {
			t.Errorf("[%s] 环境变量往返损坏: want=%q got=%q", c.name, c.value, out.String())
		}
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func headOfTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tailOfTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
