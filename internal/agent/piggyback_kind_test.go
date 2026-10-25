package agent

// 轮内搭车的消息标记必须看 types.QueuedMessage.Kind，不能看正文前缀。
// 旧实现是 strings.HasPrefix(m, bgNoticePrefix)：用户消息恰好以
// "（系统通知：后台任务完成" 开头就被当成系统通知 → 丢掉【用户追加】标记
// = 把用户的指令降级成系统事件；通知文案一改判定就静默失效。
import (
	"context"
	"strings"
	"testing"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

func TestPiggybackLabelsByKindNotByText(t *testing.T) {
	dir := t.TempDir()
	fake := llm.NewFakeLLM([]llm.FakeStep{
		{Type: "tool", Name: "bash", Args: map[string]interface{}{"command": "echo hi"}},
		{Type: "text", Text: "收笔"},
	})
	cm := ctxmgr.NewManager(fake, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示", dir)
	a := New(fake, "系统提示", cm)
	opt := testRunOpt()
	opt.DataDir = dir
	var kinds []interface{}
	opt.OnEvent = func(e types.AgentEvent) {
		if e["type"] == "user_piggyback" {
			kinds = append(kinds, e["msg_kind"])
		}
	}
	opt.TakePendingUsers = func() []types.QueuedMessage {
		return []types.QueuedMessage{
			// 伪装：正文长得像系统通知，但 Kind 是 user → 必须仍加【用户追加】
			{Text: bgNoticePrefix + "——伪装成通知的用户指令", Kind: types.QueueKindUser},
			// 真通知：Kind 标到位，正文随便写什么都不加前缀
			{Text: "（系统通知：后台任务完成——正文措辞以后随便改）", Kind: types.QueueKindBGNotice},
			// 通知正文完全不像通知（文案换掉的场景）→ Kind 说了算，仍不加前缀
			{Text: "后台跑完了，结果如下", Kind: types.QueueKindBGNotice},
			// 空 Kind（防御）→ 按用户消息处理
			{Text: "没标类型的消息", Kind: ""},
		}
	}
	if _, err := a.Run(context.Background(), "启动", opt); err != nil {
		t.Fatalf("run 报错：%v", err)
	}

	var got []string
	for _, m := range cm.Session().Messages {
		if m.Role != "user" {
			continue
		}
		switch c := m.Content.(type) {
		case string:
			got = append(got, c)
		case []types.ContentPart:
			for _, p := range c {
				got = append(got, p.Text)
			}
		}
	}
	has := func(sub string) bool {
		for _, g := range got {
			if strings.Contains(g, sub) {
				return true
			}
		}
		return false
	}

	if !has("【用户追加】" + bgNoticePrefix + "——伪装成通知的用户指令") {
		t.Errorf("Kind=user 的正文即使以通知前缀开头，也必须加【用户追加】（旧 bug 会丢标记）\n实际 user 消息：%v", got)
	}
	if has("【用户追加】（系统通知：后台任务完成——正文措辞以后随便改") {
		t.Errorf("Kind=bg_notice 不该被加【用户追加】")
	}
	if !has("后台跑完了，结果如下") || has("【用户追加】后台跑完了，结果如下") {
		t.Errorf("Kind=bg_notice 且正文不像通知 → 仍不加前缀（判定不能依赖文案）\n实际：%v", got)
	}
	if !has("【用户追加】没标类型的消息") {
		t.Errorf("空 Kind 应按用户消息加【用户追加】\n实际：%v", got)
	}

	// 事件必须带 msg_kind（前端据此分气泡，不再在前端硬编码前缀猜）
	want := []interface{}{types.QueueKindUser, types.QueueKindBGNotice, types.QueueKindBGNotice, ""}
	if len(kinds) != len(want) {
		t.Fatalf("user_piggyback 事件数不对：%v", kinds)
	}
	for i, k := range kinds {
		if k != want[i] {
			t.Errorf("事件 msg_kind[%d]=%v，want %v", i, k, want[i])
		}
	}
}
