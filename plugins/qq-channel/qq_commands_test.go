package main

// QQ 侧控制指令（/stop /restart）的测试：识别、门控、端点调用、文案、以及
// "指令绝不进 agent"。实例端用 httptest 假服务器，QQ 出口用 sendReplyFn 注入。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func sp(s string) *string { x := s; return &x }

func TestAgentCommandOf(t *testing.T) {
	yes := map[string]string{
		"/stop": "stop", " /STOP ": "stop", "/停止": "stop", "/停": "stop", "/中断": "stop",
		"/restart": "restart", "/reset": "restart", " /重开": "restart", "/重置": "restart",
	}
	for in, want := range yes {
		got, ok := agentCommandOf(in)
		if !ok || got != want {
			t.Errorf("%q → (%q,%v)，想要 (%q,true)", in, got, ok, want)
		}
	}
	no := []string{"/stop 一下", "帮我 /stop", "/stopx", "/", "/stop/", "重启一下", "", "  "}
	for _, in := range no {
		if got, ok := agentCommandOf(in); ok {
			t.Errorf("%q 不该算指令，却得到 %q", in, got)
		}
	}
}

// fakeInstance 记录被调到的路径，并按 handler 返回
type fakeInstance struct {
	mu    sync.Mutex
	calls []string
	h     map[string]func(w http.ResponseWriter)
}

func (f *fakeInstance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	h := f.h[r.URL.Path]
	f.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w)
}

