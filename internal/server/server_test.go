package server

import (
	"fmt"
	"testing"
)

// utf16Count 本地 UTF-16 code units 计数（与 context.utf16Len 同口径）
func utf16Count(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// TestAssembleBatch 无车多条消息组装规则（2026-09-14）：
// 1 条 = 该条本身（user，无前缀）；>1 条 = 合并成一条【第 N 条】…（merged）。
func TestAssembleBatchSingle(t *testing.T) {
	msg, kind, label, n, hbOnly := assembleBatch([]queuedMsg{{Text: "用户追加三"}})
	if hbOnly {
		t.Fatalf("真实消息批次不该判成纯心跳")
	}
	if msg != "用户追加三" || kind != "user" || label != nil || n != 0 {
		t.Fatalf("got (msg=%q kind=%q label=%v n=%d)，want (用户追加三, user, nil, 0)", msg, kind, label, n)
	}
}

func TestAssembleBatchMerged(t *testing.T) {
	msgs := []queuedMsg{
		{Text: "用户追加三"},
		{Text: "（系统通知：后台任务完成——D-OK）"},
		{Text: "用户追加四"},
	}
	msg, kind, label, n, hbOnly := assembleBatch(msgs)
	if hbOnly {
		t.Fatalf("含真实消息的批次不该判成纯心跳")
	}
	want := "【第 1 条】用户追加三\n【第 2 条】（系统通知：后台任务完成——D-OK）\n【第 3 条】用户追加四"
	if msg != want {
		t.Fatalf("msg = %q\nwant %q", msg, want)
	}
	if kind != "merged" || n != 3 {
		t.Fatalf("kind=%q n=%d，want merged/3", kind, n)
	}
	wantLabel := fmt.Sprintf("3 条合并 / %d 字符",
		utf16Count(msgs[0].Text)+utf16Count(msgs[1].Text)+utf16Count(msgs[2].Text))
	if label == nil || *label != wantLabel {
		t.Fatalf("label=%v，want %q", label, wantLabel)
	}
}

// TestAssembleBatchHeartbeatOnly 空转避让的判定基础（2026-10-22）：只有**整批都是
// 心跳消息**才算 hbOnly。关键反例——心跳入队后、drain 启动前用户的真实消息搭进
// 同一批：这种批必须 hbOnly=false，否则会把用户活动误判成空转，间隔越拉越长。
func TestAssembleBatchHeartbeatOnly(t *testing.T) {
	if _, _, _, _, hb := assembleBatch([]queuedMsg{{Text: "（心跳）", Heartbeat: true}}); !hb {
		t.Errorf("纯心跳单条应 hbOnly=true")
	}
	if _, _, _, _, hb := assembleBatch([]queuedMsg{
		{Text: "（心跳）", Heartbeat: true},
		{Text: "（后台任务完成通知）", Heartbeat: true},
	}); !hb {
		t.Errorf("多条全心跳应 hbOnly=true")
	}
	if _, _, _, _, hb := assembleBatch([]queuedMsg{
		{Text: "（心跳）", Heartbeat: true},
		{Text: "帮我看个东西"}, // 用户消息搭进同一批
	}); hb {
		t.Errorf("混入真实消息必须 hbOnly=false（否则误判空转）")
	}
	if _, _, _, _, hb := assembleBatch([]queuedMsg{}); hb {
		t.Errorf("空批不应 hbOnly=true")
	}
}
