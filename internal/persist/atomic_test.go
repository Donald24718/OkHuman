package persist

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func residues(t *testing.T, dir string) []string {
	t.Helper()
	ms, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// TestAtomicWriteFilePreservesMode rename 会把 CreateTemp 的 0600 带进目标文件，
// 不显式 Chmod 就悄悄收紧权限（session.json 曾因此变 0600）。
func TestAtomicWriteFilePreservesMode(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{"exist600", 0o600}, {"exist640", 0o640}, {"exist644", 0o644},
	} {
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, []byte("old"), tc.mode); err != nil {
			t.Fatal(err)
		}
		if err := AtomicWriteFile(p, "new"); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != tc.mode {
			t.Errorf("%s：权限应变 %s，实际 %s", tc.name, tc.mode, st.Mode().Perm())
		}
	}
	// 新建文件 → 0644
	p := filepath.Join(dir, "fresh")
	if err := AtomicWriteFile(p, "x"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Errorf("新建文件应 0644，实际 %s", st.Mode().Perm())
	}
}

// TestAtomicWriteFileNoResidueAndFullOverwrite 成功路径不留 .tmp-*；短内容覆盖长内容
// 不留旧尾巴（长度必须等于新内容）。
func TestAtomicWriteFileNoResidueAndFullOverwrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := AtomicWriteFile(p, strings.Repeat("A", 5000)); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(p, "短"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "短" {
		t.Errorf("覆盖不彻底：%d 字节：%q", len(b), b)
	}
	if r := residues(t, dir); len(r) != 0 {
		t.Errorf("留下临时文件残留：%v", r)
	}
}

// TestAtomicWriteFileFailureKeepsOriginal 写不进去（目录只读）时必须：报错、
// 原文件原封不动、不留残留。半截覆盖比写失败严重得多。
func TestAtomicWriteFileFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(p, []byte("原始内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skipf("无法设只读目录：%v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	if err := AtomicWriteFile(p, "新内容"); err == nil {
		t.Error("只读目录下的写入应失败")
	}
	_ = os.Chmod(dir, 0o755)
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "原始内容" {
		t.Errorf("失败后原文件被改动：%q err=%v", b, err)
	}
	if r := residues(t, dir); len(r) != 0 {
		t.Errorf("失败路径留下残留：%v", r)
	}
}

// TestAtomicWriteFileConcurrent 并发写同一文件：最终内容必须是某一次完整写入
// （rename 原子性），不能出现两次内容交织；且不留残留。
func TestAtomicWriteFileConcurrent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.txt")
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := strings.Repeat(string(rune('a'+i%26)), 2000)
			if err := AtomicWriteFile(p, payload); err != nil {
				t.Errorf("并发写失败：%v", err)
			}
		}(i)
	}
	wg.Wait()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 2000 {
		t.Errorf("内容被交织：%d 字节", len(b))
	}
	uniq := map[rune]bool{}
	for _, r := range string(b) {
		uniq[r] = true
	}
	if len(uniq) != 1 {
		t.Errorf("内容不是单次完整写入（出现混合）：%q", string(b)[:40])
	}
	if r := residues(t, dir); len(r) != 0 {
		t.Errorf("并发后留下残留：%v", r)
	}
}
