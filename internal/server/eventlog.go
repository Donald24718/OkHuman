package server

import (
	"strings"
	"sync"
)

// 事件"发车"缓冲（2026-10-18 健壮性修 B）。
//
// 旧模型：run 的 emit 直接逐个写 SSE 订阅者的 socket。慢客户端（标签页被挂起、
// 网络断、代理缓冲）会让 **run 自己的 goroutine** 卡在 socket 写上——期间读不到
// LLM 的新字节 → 空闲计时器误判"模型没响应" → 本轮被判超时失败；压缩更糟：
// 空闲超时重试 3 次全被同一个卡住的客户端掐住，第 3 次触发硬截断兜底，最老一批
// 上下文没有 summary 就被丢掉。即"一个挂住的浏览器标签页能让我丢记忆"。
//
// 新模型（定案：发车机制 + 前端自己取，不踢人）：
//   - run 只把帧放进内存批次缓冲，**永不碰网络**；
//   - 帧按节奏"发车"成批，每批带全局递增号；缓冲保留最近 maxEventBatches 批且
//     总量不超 maxEventBytes，超窗丢最旧并累计缺口计数；
//   - 两类消费者：GET /events（SSE，QQ 桥 / voice-chat 等外部消费者，**协议字节级
//     不变**，由连接自己的 goroutine 从缓冲取着写——慢客户端只卡自己）；
//     GET /events/poll（WebUI 短轮询，带游标取增量，落后超窗就明确告知缺口）；
//   - 不踢任何客户端：落后超窗的缺口由 /history 兜底（QQ 桥与 WebUI 本来都有兜底轮询）。
//
// 发车节奏：结构性帧（run_start/run_end/tool_*/compress/error…）一到即发车——
// 界面收尾与结构变化不该等一个节奏；delta / compress_delta 攒着，由消费者来取时
// 或攒够 eventBatchBytes 时发车（不养后台定时器 goroutine）。
const (
	maxEventBatches = 10       // 滑动窗口：最近 10 批
	maxEventBytes   = 3 << 20  // 窗口字节上限（一批可能含整条工具结果，光按批数会低估）
	eventBatchBytes = 64 << 10 // 攒够这么多字节提前发车
)

// eventBatch 一次发车打包的 SSE 帧（Frames 里就是 /events 上的原始帧文本）
type eventBatch struct {
	Seq    int      `json:"seq"`
	Frames []string `json:"frames"`
	Bytes  int      `json:"bytes"`
}

// eventLog 事件批次缓冲
type eventLog struct {
	mu       sync.Mutex
	batches  []eventBatch // 窗口内批次（按 Seq 升序）
	bytes    int          // 窗口内总字节
	dropped  int          // 被窗口挤掉的批次数（缺口）
	nextSeq  int          // 下一批发车号
	cur      []string     // 未发车帧
	curBytes int
	roundSeq int // 当前轮 run_start 所在批次号（0 = 当前无轮）
}

func newEventLog() *eventLog { return &eventLog{nextSeq: 1} }

// coalescible 只有纯增量帧攒批（默认其余一律立即发车——新增事件类型不会漏进
// "攒着不发"的坑）
func coalescible(eventType string) bool {
	return eventType == "delta" || eventType == "compress_delta"
}

// add 收一帧。structural=false 的增量帧攒着，其余立即发车。
func (el *eventLog) add(payload string, eventType string) {
	el.mu.Lock()
	defer el.mu.Unlock()
	if eventType == "run_end" {
		// 轮结束：新客户端不再回放该轮（与旧 RoundBuf 在 run_end 清空的语义一致，
		// 已完成轮的历史由 /history 给，回放会重复出气泡）
		el.roundSeq = 0
	}
	if eventType == "run_start" {
		// 记下本轮起始批次：新客户端 poll(since=-1) 从这里开始 → 正在跑的轮
		// 不空白（与旧 /events 的 RoundBuf 重放语义对齐）
		el.roundSeq = el.nextSeq
	}
	el.cur = append(el.cur, payload)
	el.curBytes += len(payload)
	if !coalescible(eventType) || el.curBytes >= eventBatchBytes {
		el.flushLocked()
	}
}

// flushLocked 把未发车帧打成一批发车（持锁调用）
func (el *eventLog) flushLocked() {
	if len(el.cur) == 0 {
		return
	}
	el.batches = append(el.batches, eventBatch{Seq: el.nextSeq, Frames: el.cur, Bytes: el.curBytes})
	el.bytes += el.curBytes
	el.nextSeq++
	el.cur, el.curBytes = nil, 0
	el.trimLocked()
}

// trimLocked 窗口裁剪（持锁调用）：批数或字节超上限就丢最旧，并累计缺口
func (el *eventLog) trimLocked() {
	for len(el.batches) > 0 &&
		(len(el.batches) > maxEventBatches || el.bytes > maxEventBytes) {
		el.bytes -= el.batches[0].Bytes
		el.batches = el.batches[1:]
		el.dropped++
	}
}

// takeSince 取 seq>s 的批次（先强制发车未走的帧）。
//   - s == -1：新客户端——有轮在跑就从本轮起始批开始（不空白），否则只报最新号
//     （不回放历史轮，历史由 /history 给）。
//   - s >= 0：落后超过窗口时 gap = 被跳过的批数（调用方据此提示/走 /history 兜底）。
//
// fromRoundStart 仅 s==-1 时有意义：true = 返回的批次确实从本轮开头开始
// （false = 本轮开头已被窗口挤掉，调用方应改用 RoundBuf 整轮重放兜底）
func (el *eventLog) takeSince(s int) (out []eventBatch, latest int, gap int, fromRoundStart bool) {
	el.mu.Lock()
	defer el.mu.Unlock()
	el.flushLocked()
	latest = el.nextSeq - 1
	if s < 0 {
		if el.roundSeq > 0 && el.roundSeq <= latest {
			s = el.roundSeq - 1
			fromRoundStart = el.roundSeq >= el.firstSeqLocked()
		} else {
			return nil, latest, 0, false
		}
	}
	if len(el.batches) > 0 {
		head := el.batches[0].Seq
		if s < head-1 {
			gap = head - 1 - s
			s = head - 1
		}
	}
	for _, b := range el.batches {
		if b.Seq > s {
			out = append(out, b)
		}
	}
	return out, latest, gap, fromRoundStart
}

// firstSeqLocked 窗口内最早批次号（窗口空 → nextSeq，即"没有可给的"）
func (el *eventLog) firstSeqLocked() int {
	if len(el.batches) == 0 {
		return el.nextSeq
	}
	return el.batches[0].Seq
}

// latestSeq 当前最新已发车批次号
func (el *eventLog) latestSeq() int {
	el.mu.Lock()
	defer el.mu.Unlock()
	el.flushLocked()
	return el.nextSeq - 1
}

// splitFrames 把缓冲里的一批帧拼成一次 socket 写入（/events 协议不变）
func joinFrames(bs []eventBatch) string {
	var b strings.Builder
	for _, x := range bs {
		for _, f := range x.Frames {
			b.WriteString(f)
		}
	}
	return b.String()
}
