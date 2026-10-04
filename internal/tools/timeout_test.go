//go:build !windows
// +build !windows

package tools

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// 工具超时可配置（2026-10-02）：配置值必须同时决定①实际执行上限
// ②注入给模型的描述文本——两者同源，不允许"配置改了描述还写死旧值"。

func TestConfigureDrivesDescription(t *testing.T) {
	Configure(30000, 5000)
	d := Specs()[0].Description
	for _, want := range []string{"前台等待 30 秒", "超 5 秒被强制终止", "会被 5 秒超时连带杀掉"} {
		if !strings.Contains(d, want) {
			t.Fatalf("描述缺 %q：%s", want, d)
		}
	}
	pd := Specs()[0].Parameters.Properties["timeout_seconds"].(map[string]interface{})["description"].(string)
	if !strings.Contains(pd, "默认 5，上限 5") {
		t.Fatalf("timeout_seconds 描述未随配置：%s", pd)
	}
}

func TestConfigureFallbackAndStability(t *testing.T) {
	for _, bad := range []int{0, -1, MaxToolTimeoutMS + 1} {
		Configure(30000, bad)
		if got := int(toolTimeoutMS.Load()); got != DefaultToolTimeoutMS {
			t.Fatalf("非法值 %d 应回落默认，得 %d", bad, got)
		}
		if !strings.Contains(Specs()[0].Description, "超 600 秒被强制终止") {
			t.Fatal("回落默认后描述应为 600 秒")
		}
	}
	// 同参数 → 逐字同文本（KV 前缀稳定的前提）
	Configure(30000, 5000)
	a := Specs()[0].Description
	Configure(30000, 5000)
	if a != Specs()[0].Description {
		t.Fatal("同配置两次生成文本不一致 → KV 前缀会漂")
	}
}

func TestConfiguredLimitKillsProcessGroup(t *testing.T) {
	Configure(30000, 2000) // 2 秒上限
	start := time.Now()
	out, err := ExecuteTool("bash", map[string]interface{}{"command": "sleep 8; echo SHOULD_NOT_APPEAR"})
	el := time.Since(start).Seconds()
	if err != nil {
		t.Fatalf("执行不应报错：%v", err)
	}
	if el > 4.5 {
		t.Fatalf("未在配置上限附近被杀，耗时 %.1fs", el)
	}
	if strings.Contains(out, "SHOULD_NOT_APPEAR") {
		t.Fatal("命令未被终止（上限未生效）")
	}
	// 结果文本必须自报上限值与出路（模型据此选择脱离/调配置，而非盲目重试）
	if !strings.Contains(out, "超过 2 秒上限被强制终止") || !strings.Contains(out, "tools.timeout_ms") {
		t.Fatalf("超时结果未自报上限与出路：%s", out[:200])
	}
}

func TestModelCanLowerButNotRaise(t *testing.T) {
	Configure(30000, 5000)
	// 下调：1 秒
	start := time.Now()
	ExecuteTool("bash", map[string]interface{}{"command": "sleep 8", "timeout_seconds": float64(1)})
	if el := time.Since(start).Seconds(); el > 2.5 {
		t.Fatalf("timeout_seconds=1 未生效，耗时 %.1fs", el)
	}
	// 越权上调：请求 99999，配置上限 5 秒 → 必须被 clamp 到 5 秒
	start = time.Now()
	ExecuteTool("bash", map[string]interface{}{"command": "sleep 30", "timeout_seconds": float64(99999)})
	if el := time.Since(start).Seconds(); el > 8 {
		t.Fatalf("越过配置上限未被 clamp，耗时 %.1fs", el)
	}
}