func (f *fakeInstance) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func jsonHandler(v interface{}) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func TestMakeOnCommandStop(t *testing.T) {
	f := &fakeInstance{h: map[string]func(http.ResponseWriter){
		"/stop":  jsonHandler(map[string]bool{"ok": true}),
		"/queue": jsonHandler(map[string]interface{}{"running": false, "queued": []string{"a", "b"}, "user_seq": 3}),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	reply, err := makeOnCommand(srv.URL, "inst-1", func(string) {})("stop")
	if err != nil {
		t.Fatalf("stop 出错：%v", err)
	}
	if !f.called("POST /stop") {
		t.Errorf("没调 /stop，calls=%v", f.calls)
	}
	for _, want := range []string{"inst-1", "running=false", "待发消息 2 条", "/restart"} {
		if !strings.Contains(reply, want) {
			t.Errorf("文案缺 %q：%s", want, reply)
		}
	}
}

func TestMakeOnCommandRestartReportsCountButNotLocalPath(t *testing.T) {
	f := &fakeInstance{h: map[string]func(http.ResponseWriter){
		"/reset": jsonHandler(map[string]interface{}{
			"ok": true, "session": 10,
			"record": "/var/lib/okhuman/session-records/ctx-9-demo.txt", "record_msgs": 974,
		}),
		"/queue": jsonHandler(map[string]interface{}{"running": false, "queued": []string{}}),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	reply, err := makeOnCommand(srv.URL, "inst-1", func(string) {})("restart")
	if err != nil {
		t.Fatalf("restart 出错：%v", err)
	}
	if !f.called("POST /reset") {
		t.Errorf("没调 /reset，calls=%v", f.calls)
	}
	for _, want := range []string{"新会话 #10", "974 条"} {
		if !strings.Contains(reply, want) {
			t.Errorf("文案缺 %q：%s", want, reply)
		}
	}
	// 本机绝对路径不得外泄到 QQ 渠道
	for _, bad := range []string{"/mnt/", "/media/", "/home/", "session-records"} {
		if strings.Contains(reply, bad) {
			t.Errorf("文案泄漏本机路径片段 %q：%s", bad, reply)
		}
	}
}

func TestMakeOnCommandRestartOldInstanceNoRecordField(t *testing.T) {
	f := &fakeInstance{h: map[string]func(http.ResponseWriter){
		"/reset": jsonHandler(map[string]interface{}{"ok": true, "session": 3}),
		"/queue": jsonHandler(map[string]interface{}{"running": false, "queued": []string{}}),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	reply, err := makeOnCommand(srv.URL, "old", func(string) {})("restart")
	if err != nil {
		t.Fatalf("restart 出错：%v", err)
	}
	if !strings.Contains(reply, "新会话 #3") || !strings.Contains(reply, "不回报全文存档路径") {
		t.Errorf("旧实例（无 record 字段）文案不对：%s", reply)
	}
}

func TestMakeOnCommandEmptySessionSaysNoArchive(t *testing.T) {
	f := &fakeInstance{h: map[string]func(http.ResponseWriter){
		"/reset": jsonHandler(map[string]interface{}{"ok": true, "session": 2, "record": "", "record_msgs": 0}),
		"/queue": jsonHandler(map[string]interface{}{"running": false, "queued": []string{}}),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	reply, _ := makeOnCommand(srv.URL, "x", func(string) {})("restart")
	if !strings.Contains(reply, "旧会话为空") {
		t.Errorf("空会话应说明未落盘：%s", reply)
	}
}

func TestMakeOnCommandPropagatesInstanceError(t *testing.T) {
	f := &fakeInstance{h: map[string]func(http.ResponseWriter){
		"/stop": func(w http.ResponseWriter) {
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom-detail"})
		},
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, err := makeOnCommand(srv.URL, "x", func(string) {})("stop")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom-detail") {
		t.Fatalf("应把实例错误原样带出，得到：%v", err)
	}
}

func TestMakeOnCommandUnknownCmd(t *testing.T) {
	if _, err := makeOnCommand("http://127.0.0.1:1", "x", func(string) {})("nope"); err == nil {
		t.Fatal("未知指令应报错")
	}
}

func newTestChannel(shared SharedCfg, bot BotCfg, onCmd func(string) (string, error)) (*QQChannel, *[]string, *[]string) {
	var sent, fwd []string
	c := NewQQChannel(shared, bot, t_DataDir(), nil, nil, nil, onCmd, func(string) {})
	c.sendReplyFn = func(target, text string) error {
		sent = append(sent, target+"|"+text)
		return nil
	}
	c.onStream = func(text string) error { fwd = append(fwd, text); return nil }
	return c, &sent, &fwd
}

func t_DataDir() string { return "/tmp/qq-bridge-test" }

func TestCommandAllowed(t *testing.T) {
	sh := defaultSharedCfg()
	c, _, _ := newTestChannel(sh, BotCfg{ChannelID: "chan:1"}, nil)
	if !c.commandAllowed("openid-A") {
		t.Error("默认应开放")
	}
	sh.CommandsEnabled = false
	c, _, _ = newTestChannel(sh, BotCfg{ChannelID: "chan:1"}, nil)
	if c.commandAllowed("openid-A") {
		t.Error("commands_enabled=false 应拒绝")
	}
	sh.CommandsEnabled = true
	sh.CommandOpenIDs = []string{"OpenID-A"}
	c, _, _ = newTestChannel(sh, BotCfg{ChannelID: "chan:1"}, nil)
	if !c.commandAllowed("openid-a") {
		t.Error("白名单应大小写不敏感")
	}
	if c.commandAllowed("openid-B") {
		t.Error("白名单外应拒绝")
	}
	if c.commandAllowed("") {
		t.Error("空 sender 应拒绝")
	}
}

func TestChannelCommandIsNotForwardedToAgent(t *testing.T) {
	called := false
	c, sent, fwd := newTestChannel(defaultSharedCfg(), BotCfg{ChannelID: "chan:1"},
		func(cmd string) (string, error) { called = true; return "已中断", nil })
	c.handleMessage(&qqMsg{ChannelID: sp("chan:1"), Content: sp("/stop"),
		Author: &qqAuthor{ID: sp("u1")}})
	if !called {
		t.Fatal("/stop 没走指令处理器")
	}
	if len(*fwd) != 0 {
		t.Errorf("指令不该转发给 agent，转发了 %v", *fwd)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "已中断") {
		t.Errorf("应把结果回给发送者，得到 %v", *sent)
	}
}

func TestC2CCommandIsNotForwardedAndGated(t *testing.T) {
	// 开放：走指令，不转发
	c, sent, fwd := newTestChannel(defaultSharedCfg(), BotCfg{ChannelID: "c2c:openA"},
		func(cmd string) (string, error) { return "已重置", nil })
	c.handleC2CMessage(&qqMsg{Content: sp("/restart"), UserOpenID: sp("openA")})
	if len(*fwd) != 0 || len(*sent) != 1 || !strings.Contains((*sent)[0], "c2c:openA|已重置") {
		t.Errorf("私聊指令路径不对：fwd=%v sent=%v", *fwd, *sent)
	}
	// 白名单外：拒绝且不调处理器
	called := false
	sh := defaultSharedCfg()
	sh.CommandOpenIDs = []string{"someone-else"}
	c2, sent2, fwd2 := newTestChannel(sh, BotCfg{ChannelID: "c2c:openA"},
		func(cmd string) (string, error) { called = true; return "", nil })
	c2.handleC2CMessage(&qqMsg{Content: sp("/stop"), UserOpenID: sp("openA")})
	if called {
		t.Error("白名单外不应执行处理器")
	}
	if len(*fwd2) != 0 {
		t.Errorf("被拒的指令也不许转给 agent：%v", *fwd2)
	}
	if len(*sent2) != 1 || !strings.Contains((*sent2)[0], "未开放") {
		t.Errorf("被拒应有说明，得到 %v", *sent2)
	}
}

func TestNormalMessageStillForwarded(t *testing.T) {
	c, sent, fwd := newTestChannel(defaultSharedCfg(), BotCfg{ChannelID: "chan:1"},
		func(cmd string) (string, error) { t.Fatal("普通消息不该走指令"); return "", nil })
	c.handleMessage(&qqMsg{ChannelID: sp("chan:1"), Content: sp("帮我看看 /stop 这个功能"),
		Author: &qqAuthor{ID: sp("u1")}})
	if len(*fwd) != 1 || (*fwd)[0] != "帮我看看 /stop 这个功能" {
		t.Errorf("普通消息应原样转发，得到 %v", *fwd)
	}
	if len(*sent) != 0 {
		t.Errorf("普通消息不该由指令路径回复：%v", *sent)
	}
}
