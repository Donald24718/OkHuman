package context

import (
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"okhuman/internal/types"
)

// TestPortToPID 真实监听端口 → 应找到自己（/proc 扫描链路 e2e）
func TestPortToPID(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if got := portToPID(port); got != os.Getpid() {
		t.Fatalf("portToPID(%d)=%d，want %d", port, got, os.Getpid())
	}
	if got := portToPID(1); got != -1 {
		t.Fatalf("portToPID(1)=%d，want -1", got)
	}
}

// TestParseHostPort base_url 解析
func TestParseHostPort(t *testing.T) {
	cases := []struct{ in, wantHost string; wantPort int }{
		{"http://127.0.0.1:8080/v1", "127.0.0.1:8080", 8080},
		{"http://localhost:9000", "localhost:9000", 9000},
		{"", "", 0},
	}
	for _, c := range cases {
		h, p := parseHostPort(c.in)
		if h != c.wantHost || p != c.wantPort {
			t.Fatalf("parseHostPort(%q)=(%q,%d)，want (%q,%d)", c.in, h, p, c.wantHost, c.wantPort)
		}
	}
}

// TestStampMessages 字符串/空串/parts 三类内容 + 存储不被污染
func TestStampMessages(t *testing.T) {
	orig := []types.ContentPart{
		types.TextPart("正文"),
		{Type: "input_image", Raw: json.RawMessage(`{"type":"input_image","image_ref":"x"}`)},
	}
	msgs := []types.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: ""},
		{Role: "user", Content: "hello"},
		{Role: "user", Content: orig},
	}
	stampMessages(msgs, " [T]")
	if msgs[0].Content != "sys [T]" {
		t.Fatalf("system: %v", msgs[0].Content)
	}
	if msgs[1].Content != "" {
		t.Fatalf("空串不应追加: %v", msgs[1].Content)
	}
	if msgs[2].Content != "hello [T]" {
		t.Fatalf("user: %v", msgs[2].Content)
	}
	parts := types.AsParts(msgs[3].Content)
	if parts == nil || parts[0].Text != "正文 [T]" {
		t.Fatalf("parts text: %+v", parts)
	}
	// 存储未被污染
	if orig[0].Text != "正文" {
		t.Fatalf("存储被污染: %q", orig[0].Text)
	}
	if string(orig[1].Raw) != `{"type":"input_image","image_ref":"x"}` {
		t.Fatalf("非 text part Raw 被改: %s", orig[1].Raw)
	}
	// 序列化输出含时间戳
	raw, _ := json.Marshal(parts[0])
	if !strings.Contains(string(raw), "正文 [T]") {
		t.Fatalf("序列化未含时间戳: %s", raw)
	}
}

// TestComposeLocked 动态注入：系统首自我块 + 每条消息尾时间戳
func TestComposeLocked(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	m := NewManager(nil, Cfg{CharsPerToken: 1.5}, "原始系统提示词", t.TempDir())
	m.SetSelfInfo(SelfInfo{Port: 8461, DataDir: "/tmp/x", LLMBaseURL: "http://127.0.0.1:" + strconv.Itoa(port) + "/v1"})
	m.AddMessage(&types.RawEntry{Role: "user", Content: "你好"})
	m.BeginRun()
	msgs := m.Compose(nil)
	sys, _ := msgs[0].Content.(string)
	if !strings.HasPrefix(sys, "[你的生命]") {
		t.Fatalf("system 未以自我块开头: %s", sys[:min(40, len(sys))])
	}
	for _, want := range []string{"服务端口：8461", "进程 PID：", "数据目录：/tmp/x", "127.0.0.1:" + strconv.Itoa(port), "原始系统提示词"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("自我块缺 %q:\n%s", want, sys)
		}
	}
	if !strings.HasSuffix(sys, m.runStamp+"]") {
		t.Fatalf("system 尾无时间戳: ...%s", sys[min(40, len(sys)):])
	}
	u, ok := msgs[1].Content.(string)
	if !ok || !strings.HasSuffix(u, m.runStamp+"]") {
		t.Fatalf("user 消息尾无时间戳: %v", msgs[1].Content)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
