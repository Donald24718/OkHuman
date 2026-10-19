package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

// TestAccessLogRecordsEndpoints（2026-10-18 复查修 3 + 同日实测修正）：写操作、
// 非 2xx、非轮询 GET 必须逐条留痕；高频轮询（/queue /status /health /events/poll）
// 不得逐条记——实测启动 4 分钟就有 196 行访问日志，把要查的东西全冲掉了。
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

	// 高频端点连打 200 次：汇总行按时间窗（一小时）触发，不看次数，所以这里
	// 一行都不该出现（证明不是"每 N 次一行"那种按次数的老写法）。
	for i := 0; i < 200; i++ {
		for _, p := range []string{"/queue", "/status", "/health", "/history", "/events/poll?since=-1"} {
			res, err := srv.Client().Get(srv.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
		}
	}
	// 非轮询 GET（/config）与 404 必须逐条记
	for _, p := range []string{"/config", "/no-such-endpoint"} {
		res, err := srv.Client().Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	// 写操作必须逐条记（哪怕状态正常）
	res, err := srv.Client().Post(srv.URL+"/enqueue", "application/json", strings.NewReader(`{"bad":true}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"[http] GET /config → 200", "[http] GET /no-such-endpoint → 404", "[http] POST /enqueue → 400",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("访问日志缺 %q，实得：\n%s", want, joined)
		}
	}
	for _, bad := range []string{"/queue", "/status", "/health", "/history", "/events/poll"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("高频轮询 %s 不该逐条进日志：\n%s", bad, joined)
		}
	}
}

// TestAccessLogSSEOpenAndClose（同日实测缺陷二）：只在 handler 返回后记日志的话，
// 一条活着的 SSE 连接在日志里完全不存在——实测 QQ 桥明明连着，/events 却是 0 条。
// 现在建立与结束各一行。
func TestAccessLogSSEOpenAndClose(t *testing.T) {
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

	res, err := srv.Client().Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	waitFor := func(want string) string {
		t.Helper()
		for i := 0; i < 40; i++ {
			mu.Lock()
			j := strings.Join(lines, "\n")
			mu.Unlock()
			if strings.Contains(j, want) {
				return j
			}
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("等不到 %q，日志：\n%s", want, strings.Join(lines, "\n"))
		return ""
	}
	waitFor("[http] GET /events → 连接建立")
	res.Body.Close() // 客户端断开 → handler 返回 → 记"连接结束"
	waitFor("[http] GET /events → 连接结束")
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
