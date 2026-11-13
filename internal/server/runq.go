package server

import (
	"context"
	"fmt"
	"okhuman/internal/textutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"okhuman/internal/agent"
	"okhuman/internal/config"
	"okhuman/internal/types"
)

func broadcastEvent(state *AppState, e types.AgentEvent) {
	if e["type"] == "run_start" {
		state.mu.Lock()
		idx := state.RunSeq + 1
		state.RunSeq = idx
		session := int(state.Agent.SessionNo.Load())
		e["session"] = session
		e["index"] = idx
		state.mu.Unlock()
	}
	payload := fmt.Sprintf("event: %s\ndata: %s\n\n", strOf(e["type"]), jsonStr(e))
	state.mu.Lock()
	// 轮次缓冲（重放用）：run_start 起新缓冲（round=当前 session），其间逐帧追加；
	// run_end 保留（结束后才连上的订阅者仍能重放该轮）。/reset 使 session 变化后
	// 旧缓冲因 round 不匹配自动失效。
	if e["type"] == "run_start" {
		// 注：run_msg 帧（本轮 user 消息文本）天然进缓冲——重放时随帧补发，
		// 刷新后"在跑那轮"的 user 气泡不丢，无需额外重建。
		// StartLen：run_start 广播在 AddMessage(user) 之前（agent.run 顺序），
		// 此刻的消息数即"当前轮之前的历史长度"——/history 据此截断（2026-09-19 修复：
		// 旧实现刷新时 /history 已含本轮已完成 LLM 调用的条目，重放又整轮重流 → 重复渲染）。
		state.RoundBuf = &roundBuf{Round: int(state.Agent.SessionNo.Load()), Frames: []string{payload}, StartLen: len(state.Agent.CM().Session().Messages)}
	} else if e["type"] == "run_end" {
		// 轮次结束清缓冲（2026-09-19）：重放条件是 running，run_end 后 running=false，
		// 保留旧缓冲只会让"刚结束瞬间"的 /history 截断与无重放错配（条目丢失）。
		// run_end 帧本身已进各订阅者，清空不影响已连客户端。
		state.RoundBuf = nil
	} else if state.RoundBuf != nil && len(state.RoundBuf.Frames) < maxRoundFrames {
		state.RoundBuf.Frames = append(state.RoundBuf.Frames, payload)
	}
	state.mu.Unlock()
	// 只进内存缓冲（2026-10-18 修 B）：写 socket 由各消费者自己的 goroutine 做，
	// run 的 emit 路径上没有任何网络动作——慢客户端卡不到 run。
	state.Evt.add(payload, strOf(e["type"]))
}

func heartbeatDue(nowMs, lastRunEndMs int64, cfg config.HeartbeatCfg, running bool) bool {
	if !cfg.Enabled || running {
		return false
	}
	mins := cfg.IntervalMinOrDefault()
	if lastRunEndMs <= 0 {
		return false // 计时基准未初始化（防御）
	}
	return nowMs-lastRunEndMs >= int64(mins)*60000
}

func runOpt(a *AgentState, state *AppState) agent.RunOptions {
	cfg := a.Cfg()
	return agent.RunOptions{
		DataDir:       cfg.Data.Dir,
		ResultLimit:   cfg.Tools.ResultLimit,
		DoomWarnAfter: cfg.Doom.WarnAfter,
		FgTimeoutMS:   cfg.Tools.FgTimeoutSec * 1000,
		// 轮内搭车（2026-09-14 统一）：与 drain（runDrain）共享同一队列、splice 原子取走，
		// 携带用户追加消息 + 已 settle 后台任务的通知（同一队列、同一逻辑）。
		// 安全：本回调只在 lock.Run 内被调用（runLoop 持锁），drain 的下一轮
		// splice 只会在本轮结束后执行 → 无并发竞态。
		TakePendingUsers: func() []types.QueuedMessage {
			state.mu.Lock()
			defer state.mu.Unlock()
			q := state.MessageQueue
			state.MessageQueue = nil
			return q
		},
		OnBackgroundStart: func(bg types.BackgroundStartArgs) {
			// settle → 格式化通知 → push 进消息队列 + 唤醒 drain（2026-09-14：
			// 与用户追加消息同一队列、同一逻辑）。闭包按引用捕获 a/state，
			// /reset 后自然指向新 agent/配置。
			bg.OnSettled = func(t *types.PendingBackgroundTask) {
				cfg := a.Cfg()
				notice := a.Agent().FormatBackgroundNotice(t, agent.RunOptions{
					ResultLimit: cfg.Tools.ResultLimit,
					DataDir:     cfg.Data.Dir,
				})
				_, _ = enqueueAndWake(state, a, notice, types.QueueKindBGNotice, makeOnEvent(state, a))
			}
			a.BG().OnBackgroundStart(bg)
		},
	}
}

