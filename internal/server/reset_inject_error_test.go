// /reset 清不掉收件目录时必须看得见，且不把附件计数归零（否则新附件从 inj-1 起
// 盖住残留的旧侧车 —— 2026-11-13 四方审计 #16）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

func TestResetReportsInjectCleanupFailure(t *testing.T) {
	cfg := config.Default()
	dir := t.TempDir()
	cfg.Data.Dir = dir
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, dir, a)
	ap := New(st, nil)

	id := ap.injectDirOf()
	locked := filepath.Join(id, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil { // 只读子目录 → RemoveAll 删不动
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	ap.state.mu.Lock()
	ap.state.InjectSeq = 7 // 预置：删不掉就不许归零
	ap.state.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/reset", nil)
	req.Host = "localhost:" + itoa(cfg.Server.Port) // 过 Host 白名单（见 origin_guard_test）
	w := httptest.NewRecorder()
	ap.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// 删除失败时计数**不能**归零：否则新会话的附件从 inj-1 重新编号，盖住删不掉的旧侧车。
	// （变异检验：只断言 inject_error 时"无条件归零"也全绿 → 这半没有钉。）
	ap.state.mu.Lock()
	seq := ap.state.InjectSeq
	ap.state.mu.Unlock()
	if seq != 7 {
		t.Errorf("收件目录删不掉时 InjectSeq 被归零了：应为 7，实际 %d", seq)
	}
	msg, _ := out["inject_error"].(string)
	if msg == "" {
		t.Errorf("删不掉的收件目录必须在响应里可见：%s", w.Body.String())
	}
}
