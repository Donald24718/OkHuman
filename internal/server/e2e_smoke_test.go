package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

// TestHTTPEndToEnd（2026-09-14 审计修配套冒烟）：FakeLLM 驱动全链路——
// /chat（队列 drain）→ /status → /config POST（COW 热更）→ /config GET（新值）
// → /inject（侧车）→ /chat（resolve 注入）→ /reset（会话 2，三件套原子换入）
// → /history（空）→ /chat（新会话续跑）。-race 下抓 COW/reset/队列路径的竞态。
func TestHTTPEndToEnd(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	fake := llm.NewFakeLLM([]llm.FakeStep{
		{Type: "text", Text: "回复一"},
		{Type: "text", Text: "回复二"},
		{Type: "text", Text: "回复三"},
	})
	a := CreateAgentState(cfg, fake, "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	ap := New(st, nil)
	srv := httptest.NewServer(ap)
	defer srv.Close()

	do := func(method, path string, body interface{}, out interface{}) int {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("%s %s 解码: %v", method, path, err)
			}
		}
		return resp.StatusCode
	}

	// 1. /chat → 队列 drain → 回复一
	var chat map[string]interface{}
	if code := do(http.MethodPost, "/chat", map[string]interface{}{"message": "你好"}, &chat); code != 200 {
		t.Fatalf("/chat code=%d body=%v", code, chat)
	}
	if chat["reply"] != "回复一" {
		t.Fatalf("/chat reply=%v want 回复一", chat["reply"])
	}

	// 2. /status：session=1、历史 2 条（user+assistant）、system_prompt fallback
	var status map[string]interface{}
	if code := do(http.MethodGet, "/status", nil, &status); code != 200 {
		t.Fatalf("/status code=%d", code)
	}
	if status["session"] != float64(1) {
		t.Fatalf("status session=%v want 1", status["session"])
	}

	// 3. /config POST：tools 段 COW 热更（不打 llm 段——llm 段会重建为真实
	// OpenAI 客户端，FakeLLM 驱动即失效；llm 段重建路径由单测/真实环境覆盖）
	var patchResp map[string]interface{}
	if code := do(http.MethodPost, "/config", map[string]interface{}{
		"patch": map[string]interface{}{"tools": map[string]interface{}{
			"result_limit": 999999,
		}},
	}, &patchResp); code != 200 {
		t.Fatalf("/config POST code=%d body=%v", code, patchResp)
	}

	// 4. /config GET：effective 已是新值（COW 换入生效）
	var cfgGet map[string]interface{}
	if code := do(http.MethodGet, "/config", nil, &cfgGet); code != 200 {
		t.Fatalf("/config GET code=%d", code)
	}
	eff, _ := cfgGet["effective"].(map[string]interface{})
	if toolsSec, _ := eff["tools"].(map[string]interface{}); toolsSec["result_limit"] != float64(999999) {
		t.Fatalf("effective.tools.result_limit=%v want 999999（COW 热更未生效）", toolsSec["result_limit"])
	}

	// 5. /inject：侧车落盘 + inject_ref 入会话
	var injResp map[string]interface{}
	if code := do(http.MethodPost, "/inject", map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "附件内容"}},
		"note":    "测试附件",
	}, &injResp); code != 200 {
		t.Fatalf("/inject code=%d body=%v", code, injResp)
	}
	injID, _ := injResp["id"].(string)
	if injID == "" {
		t.Fatalf("/inject 无 id: %v", injResp)
	}
	var att map[string]interface{}
	do(http.MethodGet, "/attachments", nil, &att)
	if injects, _ := att["injects"].([]interface{}); len(injects) != 1 {
		t.Fatalf("/attachments injects=%v want 1 条", injects)
	}

	// 6. /chat：compose 经 resolver 解析 inject_ref（侧车读取路径）
	if code := do(http.MethodPost, "/chat", map[string]interface{}{"message": "看看附件"}, &chat); code != 200 {
		t.Fatalf("/chat(2) code=%d", code)
	}
	if chat["reply"] != "回复二" {
		t.Fatalf("/chat(2) reply=%v want 回复二", chat["reply"])
	}

	// 7. /reset：会话 2，运行时三件套原子换入
	var resetResp map[string]interface{}
	if code := do(http.MethodPost, "/reset", nil, &resetResp); code != 200 {
		t.Fatalf("/reset code=%d body=%v", code, resetResp)
	}
	if resetResp["session"] != float64(2) {
		t.Fatalf("reset session=%v want 2", resetResp["session"])
	}

	// 8. /history：新会话空
	var hist map[string]interface{}
	if code := do(http.MethodGet, "/history", nil, &hist); code != 200 {
		t.Fatalf("/history code=%d", code)
	}
	if hist["session"] != float64(2) {
		t.Fatalf("history session=%v want 2", hist["session"])
	}
	if entries, _ := hist["entries"].([]interface{}); len(entries) != 0 {
		t.Fatalf("history entries=%d want 0（reset 后应为空会话）", len(entries))
	}

	// 9. /chat：新会话继续跑（换入后的 CM/Agent 工作正常）
	if code := do(http.MethodPost, "/chat", map[string]interface{}{"message": "重置后"}, &chat); code != 200 {
		t.Fatalf("/chat(3) code=%d", code)
	}
	if chat["reply"] != "回复三" {
		t.Fatalf("/chat(3) reply=%v want 回复三", chat["reply"])
	}

	// 10. /queue + /backgrounds：reset 后计数归零
	var q map[string]interface{}
	do(http.MethodGet, "/queue", nil, &q)
	if q["user_seq"] != float64(1) {
		t.Fatalf("queue user_seq=%v want 1（新会话一条 user 消息）", q["user_seq"])
	}
	var bg map[string]interface{}
	if code := do(http.MethodGet, "/backgrounds", nil, &bg); code != 200 {
		t.Fatalf("/backgrounds code=%d", code)
	}
	if bg["count"] != float64(0) {
		t.Fatalf("backgrounds count=%v want 0", bg["count"])
	}
}
