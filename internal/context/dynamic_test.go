package context

import (
	"net"
	"os"
	"path/filepath"
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


// TestComposeLocked 动态注入：系统首自我块 + 每条消息尾时间戳
func TestComposeLocked(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	m := NewManager(nil, Cfg{CharsPerToken: 1.5}, "原始系统提示词", t.TempDir())
	m.SetSelfInfo(SelfInfo{Port: 8461, DataDir: "/tmp/x", LLMBaseURL: "http://127.0.0.1:" + strconv.Itoa(port) + "/v1"})
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(strings.TrimSuffix(exe, " (deleted)"))
	}
	m.AddMessage(&types.RawEntry{Role: "user", Content: "你好"})
	msgs := m.Compose(nil)
	sys, _ := msgs[0].Content.(string)
	if !strings.HasPrefix(sys, "[你的生命]") {
		t.Fatalf("system 未以自我块开头: %s", sys[:min(40, len(sys))])
	}
	wants := []string{"服务端口：8461", "进程 PID：", "源码位置：", "数据目录：/tmp/x", "127.0.0.1:" + strconv.Itoa(port), "原始系统提示词"}
	if exeDir != "" {
		wants = append(wants, exeDir)
	}
	for _, want := range wants {
		if !strings.Contains(sys, want) {
			t.Fatalf("自我块缺 %q:\n%s", want, sys)
		}
	}
	if !strings.HasSuffix(sys, "原始系统提示词") {
		t.Fatalf("system 尾应原样结束（无时间戳追加）: ...%s", sys[min(60, len(sys)):])
	}
	if u, _ := msgs[1].Content.(string); u != "你好" {
		t.Fatalf("user 消息应原样（无时间戳）: %q", u)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
