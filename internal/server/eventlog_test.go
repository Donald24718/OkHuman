package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"okhuman/internal/config"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

func getJSON(ap http.Handler, path string, out interface{}) int {
	rec := httptest.NewRecorder()
	ap.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code == 200 && out != nil {
		_ = json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec.Code
}

func frame(kind string, n int) string {
	return fmt.Sprintf("event: %s\ndata: {\"i\":%d}\n\n", kind, n)
}

// 发车节奏：结构性帧一到即发车；纯增量帧攒批（不养后台定时器 goroutine）
func TestEventLogFlushPolicy(t *testing.T) {
	el := newEventLog()
	el.add(frame("delta", 1), "delta")
	el.add(frame("delta", 2), "delta")
	bs, latest, _, _ := el.takeSince(0)
	if latest != 1 || len(bs) != 1 || len(bs[0].Frames) != 2 {
		t.Fatalf("两条 delta 应在同一批：latest=%d bs=%v", latest, bs)
	}
	el.add(frame("tool_result", 3), "tool_result")
	bs, latest, _, _ = el.takeSince(1)
	if latest != 2 || len(bs) != 1 || len(bs[0].Frames) != 1 {
		t.Fatalf("结构性帧应立即发车：latest=%d bs=%v", latest, bs)
	}
}

// 窗口 + 缺口 + 消费者隔离（修 B 取代旧"订阅者隔离"测试）：
// 一个消费者落后超窗，不得影响另一个消费者取到的内容。
func TestEventLogWindowGapAndIsolation(t *testing.T) {
	el := newEventLog()
	for i := 1; i <= 30; i++ {
		el.add(frame("run_start", i), "run_start") // 结构性 → 每帧一批
	}
	_, latest, _, _ := el.takeSince(0)
	if latest != 30 {
		t.Fatalf("应发车 30 批，得 %d", latest)
	}
	// 停在 0 的慢消费者：只能拿到窗口内批次，且必须被告知缺口
	slow, _, gap, _ := el.takeSince(0)
	if gap == 0 {
		t.Fatal("落后超窗必须报缺口（前端据此走 /history 补）")
	}
	if len(slow) != maxEventBatches {
		t.Fatalf("窗口应给 %d 批，得 %d", maxEventBatches, len(slow))
	}
	// 跟得上的快消费者：不受慢消费者影响，无缺口
	fast, _, gap2, _ := el.takeSince(29)
	if gap2 != 0 || len(fast) != 1 || fast[0].Seq != 30 {
		t.Fatalf("快消费者被影响：gap=%d bs=%v", gap2, fast)
	}
	// 字节上限同样能裁窗（一批含超大帧时按批数会低估）
	el2 := newEventLog()
	for i := 1; i <= 4; i++ {
		el2.add(strings.Repeat("x", 900<<10), "run_start")
	}
	if _, _, gap3, _ := el2.takeSince(0); gap3 == 0 {
		t.Fatal("超字节上限应裁窗并报缺口")
	}
}

// 新客户端（since=-1）：有轮在跑 → 从本轮开头给（界面不空白）；无轮 → 不回放历史
func TestEventLogNewClientReplay(t *testing.T) {
	el := newEventLog()
	el.add(frame("run_start", 1), "run_start")
	el.add(frame("delta", 2), "delta")
	bs, _, _, fromStart := el.takeSince(-1)
	if !fromStart || len(bs) == 0 || !strings.Contains(bs[0].Frames[0], "run_start") {
		t.Fatalf("轮在跑时新客户端应从本轮开头拿：%v", bs)
	}
	el.add(frame("run_end", 3), "run_end")
	el2 := newEventLog()
	el2.add(frame("run_start", 1), "run_start")
	el2.add(frame("run_end", 2), "run_end")
	if bs2, _, _, _ := el2.takeSince(-1); len(bs2) != 0 {
		t.Fatalf("无轮在跑时不得回放历史轮：%v", bs2)
	}
}

// /events/poll 端点（WebUI 短轮询）
func TestEventsPollEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	ap := New(st, nil)
	broadcastEvent(st, types.AgentEvent{"type": "run_start"})
	broadcastEvent(st, types.AgentEvent{"type": "delta", "text": "hi"})

	var out struct {
		Batches []eventBatch `json:"batches"`
		Latest  int          `json:"latest"`
		Gap     int          `json:"gap"`
		Running bool         `json:"running"`
	}
	code := getJSON(ap, "/events/poll?since=-1", &out)
	if code != 200 {
		t.Fatalf("poll code=%d", code)
	}
	if out.Latest < 1 || len(out.Batches) == 0 {
		t.Fatalf("新客户端应拿到本轮批次：latest=%d batches=%v", out.Latest, out.Batches)
	}
	var again struct {
		Batches []eventBatch `json:"batches"`
		Latest  int          `json:"latest"`
	}
	if code := getJSON(ap, fmt.Sprintf("/events/poll?since=%d", out.Latest), &again); code != 200 || len(again.Batches) != 0 {
		t.Fatalf("游标已跟上时应给空批次：code=%d batches=%v", code, again.Batches)
	}
}

// 修 B 的核心回归：/events 客户端**完全不读**（挂起的标签页 / 断网 / 代理缓冲），
// run 也必须正常跑完。旧实现里 run 直接写该连接的 socket → 卡在写上面 →
// 读不到 LLM 字节 → 空闲计时器误判超时；压缩连累三次重试后触发硬截断丢记忆。
func TestSlowSSEClientDoesNotStallRun(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	big := strings.Repeat("字", 200000) // 400KB 正文 → 远超 socket 缓冲（客户端不读必然写满）
	a := CreateAgentState(cfg, llm.NewFakeLLM([]llm.FakeStep{{Type: "text", Text: big}}),
		"sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	ap := New(st, nil)
	srv := httptest.NewServer(ap)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /events HTTP/1.1\r\nHost: " + host + "\r\nAccept: text/event-stream\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// 故意不读响应体：服务端写满 socket 缓冲后就会阻塞

	done := make(chan string, 1)
	go func() {
		var out map[string]interface{}
		postJSON(ap, "/chat", map[string]interface{}{"message": "触发一轮"}, &out)
		b, _ := json.Marshal(out)
		done <- string(b)
	}()
	select {
	case body := <-done:
		if !strings.Contains(body, "reply") {
			t.Fatalf("run 未正常完成：%s", body[:min(300, len(body))])
		}
	case <-time.After(90 * time.Second): // race 检测器下慢好几倍，别把慢当成卡
		t.Fatal("run 被不读的 /events 客户端卡住（修 B 未生效）")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 编译期确保 http/httptest 引用被用到（部分测试文件共享包级 import）
var _ = http.MethodGet
var _ = httptest.NewServer
