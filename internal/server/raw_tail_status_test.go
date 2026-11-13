package server

// /status 的 raw_tail 每条 chars 必须用会话那把尺子（context.Est.MessageChars），
// 不是写死的 1000、也不是字节数。2026-11-13 两份独立复审都指出：这个字段当时没有任何
// 测试钉着——把实现改回"写死 1000"或"只按字节"，全套测试仍绿。这里补上。

import (
	"net/http/httptest"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
	"okhuman/internal/textutil"
)

func TestStatusRawTailCharsUseTheRealMeasure(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM([]llm.FakeStep{
		{Type: "text", Text: "收到"},
	}), "sys", nil, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	srv := httptest.NewServer(New(st, nil))
	defer srv.Close()

	// 含中文：按 UTF-16 单元应 = 字面个数；按字节会 ×3；旧实现直接写死 1000。
	userMsg := "中文计数验证，一条含中文的用户消息"
	if got := runTurn(t, srv, userMsg); got != "收到" {
		t.Fatalf("假 LLM 回复不对：%q", got)
	}

	var status map[string]interface{}
	if err := getJSONURL(srv, "/status", &status); err != nil {
		t.Fatal(err)
	}
	tail, _ := status["raw_tail"].([]interface{})
	if len(tail) < 2 {
		t.Fatalf("raw_tail 应至少两条（user + assistant），实得 %d", len(tail))
	}
	wantUser := textutil.UTF16Len(userMsg)
	seenUser, seenAssistant := false, false
	for _, it := range tail {
		m, _ := it.(map[string]interface{})
		role, _ := m["role"].(string)
		chars, _ := m["chars"].(float64)
		switch role {
		case "user":
			seenUser = true
			if int(chars) != wantUser {
				t.Errorf("user 那条 chars=%d，应为 UTF-16 单元数 %d（字节数会是 %d，旧实现写死 1000）",
					int(chars), wantUser, len(userMsg))
			}
		case "assistant":
			seenAssistant = true
			if int(chars) != textutil.UTF16Len("收到") {
				t.Errorf("assistant 那条 chars=%d，应为 %d", int(chars), textutil.UTF16Len("收到"))
			}
		}
	}
	if !seenUser || !seenAssistant {
		t.Fatalf("raw_tail 里没同时看到 user 与 assistant：%v", tail)
	}
}
