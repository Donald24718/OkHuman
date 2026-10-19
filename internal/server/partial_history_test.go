package server

// 端到端验证（2026-10-19）：流被掐断后入账的 partial 必须出现在 /history 的 entries 里
// ——前端 renderHistoryEntry 对 role=assistant 走 chatBotHtml，所以它既进下一轮请求，
// 也会在刷新/重连后画成 bot 气泡，不再"当时看见过、刷新就没了"。
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"okhuman/internal/config"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

type cutClient struct{}

func (cutClient) Complete(context.Context, []types.Message, []types.ToolSpec) (*types.Response, error) {
	return nil, errors.New("unused")
}
func (cutClient) CompleteStream(_ context.Context, _ []types.Message, _ []types.ToolSpec, onDelta func(types.Delta), _ int) (*types.Response, error) {
	onDelta(types.Delta{Thinking: "先想一段"})
	onDelta(types.Delta{Text: "半截正文ABC"})
	return nil, errors.New("上游流被掐断")
}

func sleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func TestPartialShowsInHistory(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, cutClient{}, "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	srv := httptest.NewServer(New(st, nil))
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/chat", "application/json", strings.NewReader(`{"message":"问"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("/chat 响应: %s", body)

	sleep(300)
	hresp, err := srv.Client().Get(srv.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		Entries []map[string]interface{} `json:"entries"`
	}
	if err := json.NewDecoder(hresp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	hresp.Body.Close()
	for i, m := range h.Entries {
		t.Logf("history[%d] role=%v content=%v reasoning=%v", i, m["role"], m["content"], m["reasoning_content"])
	}
	var found bool
	for _, m := range h.Entries {
		if m["role"] == "assistant" {
			if s, ok := m["content"].(string); ok && strings.Contains(s, "半截正文ABC") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("partial 未出现在 /history → 前端不会画出来")
	}
	_ = llm.FakeLLM{}
}
