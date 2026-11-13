package server

import (
	"fmt"
	"net/http"
	"sync"
)

func newSSE(w http.ResponseWriter) *sseConn {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl, ok := w.(http.Flusher)
	// 防御性：当前代码里不可达（statusRecorder 显式转发 Flush）。留着是因为 c.write 在
	// goroutine 里，若将来有中间件包 ResponseWriter 忘了转发 Flush，nil 接口调用会 panic
	// 带走整个实例。退化成 noFlusher + 记一行（症状：连上收不到东西）。
	if !ok {
		fl = noFlusher{}
	}
	return &sseConn{w: w, flusher: fl, flushable: ok, done: make(chan struct{})}
}

// ---------- /enqueue ----------

// sseConn SSE 连接写端（并发写加锁：事件来自 drain goroutine，心跳来自另一 goroutine）
type sseConn struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	flushable bool // false = 响应不支持 Flush（见 newSSE），事件只能等连接关闭时刷出
	mu        sync.Mutex
	closed    bool
	done      chan struct{}
}

// 5s 保活 ping 现在只在 handleEvents 里发（本连接自己的 ticker）——
// 旧的 sseConn.heartbeat 随 /chat/stream 一起删了（它只服务那个端点）。

func (c *sseConn) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	close(c.done)
}

// /chat/stream 已删除（2026-10-18 健壮性复查，主人定）：它是"同步流式"端点——
// run 每产生一个事件就在 **run 自己的 goroutine 里**往该连接的 socket 写一帧，
// 而 sseConn.write 无 deadline。对端不读 → 内核缓冲填满 → write 挂住 → run 回不到
// "读 LLM 下一批字节"那一步：轻则空闲计时器误判"模型没响应"把本轮判死，重则本轮
// 永不结束、运行锁一直被占，队列里所有消息一起等，实例对外不回话只能重启。
// 这正是修 B 消灭的模式（修 B 只覆盖了 /events，这个端点漏了）。
// 插件契约改为：流式一律 POST /enqueue（异步入队）+ GET /events（SSE 订阅）或
// GET /events/poll（短轮询）——那两个通道是"内核只写内存、消费者按自己节奏取"，
// 慢客户端只卡自己，落后超窗还会明确报 gap 让调用方走 /history 补。
// 本机三方（QQ 桥 / 语音 / WebUI）本来就走 /enqueue + /events，删除不影响它们。

// handleEnqueue 统一入队（2026-08-31）：走队列/drain 机制。
// queuedBefore/position 在 enqueueAndWake 的同一把锁内取出（2026-09-14 起
// 入队与取位原子化，不再有"先取位后入队"的时序要求）。

// noFlusher 是不支持 Flush 时的退路（nil 接口调用会在 goroutine 里 panic）。
type noFlusher struct{}

func (c *sseConn) push(event string, data interface{}) {
	c.write(fmt.Sprintf("event: %s\ndata: %s\n\n", event, jsonStr(data)))
}

func (c *sseConn) write(payload string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	_, _ = c.w.Write([]byte(payload))
	c.flusher.Flush()
}
