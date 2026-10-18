package main

import (
	"strings"
	"testing"

	"okhuman/internal/persist"
	"okhuman/internal/types"
)

// TestPersistSaveGuardSessionNo C4：监听器写盘用的会话号是**注册时**的快照，
// 不是写时才读的全局值。/reset 之后旧 cm 的迟到写入必须被丢弃（否则旧会话状态
// 会按新会话号写进 session.json：会话号回退 + 旧消息"复活"），并留下一行证据。
func TestPersistSaveGuardSessionNo(t *testing.T) {
	dir := t.TempDir()
	p, err := persist.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	logf := func(m string) { logs = append(logs, m) }
	cur := 5
	st := &types.SessionState{Messages: []types.RawEntry{{Role: "user", Content: "旧会话内容"}}}

	save := newPersistSave(p, func() int { return cur }, 5, logf)
	save(st, 7)
	snap, err := persist.LoadSessionFile(dir)
	if err != nil || snap == nil {
		t.Fatalf("会话号一致时应落盘：err=%v snap=%v", err, snap)
	}
	if snap.No != 5 || snap.Seq != 7 {
		t.Errorf("落盘内容不对：no=%d seq=%d", snap.No, snap.Seq)
	}

	// /reset 后：全局会话号前进，旧监听器的迟到写入必须被丢弃、不得覆盖新会话
	cur = 6
	save(st, 8)
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "丢弃迟到的会话写入") {
		t.Errorf("应记一行丢弃日志，实际 logs=%v", logs)
	}
	snap, _ = persist.LoadSessionFile(dir)
	if snap == nil || snap.Seq != 7 {
		t.Errorf("迟到写入覆盖了新会话状态：snap=%+v", snap)
	}
}
