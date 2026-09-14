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

// TestSubscriberIsolation（2026-09-14 审计修回归）：/events 订阅按唯一 id 摘除，
// 断开一个连接不得误杀其他连接——旧实现用 reflect 代码指针判同，同字面量闭
// 共享指针 → 任一断开把全部订阅一并摘掉（前端全瞎）。
func TestSubscriberIsolation(t *testing.T) {
	st := &AppState{}
	id1 := subSeq.Add(1)
	id2 := subSeq.Add(2)
	st.mu.Lock()
	st.BroadcastSubs = append(st.BroadcastSubs, subEntry{ID: id1, Fn: func(string) {}})
	st.BroadcastSubs = append(st.BroadcastSubs, subEntry{ID: id2, Fn: func(string) {}})
	st.mu.Unlock()
	// 模拟 handleEvents 断开清理：按 id 摘除 id1
	st.mu.Lock()
	kept := st.BroadcastSubs[:0]
	for _, s := range st.BroadcastSubs {
		if s.ID != id1 {
			kept = append(kept, s)
		}
	}
	st.BroadcastSubs = kept
	st.mu.Unlock()
	if len(st.BroadcastSubs) != 1 || st.BroadcastSubs[0].ID != id2 {
		t.Fatalf("误杀：摘除 id1 后应只剩 id2，got %d 条", len(st.BroadcastSubs))
	}
	// 重复摘除同一 id 不得影响其他订阅（幂等）
	st.mu.Lock()
	kept = st.BroadcastSubs[:0]
	for _, s := range st.BroadcastSubs {
		if s.ID != id1 {
			kept = append(kept, s)
		}
	}
	st.BroadcastSubs = kept
	st.mu.Unlock()
	if len(st.BroadcastSubs) != 1 || st.BroadcastSubs[0].ID != id2 {
		t.Fatalf("幂等摘除后 id2 丢失")
	}
}
