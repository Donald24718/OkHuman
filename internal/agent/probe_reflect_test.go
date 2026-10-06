package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"okhuman/internal/llm"
	"okhuman/internal/types"
)

// 本文件是 2026-10-06 二次反思（第一性原理）时的探针/回归测试。
// 不是照审查报告打勾，而是先问"这个参数取这个值时，代码的语义应该是什么"。

// TestProbeDoomWarnAfterSemantics 实证 doom.warn_after 各取值的语义。
// 第一性原理：isSameRunLocked(n) 问的是"最近 n 次调用是否完全相同"。
//   - n <= 0：序列长度无意义，语义上等于"不检测"（且 n=0 时 tail 为空切片 → panic 风险）
//   - n == 1：单元素序列恒等于自身 → 恒 true → 【第一次工具调用就被警告】← 语义灾难
//   - n >= 2：正常
//
// 结论：n < 2 一律判"不是"，且不要把 0 悄悄改成 3（用户写 0 是想关掉检测）。
func TestProbeDoomWarnAfterSemantics(t *testing.T) {
	cases := []struct {
		n         int
		wantWarn  bool
		wantPanic bool
		why       string
	}{
		{n: 0, wantWarn: false, why: "0 = 关闭检测（用户显式意图），不得偷偷改成 3"},
		{n: 1, wantWarn: false, why: "1 = 单元素序列恒同 → 不得在第一次调用就警告"},
		{n: 2, wantWarn: true, why: "2 = 连续两次相同才警告，最小有意义值"},
	}
	for _, c := range cases {
		var got []string
		// n=2 需要两次相同调用才可能触发；n<=1 只需一次（正是要验证"一次就警告"是错的）
		steps := []llm.FakeStep{
			{Type: "tool", Name: "bash", Args: map[string]interface{}{"command": "ls"}},
		}
		if c.n >= 2 {
			steps = append(steps,
				llm.FakeStep{Type: "tool", Name: "bash", Args: map[string]interface{}{"command": "ls"}},
			)
		}
		steps = append(steps, llm.FakeStep{Type: "text", Text: "完成"})
		a, _ := newTestAgent(steps)
		opt := testRunOpt(nil)
		opt.DoomWarnAfter = c.n
		opt.OnEvent = func(e types.AgentEvent) {
			if e["type"] == "doom_warn" {
				got = append(got, "doom_warn")
			}
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					if !c.wantPanic {
						t.Errorf("DoomWarnAfter=%d 不应 panic，实际 panic: %v", c.n, rec)
					}
				}
			}()
			_, _ = a.Run(context.Background(), "跑一下", opt)
		}()
		warned := len(got) > 0
		if warned != c.wantWarn {
			t.Errorf("DoomWarnAfter=%d：警告=%v，期望 %v（%s）", c.n, warned, c.wantWarn, c.why)
		}
	}
}

// TestProbeDataDirEmpty 实证 DataDir 为空时的落盘行为。
// 第一性原理：DataDir 是"落盘根目录"。它为空意味着"没有可落盘的位置"，
// 正确反应是【禁用落盘并如实告知模型】，而不是【退到系统临时目录】——
// 后者会给模型一个它以为可靠、实际随时被系统清理、且与实例数据目录无关的路径。
func TestProbeDataDirEmpty(t *testing.T) {
	a, _ := newTestAgent([]llm.FakeStep{
		{Type: "tool", Name: "bash", Args: map[string]interface{}{"command": "cat big"}},
		{Type: "text", Text: "完成"},
	})
	big := strings.Repeat("x", 200)
	path, ok := a.writeToolLogOK(RunOptions{DataDir: ""}, big)
	if ok {
		t.Errorf("DataDir 空时不该声称落盘成功，实际返回 path=%q", path)
	}
	if strings.Contains(path, os.TempDir()) {
		t.Errorf("DataDir 空时不得退回系统临时目录（模型会拿到一个随时失效的路径），实际 %q", path)
	}
	// 必须是绝对路径吗？——不，必须是"不含假路径"的说明：调用方据此改文案。
	if filepath.IsAbs(path) {
		t.Errorf("落盘失败时不该返回一个看起来像路径的字符串，实际 %q", path)
	}
}

// TestProbeTruncateHintNoFakePath DataDir 为空时，截断提示里不得出现可 cat 的路径。
func TestProbeTruncateHintNoFakePath(t *testing.T) {
	a, _ := newTestAgent(nil)
	out := a.formatTruncatedForModel(RunOptions{DataDir: "", ResultLimit: 50}, strings.Repeat("y", 500), 50)
	if strings.Contains(out, "`") {
		t.Errorf("无落盘位置时提示里不该出现反引号包裹的假路径：%q", out)
	}
	if !strings.Contains(out, "落盘失败") {
		t.Errorf("应如实告知落盘失败，实际：%q", out)
	}
}