// ---------- 空闲心跳（2026-10-17 用户定） ----------

// heartbeatStatus /status 里的心跳可观测字段：开关、阈值、上次 run 结束时刻、
// 当前已空闲多久、下次预计触发时刻（running 时 next_due 按"正在 run 不计时"
// 语义给 null——真实下次取决于本轮何时结束）。
func heartbeatStatus(cfg config.HeartbeatCfg, lastRunEndMs int64, running bool) map[string]interface{} {
	mins := cfg.IntervalMinOrDefault()
	out := map[string]interface{}{
		"enabled":         cfg.Enabled,
		"interval_min":    mins,
		"last_run_end_ms": lastRunEndMs,
		"idle_ms":         time.Now().UnixMilli() - lastRunEndMs,
	}
	if running || !cfg.Enabled {
		out["next_due_ms"] = nil
	} else {
		out["next_due_ms"] = lastRunEndMs + int64(mins)*60000
	}
	return out
}

// StartHeartbeat 起空闲心跳巡检：每 heartbeatTickMS 检查一次，满足 heartbeatDue
// → 把心跳提示词经 enqueueAndWake 入队并唤醒 drain——与后台完成通知、/enqueue
// 完全同一条路径（串行锁 / 事件广播 / 落盘 / 压缩全部复用，不另起 run 通道）。
//
// 触发即把 LastRunEnd 归零：同一空闲期不会反复入队；随后本轮 run 结束时
// runDrain 还会再归零一次。配置每 tick 现取（COW）→ WebUI 改开关/间隔/提示词
// 无需重启进程。返回 stop 停表（进程常驻，一般不调用）。
func StartHeartbeat(state *AppState, a *AgentState) func() {
	return startHeartbeat(state, a, heartbeatTickMS)
}

// assembleBatch 把 drain 一批内积攒的全部消息组装成发给 LLM 的用户消息
// （2026-09-14 从 runDrain 提出，便于单测）：1 条 = 该条本身（kind=user，
// 不加前缀）；>1 条 = 合并成一条【第 1 条】…【第 2 条】…（kind=merged，
// label "N 条合并 / M 字符"）。这是「无车」新轮路径的组装；「有车」轮内
// 搭车走 agent 的 TakePendingUsers，每条独立成 user 消息，不经过本函数。
func assembleBatch(msgs []types.QueuedMessage) (message, kind string, label *string, messages int) {
	if len(msgs) == 1 {
		return msgs[0].Text, "user", nil, 0
	}
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = fmt.Sprintf("【第 %d 条】%s", i+1, m.Text)
	}
	msg := strings.Join(parts, "\n")
	// 数的是**真正发给 agent 的那条拼接串**（含【第 i 条】前缀与分隔）；
	// 早先只把各条原文相加，标签里的 M 偏小（2026-11-12 复审指出）。
	l := fmt.Sprintf("%d 条合并 / %d 字符", len(msgs), textutil.UTF16Len(msg))
	return msg, "merged", &l, len(msgs)
}

