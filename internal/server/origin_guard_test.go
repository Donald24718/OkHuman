package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

// 写端点的来源守卫：API 无鉴权，浏览器同源策略是唯一屏障，而跨站"简单请求"
// （POST + text/plain）不触发 preflight，能直接打到 127.0.0.1 让 agent 执行指令。
func TestWriteEndpointsRejectForeignOrigin(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM([]llm.FakeStep{}), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	ap := New(st, nil)

	post := func(host, origin, ctype string) int {
		req := httptest.NewRequest("POST", "/reset", strings.NewReader("{}"))
		req.Host = host
		req.Header.Set("Content-Type", ctype)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		ap.ServeHTTP(rec, req)
		return rec.Code
	}
	// 本机调用方（curl / 脚本 / QQ 桥 / 插件）不带 Origin：必须照常放行
	if c := post("127.0.0.1:8451", "", "application/json"); c != 200 {
		t.Errorf("本机 Host 无 Origin 应放行，得 %d", c)
	}
	if c := post("localhost:8451", "", "text/plain"); c != 200 {
		t.Errorf("localhost 无 Origin 应放行，得 %d", c)
	}
	// WebUI 同源：放行
	if c := post("127.0.0.1:8451", "http://127.0.0.1:8451", "application/json"); c != 200 {
		t.Errorf("本机同源应放行，得 %d", c)
	}
	// 跨站页面：拒绝（这正是恶意网页能做的事）
	if c := post("127.0.0.1:8451", "http://evil.example", "text/plain"); c != 403 {
		t.Errorf("跨站 Origin 应 403，得 %d", c)
	}
	// DNS rebinding：Host 变成外部域名指到本机
	if c := post("evil.example:8451", "", "application/json"); c != 403 {
		t.Errorf("Host 非本机应 403，得 %d", c)
	}
	// Origin 里塞本机字符串但实际不是本机同源
	if c := post("127.0.0.1:8451", "http://127.0.0.1.evil.example", "text/plain"); c != 403 {
		t.Errorf("伪冒本机前缀的 Origin 应 403，得 %d", c)
	}
}
