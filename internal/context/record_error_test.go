package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"okhuman/internal/types"
)

// 存档/总结写失败必须返回真错误（旧实现返回 "[记录写入失败]" 这个魔法字符串，
// 被 5 处字符串比较消费——改文案就静默失效，错误原因也丢了）。
func TestRecordWriteFailureReturnsError(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, Cfg{CharsPerToken: 1}, "sys", blocker) // recordDir 的父是普通文件 → 建不出来
	m.session.Messages = []types.RawEntry{{Role: "user", Content: "一条"}}
	p, err := m.writeSessionRecordLocked("2026-11-12_00-00-00-000Z-seq1", "测试")
	if err == nil || p != "" {
		t.Fatalf("写不出目录时应 (\"\", error)，得 (%q, %v)", p, err)
	}
	if !strings.Contains(err.Error(), "建目录") {
		t.Errorf("错误要说明失败在哪：%q", err)
	}
	sp, serr := m.writeSummaryFileLocked("2026-11-12_00-00-00-000Z-seq1", "总结", 10, false, 10, "")
	if serr == nil || sp != "" {
		t.Fatalf("总结同样应返回错误，得 (%q, %v)", sp, serr)
	}
}
