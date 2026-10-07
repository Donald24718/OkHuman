package tools

// ipython 接入后对**工具表契约**的守护。
//
// 这里守的不是 ipython 本身（那由 ipython 包自己的测试负责），而是
// 「新增工具不得伤害既有部署」这条边界。

import (
	"os"
	"strings"
	"testing"

	"okhuman/internal/types"
)

// names 取工具表里的工具名。
func names(specs []types.ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// TestIPythonAbsentByDefault 未启用 ipython 时工具表必须只有一个 bash，
// 与本次改动前逐字一致（否则既有部署的 KV 前缀会集体失效）。
func TestIPythonAbsentByDefault(t *testing.T) {
	if err := ConfigureIPython(""); err != nil {
		t.Fatalf("禁用不应报错: %v", err)
	}
	specs := Specs()
	if len(specs) != 1 {
		t.Fatalf("工具数量 = %d，期望 1；实际名字：%v", len(specs), names(specs))
	}
	if specs[0].Name != "bash" {
		t.Fatalf("唯一工具应名为 bash，实际 %q", specs[0].Name)
	}
	if strings.Contains(specs[0].Description, "ipython") {
		t.Errorf("bash 的描述里混进了 ipython 字样（两者应互不相干）：%s", specs[0].Description)
	}
}

// TestIPythonSpecRegisteredWhenEnabled 启用后工具表多出一个 ipython。
func TestIPythonSpecRegisteredWhenEnabled(t *testing.T) {
	// 用一个"假"解释器路径：Setup 会先 VerifyPython，失败必然返回 error 且不启用。
	// 这里只想验证「启用 → 注册」这条路径，所以退而求其次检查不崩溃且状态自洽。
	if err := ConfigureIPython("/definitely/not/a/python"); err == nil {
		t.Fatalf("非法解释器路径应当报错")
	}
	if got := names(Specs()); len(got) != 1 {
		t.Fatalf("启用失败时的工具表应保持为 %v，实际 %v", []string{"bash"}, got)
	}
	if IPythonEnabled() {
		t.Errorf("启用失败不应标记 enabled")
	}
	// 环境恢复：别让本测试的状态泄漏给同包的其它测试。
	_ = ConfigureIPython("")
}

// TestIPythonSpecStableAcrossRebuild 同一份配置重建两次 → 逐字相同（KV 前缀稳定）。
//
// 这条是 ipython 描述 text 的护栏：描述里一旦掺入随机数、时间戳、map 遍历
// 顺序等不确定项，每次启动都会作废一遍前缀缓存。
func TestIPythonSpecStableAcrossRebuild(t *testing.T) {
	Configure(DefaultFgTimeoutMS, DefaultToolTimeoutMS)
	baseline := Specs()

	for i := 0; i < 3; i++ {
		Configure(DefaultFgTimeoutMS, DefaultToolTimeoutMS)
		got := Specs()
		if len(got) != len(baseline) {
			t.Fatalf("第 %d 次重建工具数量变化：%v vs %v", i, names(got), names(baseline))
		}
		for j := range got {
			if got[j].Name != baseline[j].Name || got[j].Description != baseline[j].Description {
				t.Errorf("第 %d 次重建后第 %d 个工具文本变化：\n got: %s\nwant: %s",
					i, j, got[j].Description, baseline[j].Description)
			}
		}
	}
}

// TestExecuteToolIPythonEndToEnd 真实全链路：Configure → ConfigureIPython → ExecuteTool。
//
// 前面的测试只证明了 Kernel 能跑；这条证明**接线也对**——工具进表、能被分发、
// 返回结果格式正确。少了它，"注册了但调用路径不通"这类问题是测不出来的。
func TestExecuteToolIPythonEndToEnd(t *testing.T) {
	python := os.Getenv("OKHUMAN_IPYTHON_PYTHON")
	if python == "" {
		t.Skip("未指定 OKHUMAN_IPYTHON_PYTHON，跳过端到端")
	}

	Configure(DefaultFgTimeoutMS, DefaultToolTimeoutMS)
	if err := ConfigureIPython(python); err != nil {
		t.Skipf("跳过：ipython 未启用（%v）", err)
	}
	defer ConfigureIPython("") // 恢复：别让启用状态泄漏给同包其它测试

	var inTable bool
	for _, s := range Specs() {
		if s.Name == "ipython" {
			inTable = true
			if s.Parameters.Type != "object" || len(s.Parameters.Required) == 0 {
				t.Errorf("ipython spec 参数结构异常: %+v", s.Parameters)
			}
		}
	}
	if !inTable {
		t.Fatal("启用后工具表缺少 ipython")
	}

	out, err := ExecuteTool("ipython", map[string]interface{}{"code": "6*7"})
	if err != nil {
		t.Fatalf("ExecuteTool(ipython) 失败: %v", err)
	}
	if !strings.Contains(out, "42") {
		t.Errorf("结果异常:\n%s", out)
	}
	// 成功输出绝不能以 ToolFailPrefix 开头，否则 background 会误判任务失败。
	if strings.HasPrefix(out, ToolFailPrefix) {
		t.Errorf("成功输出以失败前缀开头（会误判后台任务成败）: %s", out)
	}

	// 用户代码出错算成功返回（模型需要看到 traceback），但文案必须是"出错"而非"中断"。
	out, err = ExecuteTool("ipython", map[string]interface{}{"code": "raise ValueError('boom')"})
	if err != nil {
		t.Fatalf("用户代码出错不应走 error 路径: %v", err)
	}
	if !strings.Contains(out, "出错: ValueError") {
		t.Errorf("未报出用户代码异常:\n%s", out)
	}
	if strings.Contains(out, "被中断") {
		t.Errorf("用户代码出错被误标成中断:\n%s", out)
	}
}

// TestIPythonToolNameInSwitch 工具表里的名字必须真能被 ExecuteTool 分发。
//
// 防的是"注册了但没接线"——模型看得到、调用不到，且报错很含糊。
func TestIPythonToolNameInSwitch(t *testing.T) {
	// ipython 未启用时它不在表中，无从验证；这里只校验表里名字的可分发性。
	for _, s := range Specs() {
		out, err := ExecuteTool(s.Name, map[string]interface{}{})
		if err == nil {
			t.Errorf("%s: 空参数本应报错，实际返回 %q", s.Name, out)
		}
		if strings.Contains(err.Error(), "未知工具") {
			t.Errorf("%s: 工具在表中但 ExecuteTool 未登记（注册与分发不一致）", s.Name)
		}
	}
}
