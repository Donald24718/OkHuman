package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

func hasLine(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// 修 D（2026-10-18）：关键操作必须进落盘日志。此前这两处用 fmt.Printf，只进
// stdout 不进 okhuman.log —— 事后查不到"剥过附件""重置前存过全文"这些动作。
func TestKeyOpsWriteToLog(t *testing.T) {
	cfg := config.Default()
	dir := t.TempDir()
	cfg.Data.Dir = dir
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, dir, a)
	var lines []string
	st.Log = func(s string) { lines = append(lines, s) }
	ap := New(st, nil)

	a.CM().AddMessage(&types.RawEntry{Role: "user", Content: "留痕测试"})
	var out map[string]interface{}
	if code := postJSON(ap, "/reset", map[string]interface{}{}, &out); code != 200 {
		t.Fatalf("/reset code=%d body=%v", code, out)
	}
	if !hasLine(lines, "[reset] 旧会话全文已落盘") {
		t.Fatalf("/reset 没写落盘日志，lines=%q", lines)
	}

	injDir := filepath.Join(dir, "inject")
	if err := os.MkdirAll(injDir, 0o700); err != nil {
		t.Fatal(err)
	}
	parts, _ := json.Marshal([]interface{}{map[string]interface{}{"type": "text", "text": "hi"}})
	if err := os.WriteFile(filepath.Join(injDir, "inj-1.json"), parts, 0o600); err != nil {
		t.Fatal(err)
	}
	lines = nil
	if code := postJSON(ap, "/strip-injects", map[string]interface{}{}, &out); code != 200 {
		t.Fatalf("/strip-injects code=%d body=%v", code, out)
	}
	if !hasLine(lines, "[inject] 手动剥离附件 1 个") {
		t.Fatalf("/strip-injects 没写落盘日志，lines=%q", lines)
	}
}

// Log 未注入（测试路径）时不得 panic。
func TestLogfNilSafe(t *testing.T) {
	st := NewAppState(config.Default(), t.TempDir(), nil)
	st.logf("不该 panic：%s", "nil logger")
}
