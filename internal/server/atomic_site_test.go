package server

import (
	"os"
	"path/filepath"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

// TestPromptSaveAtomic 提示词保存改原子写（C2）：内容精确、无 .tmp-* 残留、
// 原有权限保持。提示词是行为约束文件——半截 = 下次加载失败或规则静默缺失，
// 比丢一条消息危险得多。
func TestPromptSaveAtomic(t *testing.T) {
	promptDir := t.TempDir()
	// 预置一个 0600 的同名文件：rename 不该把权限悄悄改成 0600/0644 之外的值
	target := filepath.Join(promptDir, "01-identity.md")
	if err := os.WriteFile(target, []byte("旧内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, promptDir)
	ap := New(NewAppState(cfg, t.TempDir(), a), nil)

	var out map[string]interface{}
	code := postJSON(ap, "/prompts/file", map[string]interface{}{
		"name": "01-identity.md", "content": "新提示词内容\n第二行",
	}, &out)
	if code != 200 {
		t.Fatalf("提示词保存被拒：code=%d body=%v", code, out)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "新提示词内容\n第二行" {
		t.Errorf("内容不精确：%q", b)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o600 {
		t.Errorf("原有权限 0600 应保持，实际 %s", st.Mode().Perm())
	}
	ms, _ := filepath.Glob(filepath.Join(promptDir, "*.tmp-*"))
	if len(ms) != 0 {
		t.Errorf("留下 .tmp-* 残留：%v", ms)
	}

	// 非 .md 分支（第二个写点）
	code = postJSON(ap, "/prompts/file", map[string]interface{}{
		"name": "notes.txt", "content": "纯文本",
	}, &out)
	if code != 200 {
		t.Fatalf("非 .md 保存被拒：code=%d", code)
	}
	b, _ = os.ReadFile(filepath.Join(promptDir, "notes.txt"))
	if string(b) != "纯文本" {
		t.Errorf("非 .md 内容不精确：%q", b)
	}
	ms, _ = filepath.Glob(filepath.Join(promptDir, "*.tmp-*"))
	if len(ms) != 0 {
		t.Errorf("非 .md 分支留下残留：%v", ms)
	}
}
