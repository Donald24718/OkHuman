package prompt

import (
	"os"
	"path/filepath"
	"testing"
)

// 提示词文件读不了时必须回退内置默认，不能交出一个空提示词。
func TestUnreadablePromptFallsBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视 000 权限，测不出来")
	}
	dir := t.TempDir()
	name := filepath.Join(dir, "01-identity.md")
	if err := os.WriteFile(name, []byte("身份提示词"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(name, 0o600)

	got := LoadPromptDir(dir)
	if got.Prompt == "" || !got.Fallback {
		t.Fatalf("读不了提示词时应回退内置默认；实际 prompt=%d 字符 fallback=%v files=%d",
			len([]rune(got.Prompt)), got.Fallback, len(got.Files))
	}
}
