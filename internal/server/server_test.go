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
	msg, kind, label, n := assembleBatch([]string{"用户追加三"})
	if msg != "用户追加三" || kind != "user" || label != nil || n != 0 {
		t.Fatalf("got (msg=%q kind=%q label=%v n=%d)，want (用户追加三, user, nil, 0)", msg, kind, label, n)
	}
}

func TestAssembleBatchMerged(t *testing.T) {
	msgs := []string{
		"用户追加三",
		"（系统通知：后台任务完成——D-OK）",
		"用户追加四",
	}
	msg, kind, label, n := assembleBatch(msgs)
	want := "【第 1 条】用户追加三\n【第 2 条】（系统通知：后台任务完成——D-OK）\n【第 3 条】用户追加四"
	if msg != want {
		t.Fatalf("msg = %q\nwant %q", msg, want)
	}
	if kind != "merged" || n != 3 {
		t.Fatalf("kind=%q n=%d，want merged/3", kind, n)
	}
	wantLabel := fmt.Sprintf("3 条合并 / %d 字符",
		utf16Count(msgs[0])+utf16Count(msgs[1])+utf16Count(msgs[2]))
	if label == nil || *label != wantLabel {
		t.Fatalf("label=%v，want %q", label, wantLabel)
	}
}