// TestOversizedTimeoutNotInvertedTo1s 回归（2026-10-04 审查 P0）：
// timeout_seconds 传 float64 域"有限但远超 int64"的值（1e300、9.3e18、MaxFloat64），
// 旧实现直接 int(f) 溢出成最小 int64（amd64）→ 被 `< 1` 兜底反转成 1 秒，
// 模型意图"跑很久"变成"1 秒被杀"。修法：先在 float64 域 clamp 到 limitSec 再转 int。
//
// 断言：命令必须能跑满配置上限附近（不被 1 秒杀掉），且不得出现"退出码: -1"（被杀）。
func TestOversizedTimeoutNotInvertedTo1s(t *testing.T) {
	Configure(30000, 5000) // 上限 5 秒
	for _, tc := range []struct {
		name string
		val  float64
	}{
		{"1e300", 1e300},
		{"9.3e18", 9.3e18}, // 刚越过 int64 边界
		{"1e19", 1e19},
		{"MaxFloat64", math.MaxFloat64},
	} {
		start := time.Now()
		out, err := ExecuteTool("bash", map[string]interface{}{
			"command":         "sleep 8; echo REACHED_END",
			"timeout_seconds": tc.val,
		})
		el := time.Since(start).Seconds()
		if err != nil {
			t.Fatalf("[%s] 执行不应报错：%v", tc.name, err)
		}
		// 必须在配置上限（5s）附近被杀，而不是 ~1s
		if el < 4.0 {
			t.Fatalf("[%s] 耗时仅 %.2fs → 疑似被反转成 1 秒（溢出缺陷）", tc.name, el)
		}
		if el > 8 {
			t.Fatalf("[%s] 耗时 %.2fs → 未在配置上限附近终止", tc.name, el)
		}
		// 输出必须自报 5 秒上限，不得是 1 秒
		if !strings.Contains(out, "超过 5 秒上限被强制终止") {
			t.Fatalf("[%s] 超时文案不是配置上限 5 秒：%s", tc.name, out[:min(120, len(out))])
		}
	}
}

// TestIsTimeoutKillDecisionTable 回归（2026-10-04 审查 P1）**确定性**验证：
// "是否超时终止"必须由 (killed, signaled) 两条件共同决定。
//
// 为什么用纯函数单测而非集成测试：真实竞态是概率性的（相位扫描约 2-3% 命中），
// 变异测试证明 40 次迭代的集成断言**抓不到**把 `killed && signaled` 改回 `killed`
// 的回退（假绿）。把判定抽成 isTimeoutKill 后，四种组合可确定性覆盖。
func TestIsTimeoutKillDecisionTable(t *testing.T) {
	cases := []struct {
		killed   bool
		signaled bool
		want     bool
		why      string
	}{
		{true, true, true, "发过 kill 且死于信号 → 真超时"},
		{true, false, false, "发过 kill 但进程自行 exit(0)（僵尸组 Kill 假成功）→ 不报超时"},
		{false, true, false, "进程自己 kill -9 $$，非本工具超时 → 不报超时"},
		{false, false, false, "没发 kill 也没死于信号 → 不报超时"},
	}
	for _, c := range cases {
		if got := isTimeoutKill(c.killed, c.signaled); got != c.want {
			t.Errorf("isTimeoutKill(killed=%v, signaled=%v) = %v, want %v（%s）",
				c.killed, c.signaled, got, c.want, c.why)
		}
	}
}

// TestKilledRaceNoContradictoryZeroExit 集成回归（2026-10-04 审查 P1）：
// 命令在计时器触发窗口内自行 exit(0) 时，输出不得自相矛盾（"退出码: 0" +
// "被强制终止"）。此测试补充覆盖端到端链路；判定逻辑的确定性验证见
// TestIsTimeoutKillDecisionTable（概率性命中，单跑可能漏，故两测并存）。
func TestKilledRaceNoContradictoryZeroExit(t *testing.T) {
	if testing.Short() {
		t.Skip("概率性竞态集成测试，-short 下跳过；确定性验证见 TestIsTimeoutKillDecisionTable")
	}
	Configure(30000, 600000)
	contradictions := 0
	// 相位 0.970 ~ 1.050 秒（timeout=1s），覆盖计时器触发前后
	for i := 0; i < 40; i++ {
		frac := 0.970 + float64(i)/1000.0
		cmd := fmt.Sprintf("sleep %.3f; echo SELF_DONE", frac)
		out, err := ExecuteTool("bash", map[string]interface{}{
			"command":         cmd,
			"timeout_seconds": float64(1),
		})
		if err != nil {
			t.Fatalf("执行不应报错：%v", err)
		}
		if strings.Contains(out, "退出码: 0") && strings.Contains(out, "被强制终止") {
			contradictions++
			t.Errorf("相位 %.3f 出现矛盾输出（退出码 0 却称超时）: %q", frac, out)
		}
	}
	if contradictions > 0 {
		t.Fatalf("检测到 %d 次矛盾输出 → killed 竞态未修好", contradictions)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
