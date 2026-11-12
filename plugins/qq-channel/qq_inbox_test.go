package main

// 入站附件按实例分一口（2026-11-12）。测的是"一口一实例"这件事本身：
// 同一实例的两个 bot 共用一口（对），不同实例必须分开（这就是原 bug），没写实例退回 app_id。
import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInboxDirIsPerInstance(t *testing.T) {
	a := inboxDirFor(BotCfg{AppID: "111", Instance: "default"})
	b := inboxDirFor(BotCfg{AppID: "222", Instance: "second"})
	same := inboxDirFor(BotCfg{AppID: "333", Instance: "default"})
	if a == b {
		t.Fatalf("不同实例共用了收件口: %s", a)
	}
	if a != same {
		t.Fatalf("同一实例应共用一口: %s vs %s", a, same)
	}
	for _, want := range []string{"/tmp/qq-inbox/default", "/tmp/qq-inbox/second"} {
		if a != want && b != want {
			t.Fatalf("少了 %s（实得 %s / %s）", want, a, b)
		}
	}
}

func TestInboxDirFallsBackToAppIDAndSanitizes(t *testing.T) {
	if got := inboxDirFor(BotCfg{AppID: "900"}); got != "/tmp/qq-inbox/900" {
		t.Fatalf("没写实例时应退回 app_id，得到 %s", got)
	}
	if got := inboxDirFor(BotCfg{Instance: "a b/../c"}); got != "/tmp/qq-inbox/a_b_.._c" {
		t.Fatalf("实例 id 未消毒: %s", got)
	}
	if got := inboxDirFor(BotCfg{Instance: ".."}); got != "/tmp/qq-inbox/default" {
		t.Fatalf("消毒后只剩点必须换掉，否则又合回一口: %s", got)
	}
	if got := inboxDirFor(BotCfg{}); got != "/tmp/qq-inbox/default" {
		t.Fatalf("什么都没有时应落到 default，得到 %s", got)
	}
}

func TestPruneInboxOnlyDropsStale(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "fresh.jpg")
	old := filepath.Join(dir, "old.jpg")
	sub := filepath.Join(dir, "keepme")
	for _, f := range []string{fresh, old} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ago := time.Now().Add(-26 * time.Hour) // 超过 1 天就该被清
	if err := os.Chtimes(old, ago, ago); err != nil {
		t.Fatal(err)
	}
	c := &QQChannel{logf: func(string) {}}
	c.pruneInbox(dir)
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("新文件被删了: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("过期文件没被删")
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatalf("子目录被动了: %v", err)
	}
}