// ---------- 第三轮反思（2026-10-06）的探针 ----------

// TestProbeMaxToolRoundsDefault 实证兜底值与文档声明是否一致。
// AGENTS.md 关键默认值表写"工具轮次上限 200（0/负 → 200）"，
// config/default.json 也是 200 —— 兜底值就该是 200，否则"0/负 → 200"是假话。
func TestProbeMaxToolRoundsDefault(t *testing.T) {
	got := maxToolRounds(RunOptions{})
	if got != 200 {
		t.Errorf("maxToolRounds 兜底 = %d，AGENTS.md 与 default.json 都声明 200", got)
	}
}

// TestProbeTrimCallRecsBounded 实证 lastCalls 滑窗是否【独立于】doomWarnAfter 有界。
// 第一性原理：裁剪的目的是"防止长任务无限堆积"（callRec.Key 存完整参数 JSON，
// bash command 动辄几 KB），这与检测阈值是多少【无关】。
// 关掉死循环检测（doom.warn_after: 0）不该顺带关掉内存保护。
func TestProbeTrimCallRecsBounded(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 1000} {
		const total = 5000 // 必须远大于任何 keep，否则"没裁"可能只是因为还没超阈值
		rc := make([]callRec, total)
		for i := range rc {
			rc[i] = callRec{Name: "bash", Key: "k"}
		}
		trimCallRecs(&rc, n)
		if len(rc) > maxCallRecs {
			t.Errorf("doomWarnAfter=%d：%d 条裁剪后仍保留 %d 条 —— 滑窗必须独立于阈值有界", n, total, len(rc))
		}
	}
}

// TestProbeResultLimitNonPositive 实证 ResultLimit 在使用处是否有防御。
// 与 DataDir 同理：formatTruncated* 可以绕过 Run 入口被直接调用，
// 不变量要在被使用的地方强制（上一轮只做了 DataDir，漏了 ResultLimit）。
func TestProbeResultLimitNonPositive(t *testing.T) {
	a, _ := newTestAgent(nil)
	full := strings.Repeat("z", 300)
	dir := t.TempDir() // 用测试临时目录，绝不往真实路径写（探针曾污染过源码目录）
	for _, limit := range []int{0, -1} {
		out := a.formatTruncatedForModel(RunOptions{DataDir: dir, ResultLimit: limit}, full, limit)
		if strings.Count(out, "z") == 0 {
			t.Errorf("ResultLimit=%d：正文被截成空（工具结果等于全丢），实际 %q", limit, out)
		}
	}
}

// ---------- 第四轮反思（2026-10-06）的探针 ----------

// TestProbeDoomDetectableAtLargeThreshold 实证：滑窗上限会不会把【检测能力】一起裁掉。
// 第一性原理：isSameRunLocked(n) 需要尾部 n 条记录才能判定。若滑窗只保留 64 条，
// 那么 doom.warn_after > 64 时 len(lastCalls) 永远 < n → 检测【永久失效】，
// 用户设的阈值越大越检测不到——与他设这个值的意图完全相反。
func TestProbeDoomDetectableAtLargeThreshold(t *testing.T) {
	for _, n := range []int{3, 64, 100, 200} {
		var rc []callRec
		for i := 0; i < n+50; i++ { // 连续 n+50 次完全相同
			rc = append(rc, callRec{Name: "bash", Key: "完全相同"})
			trimCallRecs(&rc, n)
		}
		a := &Agent{lastCalls: rc}
		if !a.isSameRunLocked(n) {
			t.Errorf("doom.warn_after=%d：连续 %d 次相同调用却检测不到（滑窗只剩 %d 条）—— 上限把检测能力裁掉了",
				n, n+50, len(rc))
		}
	}
}

// TestProbeCallRecKeyIsBounded 实证单条 callRec 的内存占用是否有界。
// Key 只用于【相等比较】（isSameRunLocked 里 c.Key != first），从不展示、从不解析
// → 存完整参数 JSON 是浪费：bash command 动辄几 KB，100 条就是几百 KB。
// 定长摘要才是正确的形态，这样窗口大小就不必被内存牵着走。
func TestProbeCallRecKeyIsBounded(t *testing.T) {
	big := map[string]interface{}{"command": strings.Repeat("x", 100000)} // 100KB 的命令
	k := stableKey("bash", big)
	if len(k) > 128 {
		t.Errorf("stableKey 长度 %d —— Key 只用于相等比较，应是定长摘要（实测参数 %d 字节）", len(k), 100000)
	}
}
