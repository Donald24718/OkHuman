package context

import (
	"math"
	"testing"

	"okhuman/internal/textutil"
	"okhuman/internal/types"
)

// 同一件事不能用两种口径：EstimateMessageTokens 早就是 UTF-16，messageChars 却按字节
// 数 tool_calls 的 name/arguments（2026-11-12 四方审查发现）。中文工具参数下字节是字符的
// 3 倍，而 messageChars 正是压缩预算循环走的那条 → 触发点会偏。
func TestMessageCharsCountsToolArgsInUTF16(t *testing.T) {
	name := "写文件"
	args := `{"路径":"/tmp/中文内容","内容":"你好世界"}`
	m := types.RawEntry{Role: "assistant", ToolCalls: []types.ToolCall{
		{Function: types.Function{Name: name, Arguments: args}},
	}}
	got := messageChars(m, NewEst(1.5, 0))
	want := textutil.UTF16Len(name) + textutil.UTF16Len(args)
	if got != want {
		t.Errorf("tool_calls 计数 = %d，期望 UTF-16 口径 %d（字节口径会是 %d）",
			got, want, len(name)+len(args))
	}
}

// token 估算必须就是同一份字符数除以 cpl —— 不允许再有第二套字符计算。
// 中文场景专门测一遍：字节口径会让 token 多出一倍（这就是当初的 bug 形状）。
func TestTokenEstimatorIsJustCharsOverCpl(t *testing.T) {
	m := types.RawEntry{Role: "assistant", Content: "中文正文", ToolCalls: []types.ToolCall{
		{Function: types.Function{Name: "执行", Arguments: `{"command":"ls 中文目录"}`}},
	}}
	for _, cpl := range []float64{1.5, 2.0} {
		e := NewEst(cpl, 0)
		want := int(math.Ceil(float64(messageChars(m, e)) / cpl))
		if got := EstimateMessageTokens(m, e); got != want {
			t.Errorf("cpl=%v 时 token=%d，期望 字符 %d / cpl = %d", cpl, got, messageChars(m, e), want)
		}
	}
}

// 非 assistant 却带 tool_calls 的条目也要计入（旧实现只在 assistant 分支里加，
// 会让这类条目的预算被少算）。
func TestToolArgsCountedRegardlessOfRole(t *testing.T) {
	m := types.RawEntry{Role: "tool", ToolCalls: []types.ToolCall{
		{Function: types.Function{Name: "执行", Arguments: `{"a":"中文"}`}},
	}}
	if n := messageChars(m, NewEst(1.5, 0)); n != textutil.UTF16Len(`{"a":"中文"}`)+textutil.UTF16Len("执行") {
		t.Errorf("tool 角色的 tool_calls 未被计入：%d", n)
	}
}