// enqueueAndWake 唯一入队入口（2026-09-14）：在 state.mu 内原子完成
// "取队列位置 + 入队 + 取/起活动 drain"——入队与 drain 的终止判定同一把锁，
// 不存在"drain 正要退出时入队落空、消息挂起"的竞态（旧 startDrain 两把锁
// 判定之间的窗口，2026-09-14 修）。两个调用方：/enqueue 与后台 settle
// 与后台 settle 通知（同一逻辑）。返回 handle（可等 done 读 res）+ 入队前
// 队列已有条数。
// kind 决定这条消息在轮内搭车时怎么被标记（types.QueueKindUser /
// QueueKindBGNotice，2026-10-25）——不再由正文前缀反推。
func enqueueAndWake(state *AppState, a *AgentState, msg string, kind string, onEvent func(types.AgentEvent)) (*drainHandle, int) {
	if kind == "" {
		kind = types.QueueKindUser
	}
	state.mu.Lock()
	queuedBefore := len(state.MessageQueue)
	state.MessageQueue = append(state.MessageQueue, types.QueuedMessage{Text: msg, Kind: kind})
	if state.ActiveDrain != nil {
		h := state.ActiveDrain
		state.mu.Unlock()
		return h, queuedBefore
	}
	h := &drainHandle{done: make(chan struct{})}
	state.ActiveDrain = h
	state.mu.Unlock()
	go func() {
		defer close(h.done)
		// run 内部 panic 隔离（2026-09-14 审计修：旧实现 panic 直接崩掉整个
		// HTTP 进程；现在转成错误结果——消息队列保留、下次入口重试，ActiveDrain
		// 同步摘除避免后续入队挂在死 handle 上）
		reply, ms, err := func() (string, int64, error) {
			var r2 string
			var m2 int64
			var innerErr error
			defer func() {
				if r := recover(); r != nil {
					state.mu.Lock()
					if state.ActiveDrain == h {
						state.ActiveDrain = nil
					}
					state.mu.Unlock()
					innerErr = fmt.Errorf("run panic（已隔离，消息队列保留待重试）：%v", r)
				}
			}()
			r2, m2, innerErr = runDrain(state, a, h, onEvent)
			return r2, m2, innerErr
		}()
		h.res = drainResult{Reply: reply, MS: ms, Err: err}
		if err != nil {
			// 错误广播只在此处发生一次（drain 层，2026-08-31 修）：多个入口
			// 共享同一 drain，若各自广播 → 错误卡重复出现。
			broadcastEvent(state, types.AgentEvent{"type": "error", "message": "队列处理失败：" + err.Error()})
		}
	}()
	return h, queuedBefore
}

// injectDir 附件侧车目录（<data.dir>/inject）。此前七处各自拼一遍这个布局，
// 换目录名就得改七处（2026-11-12 归并）。
func injectDir(dataDir string) string {
	return filepath.Join(dataDir, "inject")
}

// injectDirOf 取当前生效配置下的侧车目录（COW：读到的永远是完整一致的快照）。
func (ap *App) injectDirOf() string {
	return injectDir(ap.state.Agent.Cfg().Data.Dir)
}

// logRunEvent run 事件里的 doom/bg 记录（所有入口统一）
func logRunEvent(a *AgentState, e types.AgentEvent) {
	switch e["type"] {
	case "doom_warn": // 死循环警告记 warn
		a.logMu.Lock()
		a.DoomLog = append(a.DoomLog, DoomLogEntry{At: time.Now().UnixMilli(), Kind: "warn", Detail: strOf(e["detail"])})
		a.logMu.Unlock()
	case "doom_stop":
		a.logMu.Lock()
		a.DoomLog = append(a.DoomLog, DoomLogEntry{At: time.Now().UnixMilli(), Kind: "stop", Detail: strOf(e["detail"])})
		a.logMu.Unlock()
	case "bg_start":
		a.logMu.Lock()
		a.DoomLog = append(a.DoomLog, DoomLogEntry{At: time.Now().UnixMilli(), Kind: "warn", Detail: fmt.Sprintf("工具 %s 前台超时转后台（call %s）", strOf(e["tool_name"]), strOf(e["call_id"]))})
		a.logMu.Unlock()
	}
}

// makeOnEvent 本实例的统一事件回调：记日志（doom/bg）+ 广播给所有 /events 订阅者。
// 所有 run 入口（/enqueue 排空 / 定时任务触发）共用。
func makeOnEvent(state *AppState, a *AgentState) func(types.AgentEvent) {
	return func(e types.AgentEvent) {
		logRunEvent(a, e)
		broadcastEvent(state, e)
	}
}

// queuedItems 队列条目的结构化投影（text + kind）：webui 据此分"系统通知 /
// 用户"气泡，不必再在前端硬编码前缀去猜。旧的 queued 字段照旧输出
// （queuedTexts），外部消费者协议不变——新增字段向后兼容。
func queuedItems(qs []types.QueuedMessage) []types.QueuedMessage {
	if len(qs) == 0 {
		return nil
	}
	return append([]types.QueuedMessage(nil), qs...)
}

// queuedTexts 队列条目的正文投影：对外 JSON（/queue、/events hello）里的
// queued 一直是字符串数组，这里把内部结构挡住——qq-channel / voice-chat /
// webui 读到的字节不变。空队列返回 nil（与旧 append([]string(nil), ...) 同
// 行为，JSON 里是 null 不是 []）。
func queuedTexts(qs []types.QueuedMessage) []string {
	if len(qs) == 0 {
		return nil
	}
	out := make([]string, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.Text)
	}
	return out
}

