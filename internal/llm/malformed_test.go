package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"okhuman/internal/types"
)

// sseUpstream 按给定行序吐 SSE 的假上游（每行后补空行，模拟真实分帧）
func sseUpstream(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, l := range lines {
			fmt.Fprint(w, l+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}))
}

// TestMalformedFramesCounted（2026-10-18 复查修 2）：旧实现把解析不出来的帧直接跳过，
// 不计数不日志不报错——回复少了一截永远查不出原因。现在：少量畸形帧照常返回，
// 但计数带出 + 明细进日志。
func TestMalformedFramesCounted(t *testing.T) {
	lines := []string{
		`data: {"choices":[{"delta":{"content":"前半"}}]}`,
		`data: {这不是 json`,
		`data: {"choices":[{"delta":`, // 被截断的半行
		`data: ][明显坏帧[`,
		`data: {"choices":[{"delta":{"content":"后半"}}]}`,
		`data: [DONE]`,
	}
	srv := sseUpstream(t, lines)
	defer srv.Close()
	c := NewOpenAiClient(ClientConfig{BaseURL: srv.URL, Model: "m", TimeoutMS: 5000, StreamIdleMS: 3000})
	var logged []string
	c.SetLogger(func(m string) { logged = append(logged, m) })

	resp, err := c.CompleteStream(context.Background(), []types.Message{{Role: "user", Content: "x"}}, nil, nil, 3000)
	if err != nil {
		t.Fatalf("少量畸形帧不该终止本轮：%v", err)
	}
	if resp.BadFrames != 3 {
		t.Fatalf("BadFrames 应为 3，得 %d", resp.BadFrames)
	}
	if resp.Content != "前半后半" {
		t.Fatalf("正常帧内容不该被牵连：%q", resp.Content)
	}
	if len(logged) != 3 {
		t.Fatalf("畸形帧明细日志应 3 条，得 %d：%v", len(logged), logged)
	}
}

// TestMalformedFramesFatal：畸形帧多到阈值 = 上游在持续吐读不懂的东西，
// 继续跑只会得到一条"少了不知道多少"的回复 → 必须明确报错终止本轮。
func TestMalformedFramesFatal(t *testing.T) {
	lines := make([]string, 0, maxMalformedFrames+4)
	lines = append(lines, `data: {"choices":[{"delta":{"content":"开头"}}]}`)
	for i := 0; i < maxMalformedFrames+5; i++ {
		lines = append(lines, fmt.Sprintf(`data: {坏帧%d 不是json`, i))
	}
	srv := sseUpstream(t, lines)
	defer srv.Close()
	c := NewOpenAiClient(ClientConfig{BaseURL: srv.URL, Model: "m", TimeoutMS: 5000, StreamIdleMS: 3000})
	logCount := 0
	c.SetLogger(func(m string) { logCount++ })

	_, err := c.CompleteStream(context.Background(), []types.Message{{Role: "user", Content: "x"}}, nil, nil, 3000)
	if err == nil {
		t.Fatal("畸形帧超阈值必须报错，却正常返回了")
	}
	var mse *MalformedStreamError
	if !errors.As(err, &mse) {
		t.Fatalf("应为 MalformedStreamError，得 %T: %v", err, err)
	}
	if mse.Count < maxMalformedFrames {
		t.Fatalf("Count=%d 应达阈值 %d", mse.Count, maxMalformedFrames)
	}
	if logCount > malformedLogLimit+1 {
		t.Fatalf("明细日志应限流（≤%d+收尾一条），得 %d", malformedLogLimit, logCount)
	}
}
