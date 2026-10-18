package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

// TestAccessLogRecordsEndpoints（2026-10-18 复查修 3）：实例以前根本不记请求路径，
// "有没有人在用某个端点"这类问题无法回答。现在只读端点必须留痕；高频的
// /events/poll（WebUI 每 200ms 一次）不得逐条记，否则日志被刷爆。
func TestAccessLogRecordsEndpoints(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	var mu sync.Mutex
	var lines []string
	st.Log = func(m string) { mu.Lock(); lines = append(lines, m); mu.Unlock() }
	ap := New(st, nil)
	srv := httptest.NewServer(ap)
	defer srv.Close()

	for _, p := range []string{"/health", "/status", "/queue"} {
		res, err := srv.Client().Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	for i := 0; i < 3; i++ {
		res, err := srv.Client().Get(srv.URL + "/events/poll?since=-1")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"[http] GET /health → 200", "[http] GET /status → 200", "[http] GET /queue → 200"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("访问日志缺 %q，实得：\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "/events/poll") {
		t.Fatalf("高频 /events/poll 不该逐条进日志：\n%s", joined)
	}
}

// TestConfigSaveFailureIsReported（2026-10-18 复查修 1）：落盘失败时不能再说"已保存"。
// 内存生效与落盘成功是两件事，必须分开表态——否则盘满/只读/权限变了时，用户以为
// 改动扛得住重启，重启后配置回退且日志里查不到任何痕迹。
func TestConfigSaveFailureIsReported(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	var logged []string
	st.Log = func(m string) { logged = append(logged, m) }
	ap := New(st, &Hooks{OnConfigSaved: func(patch map[string]interface{}) error {
		return errors.New("read-only file system")
	}})
	srv := httptest.NewServer(ap)
	defer srv.Close()

	body, _ := json.Marshal(map[string]interface{}{"patch": map[string]interface{}{"llm": map[string]interface{}{"model": "新模型"}}})
	res, err := srv.Client().Post(srv.URL+"/config", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if out["ok"] != true {
		t.Fatalf("内存生效应 ok:true，得 %v", out)
	}
	if out["saved"] != false {
		t.Fatalf("落盘失败必须 saved=false，得 %v", out["saved"])
	}
	if out["save_error"] != "read-only file system" {
		t.Fatalf("save_error 缺失或不对：%v", out["save_error"])
	}
	if s, _ := out["warning"].(string); !strings.Contains(s, "重启后会回到旧值") {
		t.Fatalf("warning 没写清后果：%v", out["warning"])
	}
	if !strings.Contains(strings.Join(logged, "\n"), "落盘 config/user.json 失败") {
		t.Fatalf("落盘失败必须进日志，实得：%v", logged)
	}
	// 成功路径：saved=true 且不带 warning
	ap2 := New(st, &Hooks{OnConfigSaved: func(patch map[string]interface{}) error { return nil }})
	srv2 := httptest.NewServer(ap2)
	defer srv2.Close()
	res2, err := srv2.Client().Post(srv2.URL+"/config", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out2 map[string]interface{}
	json.NewDecoder(res2.Body).Decode(&out2)
	res2.Body.Close()
	if out2["saved"] != true || out2["warning"] != nil {
		t.Fatalf("成功路径应 saved=true 且无 warning，得 %v", out2)
	}
}
