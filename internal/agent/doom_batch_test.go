// 死循环强停/警告的批次形状（2026-11-13 四方审计）。两条都是"一次响应里并行多个
// 工具调用"时才暴露：
//
//	强停只补被触发的那条 → 同批后面的调用没有 tool 消息，残缺轮永久跟着会话；
//	警告文案取 lastCalls 末位 → 混合批里会叫错工具名。
//
// 注意：lastCalls/warned 每轮开始会被重置（agent.go runUserMessage），所以重复必须在
// run 内用多步响应构造，预置字段不起作用。
package agent

import (
	"context"
	"strings"
	"testing"

	"okhuman/internal/llm"
	"okhuman/internal/types"

	ctxmgr "okhuman/internal/context"
)

// scriptClient：第 n 次响应给 batches[n] 这批 tool_calls，用完给正文（强停后的收尾）。
type scriptClient struct {
	batches [][]types.ToolCall
	text    string
	n       int
}

func (c *scriptClient) CompleteStream(context.Context, []types.Message, []types.ToolSpec, func(types.Delta), int) (*types.Response, error) {
	if c.n < len(c.batches) {
		r := &types.Response{ToolCalls: c.batches[c.n]}
		c.n++
		return r, nil
	}
	c.n++
	return &types.Response{Content: c.text}, nil
}

func newScriptAgent(t *testing.T, c llm.LlmClient) *Agent {
	t.Helper()
	cm := ctxmgr.NewManager(nil, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2, KeepSummaries: 5, SummaryMaxChars: 30000,
	}, "系统提示", t.TempDir())
	return New(c, "系统提示", cm)
}

func bashCall(id, cmd string) types.ToolCall {
	return types.ToolCall{ID: id, Type: "function", Function: types.Function{
		Name: "bash", Arguments: `{"command":"` + cmd + `"` + "}"}}
}

func otherCall(id string) types.ToolCall {
	return types.ToolCall{ID: id, Type: "function", Function: types.Function{
		Name: "not_a_tool", Arguments: `{"x":"y"}`}}
}

func sessionToolReplies(a *Agent) map[string]string {
	out := map[string]string{}
	for _, m := range a.cm.Session().Messages {
		if m.Role == "tool" && m.ToolCallID != nil {
			s, _ := m.Content.(string)
			out[*m.ToolCallID] = s
		}
	}
	return out
}

func sessionWarnings(a *Agent) []string {
	var out []string
	for _, m := range a.cm.Session().Messages {
		if m.Role != "user" {
			continue
		}
		txt, _ := m.Content.(string)
		if strings.Contains(txt, "系统警告") {
			out = append(out, txt)
		}
	}
	return out
}

// 强停在批次中间：被触发的和排在它后面的都要有 tool 消息，且后面的那条是合成的
// （它从未执行，也不该有 tool_call 事件）。
func TestDoomStopClosesRemainingToolCalls(t *testing.T) {
	c := &scriptClient{
		batches: [][]types.ToolCall{
			{bashCall("c1", "echo A")},
			{bashCall("c2", "echo A")},                           // 连续 2 次相同 → 警告（DoomWarnAfter=2）
			{bashCall("c3", "echo A"), bashCall("c4", "echo B")}, // 已警告 → c3 强停，c4 一并跳过
		},
		text: "收尾",
	}
	a := newScriptAgent(t, c)
	opt := testRunOpt()
	opt.DoomWarnAfter = 2
	if _, err := a.Run(context.Background(), "跑", opt); err != nil {
		t.Fatal(err)
	}
	got := sessionToolReplies(a)
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		if _, ok := got[id]; !ok {
			t.Errorf("调用 %s 没有对应 tool 消息（会话里只有 %v）", id, keysOf(got))
		}
	}
	// 排在后面的调用要说清"没执行"，不能沿用"重复完全相同调用"那句：它自己没重复过
	s := got["c4"]
	if !strings.Contains(s, "未执行") || !strings.Contains(s, "重复完全相同") {
		t.Errorf("c4 应说清这个调用未执行，实际：%q", s)
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 警告要指名被重复的 bash，不是批次里最后那条（not_a_tool 只被调用过一次）。
func TestDoomWarnNamesTheRepeatedCall(t *testing.T) {
	c := &scriptClient{
		batches: [][]types.ToolCall{
			{bashCall("c1", "echo A")},
			{bashCall("c2", "echo A"), otherCall("c3")}, // 触发重复的是 c2，末位是 c3
		},
		text: "收尾",
	}
	a := newScriptAgent(t, c)
	opt := testRunOpt()
	opt.DoomWarnAfter = 2
	if _, err := a.Run(context.Background(), "跑", opt); err != nil {
		t.Fatal(err)
	}
	warns := sessionWarnings(a)
	if len(warns) == 0 {
		t.Fatal("没有注入系统警告")
	}
	w := warns[len(warns)-1]
	if !strings.Contains(w, "调用 bash") {
		t.Errorf("警告没有指名被重复的工具 bash：%q", w)
	}
	if strings.Contains(w, "调用 not_a_tool") {
		t.Errorf("警告把批次末位当成了被重复的那条：%q", w)
	}
}
