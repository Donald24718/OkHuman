package tools

// 阶段 2：executor 接口的确定性单测（**平台无关**，三平台都能跑）。
//
// 为什么必须平台无关：Windows 上无法执行真实 bash（阶段 3 才实现），
// 但接口契约、判定分支、降级文案这些**逻辑**必须能在任何平台被验证。
// 这正是"抽接口"的核心收益——把可测逻辑从平台实现里解放出来。

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
)

// fakeExecutor 预设结果的假执行域，用于确定性覆盖 bash.go 的判定与文案分支。
type fakeExecutor struct {
	info     ExecInfo
	err      error
	gotPath  string // 记录收到的 scriptPath（验证传递正确）
	gotTO    int    // 记录收到的 timeoutSec
	wroteOut string // 非空则模拟向 stdout 写入
	wroteErr string // 非空则模拟向 stderr 写入
}

func (f *fakeExecutor) Run(scriptPath string, timeoutSec int, stdout, stderr io.Writer) (ExecInfo, error) {
	f.gotPath = scriptPath
	f.gotTO = timeoutSec
	if f.wroteOut != "" {
		io.WriteString(stdout, f.wroteOut)
	}
	if f.wroteErr != "" {
		io.WriteString(stderr, f.wroteErr)
	}
	return f.info, f.err
}

// withFakeExecutor 在测试期间注入 fakeExecutor，结束自动还原。
func withFakeExecutor(t *testing.T, f *fakeExecutor) {
	t.Helper()
	var e executor = f
	executorOverride.Store(&e)
	t.Cleanup(func() { executorOverride.Store(nil) })
}

// TestBashTimedOutTextFromDoubleCondition 验证：超时文案由 **Killed && Signaled**
// 双条件决定（权威口径），而非平台自报的 Reason。
//
// 这是阶段 2 的关键回归：曾一度把判定写成 `info.Reason == ExitTimedOut`，
// 等价于让平台自报取代双条件 → 退化。本测试锁死"双条件为准"。
func TestBashTimedOutTextFromDoubleCondition(t *testing.T) {
	cases := []struct {
		name   string
		info   ExecInfo
		wantTO bool // 期望输出含超时文案
		why    string
	}{
		{
			name:   "真超时（killed+signaled，reason 一致）",
			info:   ExecInfo{Reason: ExitTimedOut, Code: -1, Killed: true, Signaled: true},
			wantTO: true,
			why:    "两条件成立 → 加超时文案",
		},
		{
			name:   "僵尸组假成功（killed 但未 signaled）",
			info:   ExecInfo{Reason: ExitTimedOut, Code: 0, Killed: true, Signaled: false},
			wantTO: false,
			why:    "平台自报 timedout，但双条件否定 → 不加（且触发 reason-mismatch）",
		},
		{
			name:   "命令自杀（signaled 但未 killed）",
			info:   ExecInfo{Reason: ExitSignaled, Code: -1, Killed: false, Signaled: true},
			wantTO: false,
			why:    "命令自己 kill -9 $$ → 非本工具超时",
		},
		{
			name:   "正常退出",
			info:   ExecInfo{Reason: ExitNormal, Code: 0},
			wantTO: false,
			why:    "无条件成立 → 不加",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withFakeExecutor(t, &fakeExecutor{info: c.info, wroteOut: "OUT"})
			out, err := ExecuteTool("bash", map[string]interface{}{"command": "ignored"})
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			gotTO := strings.Contains(out, "上限被强制终止")
			if gotTO != c.wantTO {
				t.Errorf("超时文案 = %v, want %v（%s）\n输出：%s", gotTO, c.wantTO, c.why, out)
			}
			// 交叉校验：Reason 与双条件不一致时必须出现 reason-mismatch 降级说明
			mismatch := (c.info.Reason == ExitTimedOut) != c.wantTO
			if mismatch && !strings.Contains(out, "reason-mismatch") {
				t.Errorf("Reason 与双条件不一致却无 reason-mismatch 提示：%s", out)
			}
			if !mismatch && strings.Contains(out, "reason-mismatch") {
				t.Errorf("一致时不应出现 reason-mismatch：%s", out)
			}
		})
	}
}

// TestBashDegradedSurfacedToModel 验证"不静默降级"（方案 §3.2）：
// 平台降级说明必须出现在**模型可见的返回值**里。
func TestBashDegradedSurfacedToModel(t *testing.T) {
	withFakeExecutor(t, &fakeExecutor{
		info:     ExecInfo{Reason: ExitNormal, Code: 0, Degraded: "job-unavailable: 父 Job 不允许 breakaway"},
		wroteOut: "hello",
	})
	out, err := ExecuteTool("bash", map[string]interface{}{"command": "ignored"})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if !strings.Contains(out, "[平台降级]") || !strings.Contains(out, "job-unavailable") {
		t.Errorf("降级说明未透出到模型可见输出：%s", out)
	}
	// 降级不影响退出码头部（仍是正常退出）
	if !strings.HasPrefix(out, "退出码: 0") {
		t.Errorf("降级不应改变退出码头部：%s", out)
	}
}

