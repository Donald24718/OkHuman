package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"okhuman/internal/textutil"
)

// files[].Chars 必须与 /status 里 system_prompt.chars 同口径（UTF-16）。此前这里是 rune 数，
// 同一个响应里两个字段口径不同（2026-11-12 审查发现）。
func TestPromptFileCharsUseUTF16(t *testing.T) {
	d := t.TempDir()
	text := "你好世界 😀 结束" // 中文 4 + emoji（代理对）+ ASCII
	if err := os.WriteFile(filepath.Join(d, "01-a.md"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	ld := LoadPromptDir(d)
	if len(ld.Files) != 1 {
		t.Fatalf("应读到 1 个文件，得 %d（unreadable=%v）", len(ld.Files), ld.Unreadable)
	}
	if got := ld.Files[0].Chars; got != textutil.UTF16Len(text) {
		t.Errorf("Chars = %d，期望 UTF-16 的 %d（rune 数会是 %d）", got, textutil.UTF16Len(text), len([]rune(text)))
	}
}
