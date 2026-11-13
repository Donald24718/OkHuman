package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 响应不支持 Flush 时不能 nil panic（newSSE 的退路）。注意：当前代码里这条**不可达**——
// statusRecorder 已显式转发 Flush，所以这是防御性保护，不是修活故障。写测试是为了：
// 以后谁再加一个包 ResponseWriter 的中间件忘了转发 Flush，这里会先红，而不是在生产里
// 于 goroutine 中 panic 带走整个实例。
type nonFlushResponseWriter struct {
	http.ResponseWriter
}

func TestSSEWithoutFlusherDegradesInsteadOfPanicking(t *testing.T) {
	rec := httptest.NewRecorder()
	c := newSSE(nonFlushResponseWriter{rec})
	if c.flushable {
		t.Fatal("包装过的 ResponseWriter 不该被认作 Flusher")
	}
	c.write("data: x\n\n") // 旧写法：nil 接口调用 → panic
	if rec.Body.String() != "data: x\n\n" {
		t.Errorf("正文仍应写出，得 %q", rec.Body.String())
	}
}
