package main

import (
	"os"
	"strings"
	"testing"

	"okhuman/internal/persist"
	"okhuman/internal/types"
)

// 会话快照写失败必须留痕（复审指出这条没有回归保护）。此前两处都是 `_ = p.SaveSession(...)`：
// 盘满或目录只读时会话再也不更新，日志里一个字都没有。
func TestPersistSaveFailureIsLogged(t *testing.T) {
	dir := t.TempDir()
	p, err := persist.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(m string) { logged = append(logged, m) }

	// 目录改成只读 → SaveSession 必失败
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	save := newPersistSave(p, func() int { return 7 }, 7, logf)
	save(&types.SessionState{Messages: []types.RawEntry{{Role: "user", Content: "x"}}}, 1)

	if len(logged) == 0 || !strings.Contains(logged[0], "会话快照写入失败") {
		t.Fatalf("写失败没有留痕：%v", logged)
	}
	if !strings.Contains(logged[0], "下一次变更会再试") {
		t.Errorf("日志应说明会重试：%q", logged[0])
	}
}
