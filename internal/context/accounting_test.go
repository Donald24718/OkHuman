package context

import (
	"strings"
	"testing"

	"okhuman/internal/types"
)

// 展示层（/status 的 raw_tail）和压缩预算必须是同一把尺子。曾经 server 那边自己算一遍、
// 附件按写死的 1000 字符计，这边按 context.attachment_tokens 配置换算 → 同一次响应两个数。
func TestEstMessageCharsIsTheOneRuler(t *testing.T) {
	est := NewEst(2.0, 500) // attach 500 token × cpl 2 = 1000 字符（默认 cpl 下才等于旧写死值）
	m := types.RawEntry{Role: "user", Content: []types.ContentPart{
		{Type: "text", Text: "abcd"},
		{Type: "image_url"},
	}}
	if got := est.MessageChars(m); got != 4+1000 {
		t.Errorf("附件应按 attachment_tokens×cpl 计：%d，期望 %d", got, 4+1000)
	}
	if got := est.MessageChars(m); got != messageChars(m, est) {
		t.Errorf("对外方法必须与内部实现同值：%d vs %d", got, messageChars(m, est))
	}
	// 非 assistant 带 tool_calls 也计（旧口径只计 assistant，两条路径会漂移）
	tool := types.RawEntry{Role: "tool", Content: "x", ToolCalls: []types.ToolCall{
		{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "bash", Arguments: strings.Repeat("汉", 10)}}}}
	if got := est.MessageChars(tool); got < 10+4+1 {
		t.Errorf("tool 角色的 tool_calls 也应计入：%d", got)
	}
}

// keep 窗口的边界：正好等于预算要继续收，超一个字符就停；且至少保留最新一条。
func TestKeepWindowKeepsLatestWithinBudget(t *testing.T) {
	mk := func(s string) types.RawEntry { return types.RawEntry{Role: "user", Content: s} }
	newMgr := func(keepChars int, msgs []types.RawEntry) *Manager {
		m := &Manager{est: NewEst(1.0, 0), session: &types.SessionState{}}
		m.cfg.KeepRecentChars = keepChars
		m.session.Messages = msgs
		return m
	}
	msgs := []types.RawEntry{mk("aaaa"), mk("bb"), mk("c")} // 从旧到新
	if got := newMgr(1, msgs).keepWindowLocked(); got != 1 {
		t.Errorf("预算只够最新一条时应为 1，得 %d", got)
	}
	if got := newMgr(3, msgs).keepWindowLocked(); got != 2 { // "c"+"bb"=3 正好等于预算 → 收
		t.Errorf("累计正好等于预算时应继续收，得 %d", got)
	}
	if got := newMgr(2, msgs).keepWindowLocked(); got != 1 { // "c"+"bb"=3 > 2 → 停
		t.Errorf("超预算一个字符即应停，得 %d", got)
	}
	if got := newMgr(99, msgs).keepWindowLocked(); got != 3 {
		t.Errorf("预算足够时应全部保留，得 %d", got)
	}
	if got := newMgr(0, nil).keepWindowLocked(); got != 0 {
		t.Errorf("空会话应为 0，得 %d", got)
	}
}