// runBatch drain 的一批：把队列消息合并成一条发给 agent（lock.Run 串行）。
// kind/label/messages 只用于事件广播标记（run_start 帧），不影响注入内容。
func runBatch(a *AgentState, opt agent.RunOptions, message, kind string, label *string, messages int, onEvent func(types.AgentEvent)) (string, error) {
	opt.OnEvent = onEvent
	if kind != "" {
		opt.Kind = kind
		opt.Label = label
		if messages > 0 {
			opt.Messages = messages
		}
	}
	return a.Agent().Run(context.Background(), message, opt)
}

// runDrain 排空消息队列（2026-08-31 厄运定；2026-09-14 竞态修）：
// 每次迭代在 state.mu 内把"队列里积攒的全部消息"splice 出来合并成一条发给
// agent（>1 条 = merged），发完再取下一批（运行中新入队的，含后台 settle
// 通知）。队列空 = 本 drain 完成（同一把锁内判空 + 摘除 ActiveDrain，与
// enqueueAndWake 互斥 → 无竞态窗口）。
// run 抛错时停止排空（队列保留，下次入口继续）。
func runDrain(state *AppState, a *AgentState, h *drainHandle, onEvent func(types.AgentEvent)) (string, int64, error) {
	started := time.Now()
	var reply string
	for {
		var msgs []types.QueuedMessage
		var batchErr error
		// splice + 发送在 RunLock 内原子完成（2026-09-14 审计 H1 修）：/reset 也持同一
		// 锁清队列+重建 → 两者互斥——要么整批在 reset 前跑完（旧会话），要么 reset
		// 已清空队列、本 drain 见空即退，不存在"splice 出的旧消息跑上新会话"的窗口。
		// 锁序 RunLock→state.mu 与 enqueueAndWake（仅 state.mu）无环。
		a.Lock.Run(func() error {
			state.mu.Lock()
			msgs = state.MessageQueue
			state.MessageQueue = nil
			if len(msgs) == 0 && state.ActiveDrain == h {
				state.ActiveDrain = nil // 与 enqueueAndWake 同一把锁内摘除 → 无竞态窗口
			}
			state.mu.Unlock()
			if len(msgs) == 0 {
				return nil // 防御：空队列不发 LLM 调用
			}
			message, kind, label, messages := assembleBatch(msgs)
			r, err := runBatch(a, runOpt(a, state), message, kind, label, messages, onEvent)
			if err == nil {
				reply = r
			}
			batchErr = err
			return err
		})
		// 心跳计时（2026-10-17）：一批 run 结束（无论成功/出错）即归零空闲计时——
		// 用户口径"只有 run 结束才重置"。放在 Lock.Run 之后：此刻本轮真的结束了。
		state.LastRunEnd.Store(time.Now().UnixMilli())
		if len(msgs) == 0 {
			break // 空队列（ActiveDrain 已在锁内摘除）→ 本 drain 完成
		}
		if batchErr != nil {
			state.mu.Lock()
			if state.ActiveDrain == h {
				state.ActiveDrain = nil
			}
			state.mu.Unlock()
			return reply, time.Since(started).Milliseconds(), batchErr
		}
	}
	return reply, time.Since(started).Milliseconds(), nil
}

// startHeartbeat 实现体（tickMS 可注入：生产 30000，测试用毫秒级以便断言）。
func startHeartbeat(state *AppState, a *AgentState, tickMS int) func() {
	if tickMS <= 0 {
		tickMS = heartbeatTickMS
	}
	t := time.NewTicker(time.Duration(tickMS) * time.Millisecond)
	go func() {
		for range t.C {
			hb := a.Cfg().Heartbeat
			if !hb.Enabled {
				continue
			}
			now := time.Now().UnixMilli()
			state.mu.Lock()
			running := a.Lock.Running() || state.ActiveDrain != nil
			last := state.LastRunEnd.Load()
			state.mu.Unlock()
			if !heartbeatDue(now, last, hb, running) {
				continue
			}
			state.LastRunEnd.Store(now)
			fmt.Fprintf(os.Stderr, "[heartbeat] 空闲 %d 分钟无 run → 主动开启一轮（阈值 %d 分钟）\n",
				(now-last)/60000, hb.IntervalMin)
			// 心跳走 user：它只在空闲时起新轮（heartbeatDue 见 running 直接
			// 不触发），不会搭车 → 行为与改动前逐字一致。
			enqueueAndWake(state, a, hb.Prompt, types.QueueKindUser, makeOnEvent(state, a))
		}
	}()
	return t.Stop
}
