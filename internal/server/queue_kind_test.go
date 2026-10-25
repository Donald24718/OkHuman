package server

// 队列内部结构化（types.QueuedMessage）之后，**对外契约必须一字不变**：
// /queue 与 /events hello 里的 queued 仍是字符串数组——qq-channel、voice-chat、
// webui 都在读它，不能因为内部加了 Kind 字段就把协议改了。
import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

func TestQueuedTextsProjection(t *testing.T) {
	if got := queuedTexts(nil); got != nil {
		t.Errorf("空队列必须投影成 nil（旧 append([]string(nil)) 同行为，JSON 是 null 不是 []），实际 %v", got)
	}
	q := []types.QueuedMessage{
		{Text: "用户正文", Kind: types.QueueKindUser},
		{Text: "（系统通知：后台任务完成——X", Kind: types.QueueKindBGNotice},
	}
	got := queuedTexts(q)
	if len(got) != 2 || got[0] != "用户正文" || got[1] != "（系统通知：后台任务完成——X" {
		t.Fatalf("投影必须只取正文、顺序不变，实际 %v", got)
	}
}

// TestQueueEndpointCarriesKind：/queue 新增 queued_items（text + kind），
// webui 据此分气泡；旧 queued 字段同时保留，两者内容必须一致。
func TestQueueEndpointCarriesKind(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	srv := httptest.NewServer(New(st, nil))
	defer srv.Close()

	st.mu.Lock()
	st.MessageQueue = []types.QueuedMessage{
		{Text: "用户正文", Kind: types.QueueKindUser},
		{Text: "（系统通知：后台任务完成——X", Kind: types.QueueKindBGNotice},
	}
	st.mu.Unlock()

	resp, err := srv.Client().Get(srv.URL + "/queue")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Queued      []string              `json:"queued"`
		QueuedItems []types.QueuedMessage `json:"queued_items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解码：%v", err)
	}
	if len(out.QueuedItems) != 2 {
		t.Fatalf("queued_items 应有 2 条：%v", out.QueuedItems)
	}
	if out.QueuedItems[0].Kind != types.QueueKindUser || out.QueuedItems[1].Kind != types.QueueKindBGNotice {
		t.Errorf("kind 必须随队列带出（前端不再靠猜）：%+v", out.QueuedItems)
	}
	if out.QueuedItems[1].Text != "（系统通知：后台任务完成——X" {
		t.Errorf("text 不对：%q", out.QueuedItems[1].Text)
	}
	if len(out.Queued) != 2 || out.Queued[1] != out.QueuedItems[1].Text {
		t.Errorf("旧 queued 字段必须保留且与 items 正文一致：%v", out.Queued)
	}
}

func TestQueueEndpointEmptyQueue(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	srv := httptest.NewServer(New(st, nil))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/queue")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解码：%v", err)
	}
	if v, ok := out["queued"]; !ok || v != nil {
		t.Errorf("空队列 queued 必须是 null（与改动前一致），实际 %v", out["queued"])
	}
	if v, ok := out["queued_items"]; ok && v != nil {
		t.Errorf("空队列 queued_items 也应是 null，实际 %v", v)
	}
}

func TestQueueEndpointStillReturnsStrings(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	srv := httptest.NewServer(New(st, nil))
	defer srv.Close()

	st.mu.Lock()
	st.MessageQueue = []types.QueuedMessage{
		{Text: "用户正文", Kind: types.QueueKindUser},
		{Text: "（系统通知：后台任务完成——X", Kind: types.QueueKindBGNotice},
	}
	st.mu.Unlock()

	resp, err := srv.Client().Get(srv.URL + "/queue")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("code=%d", resp.StatusCode)
	}
	// 严格解成 []string：queued 一旦变成对象数组，这里就解码失败
	var out struct {
		Queued []string `json:"queued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("queued 必须仍是字符串数组（对外协议不变）：%v", err)
	}
	if len(out.Queued) != 2 || out.Queued[0] != "用户正文" {
		t.Fatalf("queued 内容不对：%v", out.Queued)
	}
}
