package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// user.json 损坏时必须拒绝写，不能拿空配置当基线继续合并——
// 那会把主人原有的全部配置（端口、模型、上下文上限）静默清空。
func TestSaveConfigRefusesCorruptUserJson(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "user.json")
	// 半截 JSON（外部编辑中断、掉电写一半）
	if err := os.WriteFile(file, []byte(`{"server":{"port":8451},"llm":{"model":"keep-me"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := map[string]interface{}{"context": map[string]interface{}{"max_tokens": 1}}
	err := saveGlobalConfigPatch(root, patch)
	if err == nil {
		b, _ := os.ReadFile(file)
		t.Fatalf("损坏的 user.json 被当成空基线继续写，原配置已丢；现在文件内容：%s", strings.TrimSpace(string(b)))
	}
	t.Logf("已拒绝写入：%v", err)
}

// 文件不存在（首次保存）应当正常写，别把正常路径也堵死
func TestSaveConfigAllowsMissingUserJson(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveGlobalConfigPatch(root, map[string]interface{}{"context": map[string]interface{}{"max_tokens": 123456}}); err != nil {
		t.Fatalf("user.json 不存在时应可正常保存：%v", err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "config", "user.json"))
	if !strings.Contains(string(b), "123456") {
		t.Fatalf("补丁没写进去：%s", string(b))
	}
}
