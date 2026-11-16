package context

// 2026-11-16：进程在工具执行中被 kill，会话里只剩 assistant+tool_calls 而没有 tool 结果。
// compose 必须补上占位结果，否则整条请求被后端拒（OpenAI 要求每个 tool_call 有对应 tool 消息），
// 且模型看不见那次调用发生过 → 会重跑同一条命令。
import (
	"strings"
	"testing"

	"okhuman/internal/types"
)

func TestComposeFillsMissingToolResult(t *testing.T) {
	id1, id2 := "call-1", "call-2"
	sess := &types.SessionState{Messages: []types.RawEntry{
		{Role: "user", Content: "去跑两条命令"},
		{Role: "assistant", Content: "执行中", ToolCalls: []types.ToolCall{
			{ID: id1, Type: "function", Function: types.Function{Name: "bash", Arguments: `{"command":"date"}`}}}},
		{Role: "assistant", Content: "再执行一条", ToolCalls: []types.ToolCall{
			{ID: id2, Type: "function", Function: types.Function{Name: "bash", Arguments: `{"command":"uptime"}`}},
			{ID: "call-3", Type: "function", Function: types.Function{Name: "bash", Arguments: `{"command":"free"}`}}}},
		{Role: "tool", ToolCallID: &id1, Content: "结果一"}, // 只有第一条有结果
	}}
	out := ComposeMessages(sess, "sys")
	// 不变式：每个 tool 消息的 call_id 都必须来自它之前某条 assistant 的 tool_calls（不造孤儿）；
	// 且会话里声明过的每个 call_id 在输出里都要有结果（已有的原样、缺的补占位）。
	var got []string
	declared := map[string]bool{}
	answered := map[string]bool{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			declared[tc.ID] = true
		}
		if m.Role != "tool" {
			continue
		}
		cid := ""
		if m.ToolCallID != nil {
			cid = *m.ToolCallID
		}
		if !declared[cid] {
			t.Errorf("tool %q 没有对应的 tool_calls（造出了孤儿）", cid)
		}
		answered[cid] = true
		got = append(got, cid+":"+types.AsString(m.Content))
	}
	for _, id := range []string{id1, id2, "call-3"} {
		if !answered[id] {
			t.Errorf("%s 没有对应结果", id)
		}
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, id1+":结果一") {
		t.Errorf("已有结果被改动了: %q", joined)
	}
	for _, want := range []string{id2 + ":", "call-3:"} {
		if !strings.Contains(joined, want+missingToolResult) {
			t.Errorf("缺结果的 %s 没有补占位: %q", want, joined)
		}
	}
	if strings.Contains(joined, id1+":"+missingToolResult) {
		t.Errorf("有结果的 %s 被重复补了一条: %q", id1, joined)
	}
}