// TestBashStartErrReturnsError 验证启动失败路径：startErr != nil → ExecuteTool 返回 error
// （与原 `if err := cmd.Start(); err != nil { return "", err }` 语义一致）。
func TestBashStartErrReturnsError(t *testing.T) {
	withFakeExecutor(t, &fakeExecutor{err: errors.New("boom: exec not found")})
	out, err := ExecuteTool("bash", map[string]interface{}{"command": "ignored"})
	if err == nil {
		t.Fatal("启动失败应返回 error")
	}
	if out != "" {
		t.Errorf("失败路径输出应为空串（返回契约）：%q", out)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("错误应透传平台错误：%v", err)
	}
}

// TestBashPassesScriptPathAndTimeout 验证透传：scriptPath 与 timeoutSec 正确传给平台层。
func TestBashPassesScriptPathAndTimeout(t *testing.T) {
	f := &fakeExecutor{info: ExecInfo{Reason: ExitNormal, Code: 0}}
	withFakeExecutor(t, f)
	Configure(30000, 5000)
	if _, err := ExecuteTool("bash", map[string]interface{}{"command": "echo hi"}); err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if f.gotPath == "" || !strings.HasSuffix(f.gotPath, ".sh") {
		t.Errorf("scriptPath 未正确传递：%q", f.gotPath)
	}
	if f.gotTO != 5 {
		t.Errorf("timeoutSec = %d, want 5（应由配置/参数推导）", f.gotTO)
	}
}

// TestBashStderrSection 验证 [stderr] 分段仍在（拆分后输出格式未变）。
func TestBashStderrSection(t *testing.T) {
	withFakeExecutor(t, &fakeExecutor{
		info:     ExecInfo{Reason: ExitNormal, Code: 2},
		wroteOut: "to-stdout",
		wroteErr: "to-stderr",
	})
	out, err := ExecuteTool("bash", map[string]interface{}{"command": "ignored"})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if !strings.Contains(out, "退出码: 2") {
		t.Errorf("退出码头缺失：%s", out)
	}
	if !strings.Contains(out, "[stderr]\nto-stderr") {
		t.Errorf("stderr 分段格式变了：%s", out)
	}
	if !strings.Contains(out, "to-stdout") {
		t.Errorf("stdout 内容缺失：%s", out)
	}
}

// TestJoinDegraded 纯函数覆盖。
func TestJoinDegraded(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "x", "x"},
		{"x", "", "x"},
		{"", "", ""},
		{"x", "y", "x; y"},
	}
	for _, c := range cases {
		if got := joinDegraded(c.a, c.b); got != c.want {
			t.Errorf("joinDegraded(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// TestExitReasonString 覆盖 String()（日志可读性，不参与判定）。
func TestExitReasonString(t *testing.T) {
	cases := map[ExitReason]string{
		ExitNormal:     "normal",
		ExitSignaled:   "signaled",
		ExitTimedOut:   "timedout",
		ExitReason(99): "unknown",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("ExitReason(%d).String() = %q, want %q", r, got, want)
		}
	}
}

// TestDetachHintPlatformCorrect 锁死「脱离指引与平台一致」（阶段 4）。
//
// 为什么必须测：这段文本**进入模型可见的工具描述**。若在 Windows 上仍教 `setsid`
// （实测 Git Bash 里 `setsid: command not found`），模型会照跑一条必然失败的命令；
// 反之若在 Unix 上教 Windows 的启动方式，也误导。故断言：
//   - 当前平台的指引**必须**出现；
//   - 另一平台的指令**不得**出现（防复制粘贴串台）。
func TestDetachHintPlatformCorrect(t *testing.T) {
	Configure(30000, 600000)
	desc := Specs()[0].Description

	if runtime.GOOS == "windows" {
		if strings.Contains(desc, "setsid") {
			t.Errorf("Windows 工具描述不应出现 setsid（Git Bash 无此命令）:\n%s", desc)
		}
		if !strings.Contains(desc, "Job") {
			t.Errorf("Windows 工具描述应说明 Job 容器语义:\n%s", desc)
		}
	} else {
		if !strings.Contains(desc, "setsid") {
			t.Errorf("Unix 工具描述应保留 setsid 三件套:\n%s", desc)
		}
		if strings.Contains(desc, "Job 容器") {
			t.Errorf("Unix 工具描述不应出现 Windows 的 Job 容器文案:\n%s", desc)
		}
	}

	// 两平台的 brief 文案都必须非空（进入超时自述，缺失会让模型无从下手）。
	if detachHintBrief() == "" {
		t.Error("detachHintBrief 不得为空")
	}
	// 描述必须含「前台等待」「总时长超」两个既有语义（防 refactor 误删）。
	for _, must := range []string{"前台等待", "命令总时长超"} {
		if !strings.Contains(desc, must) {
			t.Errorf("工具描述缺关键语义 %q:\n%s", must, desc)
		}
	}
}
