//go:build !windows
// +build !windows

package tools

import (
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
