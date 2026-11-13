// 段内写错字的键必须挡在门外（2026-11-13 四方审计 E3）：此前未知键被原样接受并落进
// user.json，用户以为改了，其实什么都没生效。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

func TestConfigRejectsUnknownKeyInKnownSection(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	ap := New(NewAppState(cfg, t.TempDir(), a), nil)

	post := func(patch map[string]interface{}) (int, string) {
		body, _ := json.Marshal(map[string]interface{}{"patch": patch})
		req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(string(body)))
		req.Host = "localhost:" + itoa(cfg.Server.Port)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ap.ServeHTTP(rec, req)
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		msg, _ := out["error"].(string)
		return rec.Code, msg
	}
	if code, msg := post(map[string]interface{}{"context": map[string]interface{}{"keep_recent_char": 123}}); code != 400 || !strings.Contains(msg, "context.keep_recent_char") {
		t.Errorf("打错字的键应被拒绝并点名：code=%d msg=%s", code, msg)
	}
	if code, msg := post(map[string]interface{}{"context": map[string]interface{}{"keep_recent_chars": 12345}}); code != 200 {
		t.Errorf("合法键不该被拒：code=%d msg=%s", code, msg)
	}
	// 旧 *_ms 别名在换算之后校验，不能被当成未知键
	if code, msg := post(map[string]interface{}{"tools": map[string]interface{}{"timeout_ms": 60000}}); code != 200 {
		t.Errorf("旧 *_ms 别名应被换算接受：code=%d msg=%s", code, msg)
	}
}
