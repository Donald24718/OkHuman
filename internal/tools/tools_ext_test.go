package tools

import (
	"strings"
	"testing"
)

// TestExecuteToolUnknownNameEscaped 回归背景：default 分支原用 %s 拼接外部传入的
// name，而 name 来自 LLM 的 tool_calls[].function.name（模型可控），可能含换行/
// ANSI 转义，直接拼接会污染日志与 LLM 上下文。改用 %q 后应自动转义。
//
// 同时：文案不再硬编码"唯一元工具是 bash"——switch 是可扩展点，硬编码工具数量
// 会随新增工具过时（且该文案会回传给模型）。
func TestExecuteToolUnknownNameEscaped(t *testing.T) {
	cases := []struct {
		name    string
		evil    string
		wantSub string // 期望出现在错误文本里的（转义后）片段
	}{
		{"换行", "bash\necho injected", `"bash\necho injected"`},
		{"ANSI", "bash\x1b[31mred", `"bash\x1b[31mred"`},
		{"制表符", "bash\ttab", `"bash\ttab"`},
		{"普通", "ipython", `"ipython"`},
		{"空", "", `""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ExecuteTool(c.evil, map[string]interface{}{})
			if err == nil {
				t.Fatal("未知工具应返回 error")
			}
			msg := err.Error()
			if !strings.Contains(msg, c.wantSub) {
				t.Errorf("错误文本未按 %q 转义：got %q, want 含 %q", c.evil, msg, c.wantSub)
			}
			// 反面断言：原始控制字符不得原样出现（否则说明没转义）
			if strings.ContainsAny(msg, "\n\t\x1b") {
				t.Errorf("错误文本含未转义控制字符：%q", msg)
			}
			// 不得再断言"唯一元工具"（可扩展点，不应硬编码工具数量）
			if strings.Contains(msg, "唯一") {
				t.Errorf("错误文本不应硬编码工具数量断言：%q", msg)
			}
		})
	}
}

// TestExecuteToolBashPassThrough 见 tools_test.go（需真实 bash，带 !windows tag）。
// 本文件保持平台无关：Windows 阶段 3 前无 bash，故真实执行类测试不放这里。
