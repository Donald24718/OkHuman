package background

// 后台工具任务编排（§7.3，2026-08-25 定；2026-09-14 统一：与用户追加消息同一队列、
// 同一逻辑；2026-08-31 的"纯搭车不自唤醒"被取代——没车时作为新消息发下一轮）。
//
// 与运行锁（RunLock）配合：所有 run（用户消息 / 后台通知 / 定时任务）串行执行，
// 避免并发写上下文。双向通知：
//   - 转后台时：agent 已立即回填"已转后台"的 tool 消息（模型不必傻等）；
//   - 后台 settle 时：调 OnSettled 回调（server.runOpt 接线）→ 格式化成
//     "（系统通知：后台任务完成…）"→ push 进实例消息队列（与用户追加消息
//     同一条队列）+ 唤醒 drain。两条消费路径，均不抢 LLM：
//       1) 有车（本轮在跑）：轮内搭车——工具循环中每次 LLM 调用之前被
//          takePendingUsers 取走，与当批工具结果同车注入，纯追加：不加
//          LLM 调用、不加 KV 缓存代价（已缓存前缀不变）；
//       2) 没车（本轮已结束 / 尚未开始）：本轮完全结束后 drain 把队列拼成
//          新消息发新轮（RunLock 排队串行——新轮一定在当前 run 释放锁之后
//          才开始，llama.cpp 同一时刻对同一 agent 只有一个请求）；agent
//          完全空闲时入队即起新轮（与用户消息语义一致）。
//   - 任务表保留已 settle 任务直到 /reset（/backgrounds 可见性）；入库
//     Result 截到 256KB 封顶，防长会话堆大结果。
//   RunLock 的 running/waitIdle 保留（运行串行 + 事件广播判 running 用）。

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"okhuman/internal/tools"
	"okhuman/internal/types"
)

// RunLock 运行锁：串行执行 run（同 agent 内互斥，避免并发写上下文）
type RunLock struct {
	ch          chan struct{} // 容量 1 的令牌
	running     atomic.Bool
	mu          sync.Mutex
	idleWaiters []func()
}

// NewRunLock 新建运行锁
func NewRunLock() *RunLock {
	return &RunLock{ch: make(chan struct{}, 1)}
}

// Running 是否有 run 在执行
func (l *RunLock) Running() bool { return l.running.Load() }

// Run 排队执行 fn；返回 fn 的结果/错误，排队本身永不丢（上一个 run 即使失败也继续）
func (l *RunLock) Run(fn func() error) error {
	l.ch <- struct{}{}
	l.running.Store(true)
	err := fn()
	l.running.Store(false)
	// 队列清空 → 通知所有 waitIdle（在 running=false 之后唤醒，
	// 唤醒方看到的 running 已是最新值）
	l.mu.Lock()
	waiters := l.idleWaiters
	l.idleWaiters = nil
	l.mu.Unlock()
	for _, w := range waiters {
		w()
	}
	<-l.ch
	return err
}

// WaitIdle 等队列完全空闲（当前 run + 排队 run 都结束）；已空闲则立即返回
func (l *RunLock) WaitIdle() {
	if !l.running.Load() {
		return
	}
	done := make(chan struct{})
	l.mu.Lock()
	l.idleWaiters = append(l.idleWaiters, func() { close(done) })
	l.mu.Unlock()
	<-done
}

// BackgroundOrchestrator 后台任务编排：注册任务 + settle 检测 + 通知入队。
// settle 时经任务自带的 OnSettled 回调（runOpt 接线）把通知 push 进消息
// 队列——本结构体只管任务状态，不碰队列（队列属 server 层）。
type BackgroundOrchestrator struct {
	mu    sync.Mutex
	tasks map[string]*types.PendingBackgroundTask
}

// NewOrchestrator 新建编排器
func NewOrchestrator() *BackgroundOrchestrator {
	return &BackgroundOrchestrator{tasks: map[string]*types.PendingBackgroundTask{}}
}

// maxStoredResultBytes 任务表内 Result 的存储上限（256KB）：通知入队用的是
// 完整结果（由 server 格式化），表里只留截断副本供 /backgrounds 查看。
const maxStoredResultBytes = 256 * 1024

// OnBackgroundStart 由 Agent 的工具循环调用：注册任务 + 挂 settle 检测
//（Promise 通道收到结果 → 标记 settled/ok/durationMs → 调 a.OnSettled 把
// 通知入消息队列 → 标记 Notified）
func (o *BackgroundOrchestrator) OnBackgroundStart(a types.BackgroundStartArgs) {
	t := &types.PendingBackgroundTask{
		CallID:     a.CallID,
		ToolName:   a.ToolName,
		StartedAt:  a.StartedAt,
		DurationMs: 0,
		OK:         false,
		Result:     "",
	}
	o.mu.Lock()
	o.tasks[a.CallID] = t
	o.mu.Unlock()
	go func() {
		result := <-a.Promise
		now := time.Now().UnixMilli()
		o.mu.Lock()
		cur, exists := o.tasks[a.CallID]
		if exists {
			cur.Settled = true
			cur.OK = !strings.HasPrefix(result, tools.ToolFailPrefix) // 工具异常已被 agent 并入字符串
			cur.Result = result
			cur.DurationMs = now - a.StartedAt
			cur.SettledAt = now
		}
		o.mu.Unlock()
		if !exists {
			return
		}
		// settle → 通知入队（2026-09-14：与用户追加同一队列、同一逻辑）。
		// 回调在 o.mu 外调用——回调链是 入队→drain→lock.Run，避免重入死锁。
		if a.OnSettled != nil {
			a.OnSettled(cur)
		}
		o.mu.Lock()
		cur.Notified = true
		if len(cur.Result) > maxStoredResultBytes {
			cur.Result = capBytes(cur.Result, maxStoredResultBytes) + "\n[已截断]"
		}
		o.mu.Unlock()
	}()
}

// capBytes 把 s 截到前 n 字节，回退到完整 UTF-8 字符边界
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// Snapshot 全部任务（含未完成）—— /backgrounds 端点用
func (o *BackgroundOrchestrator) Snapshot() []types.PendingBackgroundTask {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]types.PendingBackgroundTask, 0, len(o.tasks))
	for _, t := range o.tasks {
		out = append(out, *t)
	}
	return out
}

// Count 任务总数（含未完成）
func (o *BackgroundOrchestrator) Count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.tasks)
}

// Clear 清空（会话重置时调用）
func (o *BackgroundOrchestrator) Clear() {
	o.mu.Lock()
	o.tasks = map[string]*types.PendingBackgroundTask{}
	o.mu.Unlock()
}
