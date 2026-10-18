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
	if !strings.Contains(out, "超过 2 秒上限被强制终止") || !strings.Contains(out, "tools.timeout_sec") {
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

// TestSpecTextStableAtLiveValues 工具描述文本锁定（2026-10-18）：描述里带超时秒数，
// 文本变一个字节 → 工具表变 → system+tools 前缀变 → 服务端 KV 前缀失效（整段重算）。
// 生产实配是 fg=30s / 总时长 60000s，这里把这两档下的逐字文本钉住：改配置单位或改
// 措辞时若不小心动了这段，测试直接红，而不是等到线上白烧一轮前缀才发现。
func TestSpecTextStableAtLiveValues(t *testing.T) {
	Configure(30*1000, 60000*1000)
	specs := Specs()
	if len(specs) != 1 || specs[0].Name != "bash" {
		t.Fatalf("工具表异常：%+v", specs)
	}
	want := "执行 bash 命令（异步并行；输出超长被截断时，全文已写入 log 文件、截断处会给出路径，用 bash 的 cat / grep / tail 抓回）。" +
		"前台等待 30 秒未完成自动转后台（你收到通知后继续干活，结果在轮内下一个工具调用间隙或下一次 run 带回来）；命令总时长超 60000 秒被强制终止（整个命令进程组）。" +
		"起长驻服务（python 服务器等）必须完全脱离，否则服务占住本工具的进程组、会被 60000 秒超时连带杀掉：用 " +
		"(setsid <启动命令> < /dev/null > /tmp/<名>.log 2>&1 &)，之后用 curl 探活、cat 读日志。脱离后服务独立存活，不受前台超时/转后台影响。" +
		"脱离必须三件套：setsid + 重定向 stdout/stderr + < /dev/null——只 setsid 不重定向，脱离子进程仍持有输出管道写端，Wait 收不到 EOF 会永久挂住，且超时杀组打不到它（上限彻底失效）。"
	if specs[0].Description != want {
		t.Errorf("bash 描述文本变了（会毁 KV 前缀）：\n得：%s\n望：%s", specs[0].Description, want)
	}
}
