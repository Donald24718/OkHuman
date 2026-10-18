package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"okhuman/internal/types"
)

// 修 A（2026-10-18）：LLM 往回吐的内容必须有总量闸。此前工具输出封顶 32MB、
// 入站请求体封顶 256MB，唯独这一路无界——上游出 bug 疯狂吐内容就把本进程内存
// 吃光（数据目录还在 tmpfs）。空闲计时器不管用：它每有字节就重置。

func streamSrv(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, body)
	}))
}

func contentFrames(n int, size int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", strings.Repeat("x", size)))
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestStreamResponseTooLarge(t *testing.T) {
	srv := streamSrv(t, contentFrames(300, 200)) // ≈70KB
	defer srv.Close()
	c := NewOpenAiClient(ClientConfig{BaseURL: srv.URL, Model: "m", MaxResponseBytes: 4096})
	_, err := c.CompleteStream(context.Background(), []types.Message{{Role: "user", Content: "hi"}}, nil, func(types.Delta) {}, 5000)
	var tl *ResponseTooLargeError
	if !errors.As(err, &tl) {
		t.Fatalf("应报响应体量超限，实得：%v", err)
	}
	if tl.What != "body" || tl.LimitBytes != 4096 {
		t.Fatalf("错误内容不对：%+v", tl)
	}
}

// 未超限的正常流不受影响
func TestStreamUnderCapOK(t *testing.T) {
	srv := streamSrv(t, contentFrames(5, 20))
	defer srv.Close()
	c := NewOpenAiClient(ClientConfig{BaseURL: srv.URL, Model: "m", MaxResponseBytes: 4096})
	resp, err := c.CompleteStream(context.Background(), []types.Message{{Role: "user", Content: "hi"}}, nil, func(types.Delta) {}, 5000)
	if err != nil {
		t.Fatalf("正常响应被误伤：%v", err)
	}
	if len(resp.Content) == 0 {
		t.Fatal("正文为空")
	}
}

// 非流式路径同样有闸（此前直接 Decode 整个 body）
func TestCompleteResponseTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"%s"}}]}`, strings.Repeat("y", 20000))
	}))
	defer srv.Close()
	c := NewOpenAiClient(ClientConfig{BaseURL: srv.URL, Model: "m", TimeoutMS: 10000, MaxResponseBytes: 2048})
	_, err := c.Complete(context.Background(), []types.Message{{Role: "user", Content: "hi"}}, nil)
	var tl *ResponseTooLargeError
	if !errors.As(err, &tl) {
		t.Fatalf("非流式应报体量超限，实得：%v", err)
	}
}

// tool_calls 槽位闸：总量闸管字节，海量小帧仍能把按 index 建的槽位表撑大
func TestStreamToolCallSlotsCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= maxToolCallSlots; i++ {
		b.WriteString(fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":\"c%d\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n", i, i))
	}
	b.WriteString("data: [DONE]\n\n")
	srv := streamSrv(t, b.String())
	defer srv.Close()
	c := NewOpenAiClient(ClientConfig{BaseURL: srv.URL, Model: "m", MaxResponseBytes: 64 << 20})
	_, err := c.CompleteStream(context.Background(), []types.Message{{Role: "user", Content: "hi"}}, nil, func(types.Delta) {}, 10000)
	var tl *ResponseTooLargeError
	if !errors.As(err, &tl) {
		t.Fatalf("应报 tool_calls 槽位超限，实得：%v", err)
	}
	if tl.What != "tool_call_slots" {
		t.Fatalf("错误类型不对：%+v", tl)
	}
}

// 默认上限（未配置）= 256MB
func TestDefaultMaxResponseBytes(t *testing.T) {
	c := NewOpenAiClient(ClientConfig{BaseURL: "http://127.0.0.1:1/v1"})
	if got := c.maxRespBytes(); got != DefaultMaxResponseBytes {
		t.Fatalf("默认上限应为 256MB，得 %d", got)
	}
}
