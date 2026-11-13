package persist

import (
	"os"
	"path/filepath"
	"testing"
)

// WritePath/WriteJSONPath 是九个写点的共同入口：必须顺手建目录、必须原子、
// 且错误如实返回（此前调用方各自 open-code，三处把 error 吞了）。
func TestWritePathCreatesDirAndReportsErrors(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c.log")
	if err := WritePath(nested, "全文"); err != nil {
		t.Fatalf("应能顺手建目录：%v", err)
	}
	b, err := os.ReadFile(nested)
	if err != nil || string(b) != "全文" {
		t.Fatalf("内容不对：%q %v", b, err)
	}
	// 父路径是个普通文件 → 建目录必然失败，且要带原因
	blocker := filepath.Join(root, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = WritePath(filepath.Join(blocker, "x.log"), "y")
	if err == nil {
		t.Fatal("建目录失败必须返回错误")
	}
	if got := err.Error(); got == "" || !contains(got, "建目录") {
		t.Errorf("错误要说明失败在哪：%q", got)
	}
}

func TestWriteJSONPathReportsMarshalError(t *testing.T) {
	root := t.TempDir()
	if err := WriteJSONPath(filepath.Join(root, "ok.json"), map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "ok.json"))
	if !contains(string(b), "\"a\": 1") {
		t.Errorf("应是缩进 2 的 JSON：%s", b)
	}
	// channel 不能序列化 → 必须报错，而不是写出空文件
	if err := WriteJSONPath(filepath.Join(root, "bad.json"), make(chan int)); err == nil {
		t.Error("序列化失败必须返回错误（调用方原来写的是 out, _ := json.MarshalIndent）")
	}
	if _, err := os.Stat(filepath.Join(root, "bad.json")); !os.IsNotExist(err) {
		t.Error("序列化失败时不该留下半个文件")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
