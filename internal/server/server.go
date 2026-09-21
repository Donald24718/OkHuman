// Package server HTTP 服务（移植自 TS src/server.ts）：
// 端点全集与 TS 逐字段同形（插件 qq-channel/cron 依赖的协议不变）：
//
//	GET  /health /status /queue /history /backgrounds /attachments
//	GET  /events (SSE 常驻广播) /prompts /prompts/file /config
//	GET  / /favicon.ico /marked.min.js (WebUI 静态)
//	POST /chat /chat/stream (SSE) /enqueue /inject /drop /reset /stop
//	POST /prompts/file /prompts/reload /config
//
// 单实例模型（§11）：一个进程 = 一个 agent；事件广播集 + 消息队列 +
// 活动 drain + 轮次缓冲集中在 AppState，三入口（/chat /chat/stream /enqueue）
// 共享同一 drain（splice 队列不并发）。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"okhuman/internal/agent"
	"okhuman/internal/background"
	"okhuman/internal/config"
	ctxmgr "okhuman/internal/context"
	"okhuman/internal/inject"
	"okhuman/internal/llm"
	"okhuman/internal/prompt"
	"okhuman/internal/types"
)

// ---------- 状态结构（对应 TS AgentState / AppState） ----------

// PromptFileInfo 提示词文件元信息（/status /prompts 返回，json 字段与 TS 一致）
type PromptFileInfo struct {
	Name  string `json:"name"`
	Chars int    `json:"chars"`
}

// CompressLogEntry 压缩日志条目（/status compress 字段）
type CompressLogEntry struct {
	At     int64  `json:"at"`
	Scope  string `json:"scope"`
	Detail string `json:"detail"`
	Before int    `json:"before"`
	After  int    `json:"after"`
	Rounds int    `json:"rounds"`
}

// DoomLogEntry doom/bg 日志条目（/status doom 字段）
type DoomLogEntry struct {
	At     int64  `json:"at"`
	Kind   string `json:"kind"` // warn | stop
	Detail string `json:"detail"`
}

// InjectInfo 上下文注入附件（/attachments 返回；note 空串也保留，与 TS 一致）
type InjectInfo struct {
	ID        string  `json:"id"`
	Bytes     int     `json:"bytes"`
	CreatedAt int64   `json:"created_at"`
	Note      *string `json:"note,omitempty"`
}

// agentRuntime CM/Agent/BG 三件套：/reset 整体重建，经 atomic.Pointer 原子换入
// （2026-09-14 审计修：旧实现直接改 AgentState 字段——/inject 等不持运行锁的
// HTTP 路径与 /reset 并发时读写同一字段 → 数据竞争，且注入消息可能落进已废弃
// 的旧 CM 丢失）
type agentRuntime struct {
	CM    *ctxmgr.Manager
	Agent *agent.Agent
	BG    *background.BackgroundOrchestrator
}

// AgentState 单个 agent 的全部运行时状态（单实例）
type AgentState struct {
	CfgRef         atomic.Pointer[config.Config] // 配置（COW：热更新生成新对象，旧对象不可变）
	rt             atomic.Pointer[agentRuntime]  // 运行时三件套（/reset 原子换入）
	PromptDir      string
	LLM            llm.LlmClient
	llmMu          sync.Mutex // 保护 LLM（配置热更重建写 / reset 读）
	promptMu       sync.Mutex // 保护 SystemPrompt / PromptFiles / PromptFallback（提示词页重载写 / 只读端点读）
	SystemPrompt   string
	PromptFiles    []PromptFileInfo
	PromptFallback bool
	SessionNo      atomic.Int32
	Lock           *background.RunLock
	logMu          sync.Mutex // 保护 CompressLog / DoomLog
	CompressLog    []CompressLogEntry
	DoomLog        []DoomLogEntry
}

// Cfg 当前配置（COW 读：拿到的是不可变对象，无需再锁）
func (a *AgentState) Cfg() *config.Config { return a.CfgRef.Load() }

// CM 当前上下文管理器（/reset 后指向新会话）
func (a *AgentState) CM() *ctxmgr.Manager { return a.rt.Load().CM }

// Agent 当前 agent 运行时（/reset 后指向新实例）
func (a *AgentState) Agent() *agent.Agent { return a.rt.Load().Agent }

// BG 当前后台编排器（/reset 后指向新实例）
func (a *AgentState) BG() *background.BackgroundOrchestrator { return a.rt.Load().BG }

// roundBuf 轮次事件缓冲（重放用）
type roundBuf struct {
	Round  int
	Frames []string
	// StartLen run_start 时刻会话消息数——当前轮的条目从下标 StartLen 起
	// （run_start 广播在 AddMessage(user) 之前，见 agent.run）。
	// 刷新时 /history 截断到 StartLen、/events 重放整轮 → 不重复不丢失。
	StartLen int
}

// drainHandle 活动 drain 句柄（enqueueAndWake 返回；等待 done 后读 res）
type drainHandle struct {
	done chan struct{}
	res  drainResult
}

type drainResult struct {
	Reply string
	MS    int64
	Err   error
}

// subEntry 事件广播订阅（2026-09-14 审计修：带唯一 id——旧实现用
// reflect.ValueOf(fn).Pointer() 判同，同一函数字面量产生的所有闭共享同一
// 代码指针 → 任一 /events 连接断开时会把其他连接的订阅一并摘除）
type subEntry struct {
	ID uint64
	Fn func(string)
}

var subSeq atomic.Uint64 // 订阅 id 发号器

// AppState 单实例服务状态
type AppState struct {
	Root          string // 项目根（webui 静态文件 / config/user.json 落盘用）
	Agent         *AgentState
	mu            sync.Mutex // 保护下面全部字段
	BroadcastSubs []subEntry
	MessageQueue  []string
	ActiveDrain   *drainHandle
	RoundBuf      *roundBuf
	RunSeq        int
	InjectSeq     int
	Injects       []InjectInfo
}

// NewAppState AppState 工厂（2026-08-31）：统一初始化事件广播集 + 消息队列 + 活动 drain + 轮次缓冲。
// cfg 参数保留兼容（配置统一存于 AgentState.CfgRef）。
func NewAppState(cfg *config.Config, root string, agent *AgentState) *AppState {
	return &AppState{
		Root:          root,
		Agent:         agent,
		BroadcastSubs: []subEntry{},
		MessageQueue:  []string{},
		Injects:       []InjectInfo{},
	}
}

// ---------- agent 运行时构建（对应 TS createAgentState / makeCm） ----------

// CreateAgentState 创建单实例 agent 的运行时（上下文 / 运行锁 / 后台编排）。
// systemPrompt / promptFiles / promptFallback / promptDir 由调用方注入
// （入口恢复落盘状态用）。
func CreateAgentState(cfg *config.Config, client llm.LlmClient, systemPrompt string, promptFiles []PromptFileInfo, promptFallback bool, promptDir string) *AgentState {
	a := &AgentState{
		PromptDir:      promptDir,
		LLM:            client,
		SystemPrompt:   systemPrompt,
		PromptFiles:    promptFiles,
		PromptFallback: promptFallback,
		Lock:           background.NewRunLock(),
		CompressLog:    []CompressLogEntry{},
		DoomLog:        []DoomLogEntry{},
	}
	a.CfgRef.Store(cfg)
	a.SessionNo.Store(1)
	cm := makeCm(a)
	a.rt.Store(&agentRuntime{CM: cm, Agent: agent.New(client, systemPrompt, cm, filepath.Join(a.Cfg().Data.Dir, "inject")), BG: background.NewOrchestrator()})
	return a
}

// makeCm 建 ContextManager（落盘根 = 本实例目录，已含端口后缀：
// session-records / tool-results 与 session.json 同处一个目录）
func makeCm(a *AgentState) *ctxmgr.Manager {
	cfg := a.Cfg()
	a.llmMu.Lock()
	client := a.LLM
	a.llmMu.Unlock()
	a.promptMu.Lock()
	sp := a.SystemPrompt
	a.promptMu.Unlock()
	m := ctxmgr.NewManager(client, ctxmgr.Cfg{
		MaxTokens:       cfg.Context.MaxTokens,
		KeepRecentChars: cfg.Context.KeepRecentChars,
		HardTruncChars:  cfg.Context.HardTruncChars,
		StreamIdleMS:    cfg.Context.StreamIdleMS,
		CharsPerToken:   cfg.Context.CharsPerToken,
	}, sp, cfg.Data.Dir)
	m.SetSessionNo(int(a.SessionNo.Load()))
	m.AddCompressListener(func(e ctxmgr.CompressEvent) { pushCompressLog(a, e) })
	// 自我生命感知（2026-09-20）：本实例端口/pid/源码位置 + LLM 端口/pid，compose 时
	// 实时注入系统提示词首
	m.SetSelfInfo(ctxmgr.SelfInfo{Port: cfg.Server.Port, DataDir: cfg.Data.Dir, LLMBaseURL: cfg.LLM.BaseURL})
	return m
}

// llmCfgOf config.LLMCfg → llm.ClientConfig
func llmCfgOf(c config.LLMCfg) llm.ClientConfig {
	return llm.ClientConfig{BaseURL: c.BaseURL, Model: c.Model, APIKey: c.APIKey, TimeoutMS: c.TimeoutMS}
}

func pushCompressLog(a *AgentState, e ctxmgr.CompressEvent) {
	a.logMu.Lock()
	a.CompressLog = append(a.CompressLog, CompressLogEntry{
		At:     time.Now().UnixMilli(),
		Scope:  e.Scope,
		Detail: e.Detail,
		Before: e.BeforeChars,
		After:  e.AfterChars,
		Rounds: e.Rounds,
	})
	a.logMu.Unlock()
}

// ---------- run 入口（对应 TS runOpt / drainLoop / startDrain；Go 侧 2026-09-14 起为 runBatch / runDrain / enqueueAndWake） ----------

// runOpt 本实例的 run 选项（用户消息 / 插件经 POST /chat 触发共用；每轮现取最新配置）
func runOpt(a *AgentState, state *AppState) agent.RunOptions {
	cfg := a.Cfg()
	return agent.RunOptions{
		DataDir:       cfg.Data.Dir,
		ResultLimit:   cfg.Tools.ResultLimit,
		DoomWarnAfter: cfg.Doom.WarnAfter,
		FgTimeoutMS:   cfg.Tools.FgTimeoutMS,
		// 轮内搭车（2026-09-14 统一）：与 drain（runDrain）共享同一队列、splice 原子取走，
		// 携带用户追加消息 + 已 settle 后台任务的通知（同一队列、同一逻辑）。
		// 安全：本回调只在 lock.Run 内被调用（runLoop 持锁），drain 的下一轮
		// splice 只会在本轮结束后执行 → 无并发竞态。
		TakePendingUsers: func() []string {
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
				_, _ = enqueueAndWake(state, a, notice, makeOnEvent(state, a))
			}
			a.BG().OnBackgroundStart(bg)
		},
	}
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

// broadcastEvent 广播一条事件给所有 SSE 订阅者（/events 常驻通道）。
// 每个订阅者独立隔离——某个慢客户端写失败不影响其他订阅者。
// 同时维护"轮次缓冲"（roundBuf）：run_start→run_end 的帧逐条记录，
// 新订阅者连上且正在跑时重放（见 /events），刷新后不丢已流出内容。
// maxRoundFrames 轮次缓冲帧数上限（2026-09-14 审计修：旧实现无界，超长 run
// 的 delta 帧可把内存撑大；超限后停止追加——实时流不受影响，仅新连接重放
// 不完整）
const maxRoundFrames = 100000

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
	subs := append([]subEntry(nil), state.BroadcastSubs...)
	state.mu.Unlock()
	if len(subs) == 0 {
		return
	}
	for _, s := range subs {
		func() {
			defer func() { _ = recover() }() // 订阅者写失败：由其对端断开清理
			s.Fn(payload)
		}()
	}
}

// logRunEvent run 事件里的 doom/bg 记录（所有入口统一）
func logRunEvent(a *AgentState, e types.AgentEvent) {
	switch e["type"] {
	case "doom_warn", "trunc_warn": // 异常截断（2026-09-15）与死循环警告同记 warn
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
// 所有 run 入口（/chat /chat/stream /enqueue 排空 / 定时任务触发）共用。
func makeOnEvent(state *AppState, a *AgentState) func(types.AgentEvent) {
	return func(e types.AgentEvent) {
		logRunEvent(a, e)
		broadcastEvent(state, e)
	}
}

// enqueueAndWake 唯一入队入口（2026-09-14）：在 state.mu 内原子完成
// "取队列位置 + 入队 + 取/起活动 drain"——入队与 drain 的终止判定同一把锁，
// 不存在"drain 正要退出时入队落空、消息挂起"的竞态（旧 startDrain 两把锁
// 判定之间的窗口，2026-09-14 修）。四个调用方：/chat /chat/stream /enqueue
// 与后台 settle 通知（同一逻辑）。返回 handle（可等 done 读 res）+ 入队前
// 队列已有条数。
func enqueueAndWake(state *AppState, a *AgentState, msg string, onEvent func(types.AgentEvent)) (*drainHandle, int) {
	state.mu.Lock()
	queuedBefore := len(state.MessageQueue)
	state.MessageQueue = append(state.MessageQueue, msg)
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

// assembleBatch 把 drain 一批内积攒的全部消息组装成发给 LLM 的用户消息
// （2026-09-14 从 runDrain 提出，便于单测）：1 条 = 该条本身（kind=user，
// 不加前缀）；>1 条 = 合并成一条【第 1 条】…【第 2 条】…（kind=merged，
// label "N 条合并 / M 字符"）。这是「无车」新轮路径的组装；「有车」轮内
// 搭车走 agent 的 TakePendingUsers，每条独立成 user 消息，不经过本函数。
func assembleBatch(msgs []string) (message, kind string, label *string, messages int) {
	if len(msgs) == 1 {
		return msgs[0], "user", nil, 0
	}
	total := 0
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = fmt.Sprintf("【第 %d 条】%s", i+1, m)
		total += utf16Len(m)
	}
	l := fmt.Sprintf("%d 条合并 / %d 字符", len(msgs), total)
	return strings.Join(parts, "\n"), "merged", &l, len(msgs)
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
		var msgs []string
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

// ---------- 运行时配置热更新（WebUI 配置页，2026-08-29，2026-09-09 单实例化） ----------

var hotSections = []string{"llm", "context", "tools", "doom"}
var restartSections = []string{"server", "data", "system_prompt"} // 改后需重启进程

type PatchResult struct {
	Applied []string
	Restart []string
}

// applyConfigPatch 把配置补丁应用到全局活对象 + 本实例 agent。
// 即时生效的段：llm（重建客户端）/ context（压缩参数）/
//
//	tools（fg_timeout_ms，runOpt 每轮取最新）/ doom（warn_after，每轮取最新）。
//	需重启的段：server / data / system_prompt。
//
// 返回 { applied: 即时生效的段, restart: 需要重启才生效的段 }。
func applyConfigPatch(a *AgentState, patch map[string]interface{}) PatchResult {
	applied := []string{}
	restart := []string{}
	has := func(s string) bool { _, ok := patch[s]; return ok }

	if has("server") {
		restart = append(restart, "server（端口/地址，需重启进程）")
	}
	if has("data") {
		restart = append(restart, "data（落盘根目录，需重启进程）")
	}
	if has("system_prompt") {
		restart = append(restart, "system_prompt（提示词目录，需重启进程；单文件编辑请用提示词页）")
	}
	if has("llm") {
		applied = append(applied, "llm（重建 LLM 客户端，即时生效）")
	}
	if has("context") {
		applied = append(applied, "context（压缩参数，即时生效）")
	}
	if has("tools") {
		applied = append(applied, "tools（前台超时/结果上限，下轮运行生效）")
	}
	if has("doom") {
		applied = append(applied, "doom（死循环告警阈值，下轮运行生效）")
	}

	// COW 热更新（2026-09-14 审计修：旧实现 ApplyPatchInPlace 原地改共享
	// *Config——runOpt / 只读端点的并发读与写入数据竞争，字符串字段撕裂读可
	// 致崩溃）：DeepMerge 生成全新不可变 Config，先更新从属对象（LLM 客户端 /
	// 压缩参数），最后原子换入 CfgRef——读方要么拿旧配置要么拿新配置，完整一致。
	old := a.Cfg()
	nc := config.DeepMerge(old, patch)
	if has("llm") && llmCfgOf(nc.LLM) != llmCfgOf(old.LLM) {
		client := llm.NewOpenAiClient(llmCfgOf(nc.LLM))
		a.llmMu.Lock()
		a.LLM = client
		a.llmMu.Unlock()
		a.Agent().SetLLM(client)
		a.CM().SetLLM(client)
	}
	if has("context") {
		a.CM().SetCfg(ctxmgr.Cfg{
			MaxTokens:       nc.Context.MaxTokens,
			KeepRecentChars: nc.Context.KeepRecentChars,
			HardTruncChars:  nc.Context.HardTruncChars,
			StreamIdleMS:    nc.Context.StreamIdleMS,
			CharsPerToken:   nc.Context.CharsPerToken,
		})
	}
	// 自我生命感知输入随配置热更新（llm base_url / server 段变化）
	a.CM().SetSelfInfo(ctxmgr.SelfInfo{Port: nc.Server.Port, DataDir: nc.Data.Dir, LLMBaseURL: nc.LLM.BaseURL})
	a.CfgRef.Store(nc)
	// tools / doom 段：由 runOpt 每轮现取，无需动作
	return PatchResult{Applied: dedupe(applied), Restart: dedupe(restart)}
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// reloadAgentPrompt 加载提示词（WebUI 提示词页重载用）：
// 目录取自 a.PromptDir；无目录 → 保持现状（fallback 内置提示词）。
func reloadAgentPrompt(a *AgentState) prompt.Loaded {
	// promptMu 全程持锁（2026-09-14 审计修：SystemPrompt / PromptFiles /
	// PromptFallback 与只读端点（/status /prompts）的并发读竞态；重载含读盘
	// 阻塞，持锁时间略长但可接受——提示词页低频操作）
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	if a.PromptDir == "" {
		files := make([]prompt.File, 0, len(a.PromptFiles))
		for _, f := range a.PromptFiles {
			files = append(files, prompt.File{Name: f.Name, Chars: f.Chars})
		}
		return prompt.Loaded{Prompt: a.SystemPrompt, Files: files, Fallback: a.PromptFallback}
	}
	entries, err := os.ReadDir(a.PromptDir)
	if err != nil {
		r := prompt.Loaded{Prompt: prompt.DefaultSystemPrompt, Files: []prompt.File{}, Fallback: true}
		a.SystemPrompt = r.Prompt
		a.PromptFiles = nil
		a.PromptFallback = true
		a.CM().SetSystemPrompt(r.Prompt)
		a.Agent().SetSystemPrompt(r.Prompt)
		return r
	}
	var mdNames []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			mdNames = append(mdNames, e.Name())
		}
	}
	sort.Slice(mdNames, func(i, j int) bool { return prompt.NaturalCompare(mdNames[i], mdNames[j]) < 0 })
	files := make([]PromptFileInfo, 0, len(mdNames))
	chunks := make([]string, 0, len(mdNames))
	for _, name := range mdNames {
		data, err := os.ReadFile(filepath.Join(a.PromptDir, name))
		if err != nil {
			continue
		}
		text := string(data)
		files = append(files, PromptFileInfo{Name: name, Chars: utf16Len(text)})
		chunks = append(chunks, text)
	}
	var p string
	if len(chunks) > 0 {
		p = strings.Join(chunks, "\n\n")
	} else {
		p = prompt.DefaultSystemPrompt
	}
	fallback := len(chunks) == 0
	a.SystemPrompt = p
	a.PromptFiles = files
	a.PromptFallback = fallback
	a.CM().SetSystemPrompt(p)
	a.Agent().SetSystemPrompt(p)
	pfiles := make([]prompt.File, 0, len(files))
	for _, f := range files {
		pfiles = append(pfiles, prompt.File{Name: f.Name, Chars: f.Chars})
	}
	return prompt.Loaded{Prompt: p, Files: pfiles, Fallback: fallback}
}

// ---------- App（对应 TS createApp） ----------

// Hooks /reset 重建会话后回调（重挂落盘监听）；配置页保存后回调（入口落盘 config/user.json 用）
type Hooks struct {
	OnSessionRebuilt func(a *AgentState)
	OnConfigSaved    func(patch map[string]interface{})
}

// App HTTP 应用（实现 http.Handler）
type App struct {
	state    *AppState
	hooks    *Hooks
	resolver atomic.Pointer[inject.Resolver] // /drop 清缓存（读）与 /reset 重建（写）并发时原子安全（2026-09-14 审计修）
}

// New 构建单实例应用。
func New(state *AppState, hooks *Hooks) *App {
	ap := &App{state: state, hooks: hooks}
	// 上下文注入（2026-09-11）：组装请求时把 inject_ref 解析成真实 parts
	// 载入回退（2026-09-13）：侧车载入失败 → 移除附件 + 报错文字存根回传 LLM
	ap.reconfigureResolver()
	return ap
}

// reconfigureResolver 建（或重建）inject 解析器并注入 context 包全局
func (ap *App) reconfigureResolver() {
	onFail := func(id, reason string) {
		ap.state.mu.Lock()
		kept := ap.state.Injects[:0]
		for _, x := range ap.state.Injects {
			if x.ID != id {
				kept = append(kept, x)
			}
		}
		ap.state.Injects = kept
		ap.state.mu.Unlock()
	}
	res := inject.NewResolver(filepath.Join(ap.state.Agent.Cfg().Data.Dir, "inject"), onFail)
	ap.resolver.Store(res)
	ctxmgr.ConfigureInjectResolver(res)
}

// ServeHTTP 路由（与 TS 端点全集一一对应）
func (ap *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/health":
		ap.handleHealth(w, r)
	case r.Method == http.MethodGet && path == "/status":
		ap.handleStatus(w, r)
	case r.Method == http.MethodGet && path == "/queue":
		ap.handleQueue(w, r)
	case r.Method == http.MethodGet && path == "/history":
		ap.handleHistory(w, r)
	case r.Method == http.MethodGet && path == "/backgrounds":
		ap.handleBackgrounds(w, r)
	case r.Method == http.MethodPost && path == "/chat":
		ap.handleChat(w, r)
	case r.Method == http.MethodPost && path == "/chat/stream":
		ap.handleChatStream(w, r)
	case r.Method == http.MethodPost && path == "/enqueue":
		ap.handleEnqueue(w, r)
	case r.Method == http.MethodGet && path == "/events":
		ap.handleEvents(w, r)
	case r.Method == http.MethodPost && path == "/inject":
		ap.handleInject(w, r)
	case r.Method == http.MethodPost && path == "/drop":
		ap.handleDrop(w, r)
	case r.Method == http.MethodGet && path == "/attachments":
		ap.handleAttachments(w, r)
	case r.Method == http.MethodPost && path == "/reset":
		ap.handleReset(w, r)
	case r.Method == http.MethodPost && path == "/stop":
		ap.handleStop(w, r)
	case r.Method == http.MethodGet && path == "/prompts":
		ap.handlePrompts(w, r)
	case r.Method == http.MethodGet && path == "/prompts/file":
		ap.handlePromptsFileGet(w, r)
	case r.Method == http.MethodPost && path == "/prompts/file":
		ap.handlePromptsFilePost(w, r)
	case r.Method == http.MethodPost && path == "/prompts/reload":
		ap.handlePromptsReload(w, r)
	case r.Method == http.MethodGet && path == "/config":
		ap.handleConfigGet(w, r)
	case r.Method == http.MethodPost && path == "/config":
		ap.handleConfigPost(w, r)
	case r.Method == http.MethodGet && path == "/favicon.ico":
		ap.handleFavicon(w, r)
	case r.Method == http.MethodGet && path == "/":
		ap.handleIndex(w, r)
	case r.Method == http.MethodGet && path == "/marked.min.js":
		ap.handleMarked(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ---------- 只读端点 ----------

func (ap *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	cfg := a.Cfg()
	sess := a.CM().Session()
	writeJSON(w, 200, map[string]interface{}{
		"ok":      true,
		"session": int(a.SessionNo.Load()),
		"context": ctxmgr.ContextStatsOf(sess, cfg.Context.CharsPerToken),
		"llm":     cfg.LLM.BaseURL,
	})
}

func (ap *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	ap.state.mu.Lock()
	running := a.Lock.Running() || ap.state.ActiveDrain != nil
	ap.state.mu.Unlock()
	cfg := a.Cfg()
	sess := a.CM().Session()
	a.logMu.Lock()
	compress := lastN(a.CompressLog, 20)
	doom := lastN(a.DoomLog, 10)
	a.logMu.Unlock()

	var summary interface{}
	if s := sess.Summary; s != nil {
		summary = map[string]interface{}{
			"chars":   utf16Len(s.Content),
			"preview": utf16Slice(s.Content, 120),
		}
	}

	// raw_tail：末 5 条消息（chars/preview 口径与 TS 一致）
	msgs := sess.Messages
	from := len(msgs) - 5
	if from < 0 {
		from = 0
	}
	rawTail := make([]map[string]interface{}, 0, len(msgs)-from)
	for i := from; i < len(msgs); i++ {
		e := &msgs[i]
		if s, ok := e.Content.(string); ok {
			rawTail = append(rawTail, map[string]interface{}{
				"role":    e.Role,
				"chars":   utf16Len(s),
				"preview": utf16Slice(s, 120),
			})
		} else {
			chars := 0
			var text strings.Builder
			for _, p := range types.AsParts(e.Content) {
				if p.Type == "text" {
					chars += utf16Len(p.Text)
					text.WriteString(p.Text)
				} else {
					chars += 1000
				}
			}
			rawTail = append(rawTail, map[string]interface{}{
				"role":    e.Role,
				"chars":   chars,
				"preview": utf16Slice(text.String(), 120),
			})
		}
	}

	bgs := make([]map[string]interface{}, 0)
	for _, t := range a.BG().Snapshot() {
		bgs = append(bgs, map[string]interface{}{
			"call_id":     t.CallID,
			"tool":        t.ToolName,
			"started_at":  t.StartedAt,
			"settled":     t.Settled,
			"ok":          t.OK,
			"duration_ms": t.DurationMs,
		})
	}

	a.promptMu.Lock()
	fallback := a.PromptFallback
	files := append([]PromptFileInfo(nil), a.PromptFiles...)
	spChars := utf16Len(a.SystemPrompt)
	a.promptMu.Unlock()

	writeJSON(w, 200, map[string]interface{}{
		"session": int(a.SessionNo.Load()),
		"running": running,
		"system_prompt": map[string]interface{}{
			"fallback": fallback,
			"files":    files,
			"chars":    spChars,
		},
		"context":     ctxmgr.ContextStatsOf(sess, cfg.Context.CharsPerToken),
		"compress":    compress,
		"doom":        doom,
		"backgrounds": bgs,
		"summary":     summary,
		"raw_tail":    rawTail,
	})
}

func (ap *App) handleQueue(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	ap.state.mu.Lock()
	queued := append([]string(nil), ap.state.MessageQueue...)
	running := a.Lock.Running() || ap.state.ActiveDrain != nil
	ap.state.mu.Unlock()
	// 快照取一次（2026-09-14 审计修：旧代码每次迭代都调 Session() 取活引用，
	// 两次取值可能不一致——压缩删消息时索引越界 panic）
	sess := a.CM().Session()
	userSeq := 0
	for i := range sess.Messages {
		if sess.Messages[i].Role == "user" {
			userSeq++
		}
	}
	writeJSON(w, 200, map[string]interface{}{"running": running, "user_seq": userSeq, "queued": queued})
}

func (ap *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	sess := a.CM().Session()
	entries := make([]map[string]interface{}, 0, len(sess.Messages))
	for i := range sess.Messages {
		e := &sess.Messages[i]
		m := map[string]interface{}{"role": e.Role, "content": e.Content}
		if e.ToolCallID != nil {
			m["tool_call_id"] = *e.ToolCallID
		}
		if len(e.ToolCalls) > 0 {
			m["tool_calls"] = e.ToolCalls
		}
		if e.ReasoningContent != nil {
			m["reasoning_content"] = *e.ReasoningContent
		}
		entries = append(entries, m)
	}
	// 2026-09-19：本轮正在跑时，当前轮的条目（run_start 之后持久化的）不进 /history——
	// 它们由 /events 重放整轮重建（重放含 run_msg/delta/tool_* 全部帧）。两边给同一条目
	// 会让前端重复渲染（history 画一份 + 重放流一份）。条件与 /events 重放完全一致。
	ap.state.mu.Lock()
	runningNow := a.Lock.Running() || ap.state.ActiveDrain != nil
	buf := ap.state.RoundBuf
	ap.state.mu.Unlock()
	if runningNow && buf != nil && buf.Round == int(a.SessionNo.Load()) && buf.StartLen < len(entries) {
		entries = entries[:buf.StartLen]
	}
	var summary interface{}
	if s := sess.Summary; s != nil {
		summary = s.Content
	}
	writeJSON(w, 200, map[string]interface{}{"session": int(a.SessionNo.Load()), "summary": summary, "entries": entries})
}

func (ap *App) handleBackgrounds(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	tasks := make([]map[string]interface{}, 0)
	for _, t := range a.BG().Snapshot() {
		tasks = append(tasks, map[string]interface{}{
			"call_id":      t.CallID,
			"tool":         t.ToolName,
			"started_at":   t.StartedAt,
			"settled":      t.Settled,
			"ok":           t.OK,
			"duration_ms":  t.DurationMs,
			"result_chars": utf16Len(t.Result),
		})
	}
	writeJSON(w, 200, map[string]interface{}{
		"running": a.Lock.Running(),
		"count":   a.BG().Count(),
		"tasks":   tasks,
	})
}

// ---------- /chat /chat/stream /enqueue ----------

func (ap *App) handleChat(w http.ResponseWriter, r *http.Request) {
	b := tryReadJSON(r)
	message, _ := b["message"].(string)
	if message == "" {
		writeJSON(w, 400, map[string]interface{}{"error": `body 需为 { "message": "..." }`})
		return
	}
	a := ap.state.Agent
	events := []types.AgentEvent{}
	onEvent := func(e types.AgentEvent) {
		events = append(events, e)
		logRunEvent(a, e)
		broadcastEvent(ap.state, e)
	}
	started := time.Now()
	// 入队 + 活动 drain（2026-08-31，同 /chat/stream）：运行中发的消息合并成一条一起发
	h, _ := enqueueAndWake(ap.state, a, message, onEvent)
	<-h.done
	if h.res.Err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": h.res.Err.Error(), "events": events, "ms": time.Since(started).Milliseconds()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"reply": h.res.Reply, "ms": h.res.MS, "events": events})
}

// sseConn SSE 连接写端（并发写加锁：事件来自 drain goroutine，心跳来自另一 goroutine）
type sseConn struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
	closed  bool
	done    chan struct{}
}

func newSSE(w http.ResponseWriter) *sseConn {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl, _ := w.(http.Flusher)
	return &sseConn{w: w, flusher: fl, done: make(chan struct{})}
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

func (c *sseConn) push(event string, data interface{}) {
	c.write(fmt.Sprintf("event: %s\ndata: %s\n\n", event, jsonStr(data)))
}

// heartbeat 5s 心跳保活（同 TS）：LLM 思考 / 工具执行期间可能长时间无事件，
// 防空闲超时机制（反向代理）掐断长连接。
func (c *sseConn) heartbeat() {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				c.write(": ping\n\n")
			case <-c.done:
				return
			}
		}
	}()
}

func (c *sseConn) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	close(c.done)
}

func (ap *App) handleChatStream(w http.ResponseWriter, r *http.Request) {
	b := tryReadJSON(r)
	message, _ := b["message"].(string)
	if message == "" {
		writeJSON(w, 400, map[string]interface{}{"error": `body 需为 { "message": "..." }`})
		return
	}
	a := ap.state.Agent
	conn := newSSE(w)
	conn.heartbeat()
	onEvent := func(e types.AgentEvent) {
		// doom/bg 事件记日志 + 广播到 /events（2026-08-31 方案A：刷新后新连接也能收全）
		logRunEvent(a, e)
		broadcastEvent(ap.state, e)
		// 本流也推全量事件（API 流式客户端兼容；WebUI 走 /enqueue + /events，不用本端点）
		conn.push(strOf(e["type"]), e)
	}
	h, queuedBefore := enqueueAndWake(ap.state, a, message, onEvent)
	conn.push("start", map[string]interface{}{"message": message, "queued_before": queuedBefore})
	<-h.done
	if h.res.Err != nil {
		conn.push("error", map[string]interface{}{"message": h.res.Err.Error()})
		// run 抛错 → 停止排空（队列保留，下次 /chat 或 /enqueue 触发时继续）
	} else {
		conn.push("finish", map[string]interface{}{"reply": h.res.Reply, "ms": h.res.MS})
	}
	conn.close()
}

// handleEnqueue 统一入队（2026-08-31）：走队列/drain 机制。
// queuedBefore/position 在 enqueueAndWake 的同一把锁内取出（2026-09-14 起
// 入队与取位原子化，不再有"先取位后入队"的时序要求）。
func (ap *App) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	b := tryReadJSON(r)
	message, _ := b["message"].(string)
	if message == "" {
		writeJSON(w, 400, map[string]interface{}{"error": `body 需为 { "message": "..." }`})
		return
	}
	a := ap.state.Agent
	// running 含活动 drain（多轮间隙锁瞬释放不算空闲）——入队前读
	ap.state.mu.Lock()
	running := a.Lock.Running() || ap.state.ActiveDrain != nil
	ap.state.mu.Unlock()
	h, queuedBefore := enqueueAndWake(ap.state, a, message, makeOnEvent(ap.state, a))
	position := queuedBefore + 1
	broadcastEvent(ap.state, types.AgentEvent{"type": "enqueued", "message": message, "position": position, "running": running})
	go func() { <-h.done }() // 防 unhandledRejection 等价：错误广播由 drain 统一处理
	writeJSON(w, 200, map[string]interface{}{"ok": true, "queued": queuedBefore, "position": position, "running": running})
}

// ---------- /events：常驻事件广播（2026-08-31 方案A） ----------
// 订阅本实例的全部 run 事件（run_start/delta/tool_*/done/…）。前端一个常驻
// EventSource，刷新/重开页面后续接——不再丢"刷新瞬间正在流式那轮"的显示。
func (ap *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	conn := newSSE(w)
	conn.heartbeat()
	subID := subSeq.Add(1)
	sub := func(payload string) { conn.write(payload) }
	ap.state.mu.Lock()
	// 订阅：每次广播回调里直接写本连接（broadcastEvent 逐个 recover 隔离）
	ap.state.BroadcastSubs = append(ap.state.BroadcastSubs, subEntry{ID: subID, Fn: sub})
	// 连接建立即报当前状态（前端据此对齐：running + 排队消息全文 + 后台任务数）。
	// queued 带全文：刷新/重开/重连后前端据此补显"排队中"气泡（与历史去重）。
	queued := append([]string(nil), ap.state.MessageQueue...)
	running := a.Lock.Running() || ap.state.ActiveDrain != nil
	session := int(a.SessionNo.Load())
	bgPending := a.BG().Count()
	var replay []string
	// 重放：仅当正在跑时，补发当前轮已流出的帧。空闲时不重放——已完成的
	// 轮次由前端 /history 加载，重放会重复出气泡。只重放 round=当前 session 的缓冲。
	if running && ap.state.RoundBuf != nil && ap.state.RoundBuf.Round == session {
		replay = append([]string(nil), ap.state.RoundBuf.Frames...)
	}
	ap.state.mu.Unlock()
	conn.write(fmt.Sprintf("event: hello\ndata: %s\n\n", jsonStr(map[string]interface{}{
		"session":    session,
		"running":    running,
		"queued":     queued,
		"bg_pending": bgPending,
	})))
	for _, f := range replay {
		conn.write(f)
	}
	// 连接挂着直到对端断开 → 清订阅、停心跳
	<-r.Context().Done()
	ap.state.mu.Lock()
	kept := ap.state.BroadcastSubs[:0]
	for _, s := range ap.state.BroadcastSubs {
		if s.ID != subID {
			kept = append(kept, s)
		}
	}
	ap.state.BroadcastSubs = kept
	ap.state.mu.Unlock()
	conn.close()
}

// ---------- 上下文注入（2026-09-11，厄运定：内核只给接口，识别/格式是插件的事） ----------

// INJECT_MAX 预算软上限（2026-09-12 修复）：活跃附件不再强制清（清最旧会断
// KV 前缀匹配）；侧车只在"会话引用已消失（被压缩吃掉）"时清；超此数只警告。
const INJECT_MAX = 8

// validateContentParts OpenAI parts 校验（非空数组；每 part 须为带非空 type 的 JSON 对象——
// 透传管道，part 类型语义属于 LLM 端点，主程序不逐类型识别）
func validateContentParts(v interface{}) []types.ContentPart {
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]types.ContentPart, 0, len(arr))
	for _, p := range arr {
		o, ok := p.(map[string]interface{})
		if !ok {
			return nil
		}
		part, err := types.FromPartMap(o)
		if err != nil {
			return nil
		}
		out = append(out, part)
	}
	return out
}

// sessionHasInjectRef inject_ref 是否仍在会话里（2026-09-12）：在 = 侧车必须保留
// （KV 前缀稳定）；不在 = 已被压缩吃掉，清掉安全（无匹配断）。
func sessionHasInjectRef(cm *ctxmgr.Manager, id string) bool {
	// 快照取一次（2026-09-14 审计修：旧代码每行调两次 Session() 取活引用，
	// 两次不一致时索引越界 panic）
	msgs := cm.Session().Messages
	for i := range msgs {
		for _, p := range types.AsParts(msgs[i].Content) {
			if p.Type == "inject_ref" && p.Ref == id {
				return true
			}
		}
	}
	return false
}

func (ap *App) handleInject(w http.ResponseWriter, r *http.Request) {
	b := tryReadJSON(r)
	var parts []types.ContentPart
	if b != nil {
		parts = validateContentParts(b["content"])
	}
	if parts == nil {
		writeJSON(w, 400, map[string]interface{}{"error": "content 须为非空 content parts 数组（OpenAI 格式：每个 part 为带非空 type 字段的 JSON 对象，如 {type:text,text} / {type:image_url,image_url:{url}} / {type:input_video,input_video:{data}}）"})
		return
	}
	a := ap.state.Agent
	dir := filepath.Join(a.Cfg().Data.Dir, "inject")
	// 先无锁扫描（2026-09-14 审计修：旧实现在 state.mu 内调 sessionHasInjectRef
	// （取 cm.mu）→ 锁序 state.mu→cm.mu；而 compose 侧 resolve 失败回调是
	// cm.mu→state.mu → 反向锁序，并发 /inject 与在飞 run 可死锁。现在扫描
	// 全程不持 state.mu，两个方向都断）
	ap.state.mu.Lock()
	pending := append([]InjectInfo(nil), ap.state.Injects...)
	ap.state.mu.Unlock()
	kept := make([]InjectInfo, 0, len(pending))
	for _, x := range pending {
		if sessionHasInjectRef(a.CM(), x.ID) {
			kept = append(kept, x)
		} else {
			// 预算修复（2026-09-12 破案）：只清"会话里已无引用"的侧车（被压缩吃掉的）
			inject.DropInjectFile(dir, x.ID)
		}
	}
	ap.state.mu.Lock()
	ap.state.InjectSeq++
	id := fmt.Sprintf("inj-%d", ap.state.InjectSeq)
	ap.state.Injects = kept
	overLimit := len(kept) > INJECT_MAX
	n := len(kept)
	ap.state.mu.Unlock()
	if overLimit {
		fmt.Fprintf(os.Stderr, "[inject] 活跃附件 %d 超软上限 %d（引用仍在会话，保留不清）\n", n, INJECT_MAX)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": "inject 目录创建失败：" + err.Error()})
		return
	}
	payload, _ := json.Marshal(parts)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), payload, 0o644); err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": "侧车写入失败：" + err.Error()})
		return
	}
	var notePtr *string
	if b != nil {
		if note, ok := b["note"].(string); ok {
			notePtr = &note
		}
	}
	ap.state.mu.Lock()
	ap.state.Injects = append(ap.state.Injects, InjectInfo{ID: id, Bytes: len(payload), CreatedAt: time.Now().UnixMilli(), Note: notePtr})
	ap.state.mu.Unlock()
	label := "附件 " + id
	if notePtr != nil && *notePtr != "" {
		label = *notePtr
	}
	a.CM().AddMessage(&types.RawEntry{
		Role: "user",
		Content: []types.ContentPart{
			types.TextPart("[okattach] " + label),
			types.RefPart(id),
		},
	})
	writeJSON(w, 200, map[string]interface{}{"ok": true, "id": id})
}

var injIDRe = regexp.MustCompile(`^inj-\d+$`)

func (ap *App) handleDrop(w http.ResponseWriter, r *http.Request) {
	b := tryReadJSON(r)
	id, _ := b["id"].(string)
	if id == "" || !injIDRe.MatchString(id) {
		writeJSON(w, 400, map[string]interface{}{"error": `body 须为 {"id":"inj-N"}`})
		return
	}
	a := ap.state.Agent
	ok := inject.DropInjectFile(filepath.Join(a.Cfg().Data.Dir, "inject"), id)
	// 清解析器缓存（2026-09-14 审计修：旧实现只删侧车文件不清缓存——已 resolve
	// 过的附件 /drop 后下一轮 compose 仍从缓存取到真实 parts，"已删附件"持续
	// 注入直到进程重启）
	if res := ap.resolver.Load(); res != nil {
		res.ClearCache(id)
	}
	ap.state.mu.Lock()
	kept := ap.state.Injects[:0]
	for _, x := range ap.state.Injects {
		if x.ID != id {
			kept = append(kept, x)
		}
	}
	ap.state.Injects = kept
	ap.state.mu.Unlock()
	writeJSON(w, 200, map[string]interface{}{"ok": ok})
}

func (ap *App) handleAttachments(w http.ResponseWriter, r *http.Request) {
	ap.state.mu.Lock()
	injects := append([]InjectInfo(nil), ap.state.Injects...)
	ap.state.mu.Unlock()
	writeJSON(w, 200, map[string]interface{}{"injects": injects})
}

// ---------- /reset：新会话 ----------

func (ap *App) handleReset(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	// 审计 H1 修（2026-09-14）：旧代码不持运行锁直接换 CM/Agent/BG——run 在飞时
	// 会留一个 zombie run 继续在旧 CM 上跑完，其落盘闭包读到已 +1 的 SessionNo →
	// 旧会话内容以新会话号写进 session.json（污染）。现在：先 Stop() 中断在飞
	// LLM 流（partial 保留、本轮立即收尾），重建整体包进 Lock.Run——与在飞 run
	// 互斥，等当前轮完全结束后才原子换运行时（在飞的是长工具时最长等其结束）。
	a.Agent().Stop()
	a.Lock.Run(func() error {
		a.SessionNo.Add(1)
		ap.state.mu.Lock()
		ap.state.MessageQueue = nil // 未发消息随会话清除（2026-08-31）
		ap.state.Injects = nil      // 上下文注入附件随会话清除（2026-09-11）
		ap.state.InjectSeq = 0
		ap.state.RoundBuf = nil // 轮次事件缓冲随会话清除（重放不再带旧轮）
		ap.state.RunSeq = 0     // 会话内轮次计数随会话清除（新会话从 1 重新计）
		ap.state.mu.Unlock()
		a.logMu.Lock()
		a.CompressLog = nil
		a.DoomLog = nil
		a.logMu.Unlock()
		a.BG().Clear() // 未完成后台任务随会话清除（旧编排器随后整体废弃）
		if res := ap.resolver.Load(); res != nil {
			res.ClearCache("")
		}
		_ = os.RemoveAll(filepath.Join(a.Cfg().Data.Dir, "inject"))
		ap.reconfigureResolver()
		// 重建三件套并整体原子换入（2026-09-14 审计修：/inject 等不持运行锁的
		// 路径经 CM()/Agent()/BG() 访问器取当前运行时，换入后自动指向新会话）
		a.llmMu.Lock()
		client := a.LLM
		a.llmMu.Unlock()
		a.promptMu.Lock()
		sp := a.SystemPrompt
		a.promptMu.Unlock()
		newCM := makeCm(a)
		a.rt.Store(&agentRuntime{CM: newCM, Agent: agent.New(client, sp, newCM, filepath.Join(a.Cfg().Data.Dir, "inject")), BG: background.NewOrchestrator()})
		if ap.hooks != nil && ap.hooks.OnSessionRebuilt != nil {
			ap.hooks.OnSessionRebuilt(a)
		}
		return nil
	})
	writeJSON(w, 200, map[string]interface{}{"ok": true, "session": int(a.SessionNo.Load())})
}

// ---------- /stop：中断当前 LLM 输出（2026-08-29） ----------
// 语义：cancel 当前 LLM 请求（HTTP 断开 → llama.cpp 检测客户端断连取消生成）；
// 若正处工具执行中，本轮跑完工具后在下次 LLM 调用前强停。已生成的增量内容
// 保留进会话（partial 收尾），不会丢。
func (ap *App) handleStop(w http.ResponseWriter, r *http.Request) {
	ap.state.Agent.Agent().Stop()
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// ---------- 提示词（WebUI 提示词页，2026-08-29） ----------

func (ap *App) handlePrompts(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	a.promptMu.Lock()
	var dir interface{}
	if a.PromptDir != "" {
		dir = a.PromptDir
	}
	fallback := a.PromptFallback
	files := append([]PromptFileInfo(nil), a.PromptFiles...)
	sp := a.SystemPrompt
	a.promptMu.Unlock()
	writeJSON(w, 200, map[string]interface{}{
		"dir":      dir,
		"fallback": fallback,
		"files":    files,
		"chars":    utf16Len(sp),
		"prompt":   sp,
	})
}

func (ap *App) handlePromptsFileGet(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	name := r.URL.Query().Get("name")
	if a.PromptDir == "" || !safeName(name) {
		writeJSON(w, 400, map[string]interface{}{"error": "无提示词目录或文件名非法"})
		return
	}
	s, err := os.ReadFile(filepath.Join(a.PromptDir, name))
	if err != nil {
		writeJSON(w, 404, map[string]interface{}{"error": "文件不存在"})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"name": name, "content": string(s)})
}

func (ap *App) handlePromptsFilePost(w http.ResponseWriter, r *http.Request) {
	b := tryReadJSON(r)
	a := ap.state.Agent
	if a.PromptDir == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "无提示词目录（内置默认提示词，请用配置页 system_prompt.dir 建目录后重启）"})
		return
	}
	name, ok := b["name"].(string)
	if !ok || !safeName(name) {
		writeJSON(w, 400, map[string]interface{}{"error": "文件名非法（仅字母/数字/_/-/.，不能以 . 开头）"})
		return
	}
	content, ok := b["content"].(string)
	if !ok {
		writeJSON(w, 400, map[string]interface{}{"error": `需 { "name": "xx.md", "content": "..." }`})
		return
	}
	file := filepath.Join(a.PromptDir, name)
	if strings.HasSuffix(strings.ToLower(name), ".md") {
		// 新增 .md → 立即重载生效
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": "写入失败：" + err.Error()})
			return
		}
		reloadAgentPrompt(a)
		a.promptMu.Lock() // 2026-09-14 审计修：PromptFiles 读取与并发重载互斥
		names := make([]string, 0, len(a.PromptFiles))
		for _, f := range a.PromptFiles {
			names = append(names, f.Name)
		}
		a.promptMu.Unlock()
		writeJSON(w, 200, map[string]interface{}{"ok": true, "name": name, "reloaded": true, "files": names})
		return
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": "写入失败：" + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "name": name, "reloaded": false})
}

func (ap *App) handlePromptsReload(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	rl := reloadAgentPrompt(a)
	names := make([]string, 0, len(rl.Files))
	for _, f := range rl.Files {
		names = append(names, f.Name)
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "fallback": rl.Fallback, "files": names, "chars": utf16Len(rl.Prompt)})
}

// ---------- 配置（WebUI 配置页，2026-08-29，2026-09-09 单实例化） ----------

func (ap *App) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := ap.state.Agent.Cfg() // COW 读：不可变对象，直接序列化
	buf, _ := json.Marshal(cfg)
	var global map[string]interface{}
	_ = json.Unmarshal(buf, &global)
	restart := make([]string, 0, len(restartSections))
	for _, s := range restartSections {
		restart = append(restart, fmt.Sprintf("%s（改后需重启进程）", s))
	}
	writeJSON(w, 200, map[string]interface{}{
		"global":       mergeMaps(global, map[string]interface{}{"restart_required": restart}),
		"hot_sections": hotSections,
		"effective":    cfg,
		"llm_client":   llmCfgOf(cfg.LLM),
	})
}

// numChecks 数值段基础校验（与 TS 表一致）
var numChecks = map[string]func(interface{}) bool{
	"port":              func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 && n < 65536 },
	"timeout_ms":        func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 },
	"tick_ms":           func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 },
	"catchup_max":       func(v interface{}) bool { n, ok := v.(float64); return ok && n >= 0 },
	"keep_recent_chars": func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 },
	"hard_trunc_chars":  func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 },
	"stream_idle_ms":    func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 },
	"max_tokens":        func(v interface{}) bool { n, ok := v.(float64); return ok && n > 100 },
	// chars_per_token：0/负数 → 估算 Infinity/恒超阈（每条消息白压一次）；过大 →
	// 严重低估 → 永不压缩 → 撑爆模型上下文。英文 ~4 字符/token，20 已极宽松。
	"chars_per_token": func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 && n <= 20 },
	"result_limit":    func(v interface{}) bool { n, ok := v.(float64); return ok && n > 1000 },
	"fg_timeout_ms":   func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 },
	"warn_after":      func(v interface{}) bool { n, ok := v.(float64); return ok && n >= 2 },
}

func (ap *App) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	b := tryReadJSON(r)
	var patch map[string]interface{}
	if b != nil {
		patch, _ = b["patch"].(map[string]interface{})
	}
	if patch == nil || len(patch) == 0 {
		writeJSON(w, 400, map[string]interface{}{"error": `body 需为 { "patch": { "llm": {...}, ... } }`})
		return
	}
	// 段校验：只接受已知段
	known := map[string]bool{}
	for _, s := range append(append([]string{}, hotSections...), restartSections...) {
		known[s] = true
	}
	for k := range patch {
		if !known[k] {
			writeJSON(w, 400, map[string]interface{}{"error": fmt.Sprintf("未知配置段：%s（允许：llm / context / tools / doom / server / data / system_prompt）", k)})
			return
		}
	}
	// 数值段基础校验
	for section, secVal := range patch {
		sec, ok := secVal.(map[string]interface{})
		if !ok {
			writeJSON(w, 400, map[string]interface{}{"error": fmt.Sprintf("段 %s 需为 JSON 对象", section)})
			return
		}
		for k, v := range sec {
			if chk, ok := numChecks[k]; ok && !chk(v) {
				vb, _ := json.Marshal(v)
				writeJSON(w, 400, map[string]interface{}{"error": fmt.Sprintf("%s.%s 数值非法：%s", section, k, string(vb))})
				return
			}
			if section == "llm" && k == "base_url" {
				s, ok2 := v.(string)
				if !ok2 || !strings.HasPrefix(s, "http") {
					writeJSON(w, 400, map[string]interface{}{"error": "llm.base_url 需为 http(s) 地址"})
					return
				}
			}
			if section == "llm" && (k == "model" || k == "api_key") {
				if _, ok2 := v.(string); !ok2 {
					writeJSON(w, 400, map[string]interface{}{"error": fmt.Sprintf("llm.%s 需为字符串", k)})
					return
				}
			}
			// compress_thinking 已删（2026-09-08）：从补丁里剔除（不落盘不生效）——
			// 比 400 更宽容，旧客户端/旧 webui 发此键不报错；sec 与 patch[section]
			// 同一引用，delete 后深合并也不会带上它
			if section == "context" && k == "compress_thinking" {
				delete(sec, k)
				continue
			}
		}
	}
	// 应用（单实例：全局 = 本实例）
	rl := applyConfigPatch(ap.state.Agent, patch)
	if ap.hooks != nil && ap.hooks.OnConfigSaved != nil {
		ap.hooks.OnConfigSaved(patch)
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "scope": "global", "applied": rl.Applied, "restart": rl.Restart})
}

// ---------- WebUI 静态页面（2026-08-29） ----------

// favicon（浏览器自动请求；无文件时返回内联 SVG，消除 404 噪音）
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><circle cx="50" cy="50" r="46" fill="#0e0e12"/><circle cx="38" cy="44" r="9" fill="#e05252"/><circle cx="64" cy="58" r="7" fill="#4a9eff"/><text x="50" y="88" font-size="28" text-anchor="middle" fill="#ddd">Ok</text></svg>`

func (ap *App) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, faviconSVG)
}

func (ap *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(filepath.Join(ap.state.Root, "webui", "index.html"))
	if err != nil {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, "OkHuman WebUI 未部署（缺 webui/index.html）")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

// marked.min.js：markdown 渲染库（2026-09-09）。本地化随 webui/ 部署，不依赖 CDN；
// 文件缺失时返回 404（前端 md() 会回退纯文本，页面不崩）。
func (ap *App) handleMarked(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(filepath.Join(ap.state.Root, "webui", "marked.min.js"))
	if err != nil {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, "marked.min.js 未部署")
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

// ---------- 工具函数 ----------

// safeName 提示词文件名合法性（防穿越）
var safeNameRe = regexp.MustCompile(`^[\w.\-]+$`)

func safeName(name string) bool {
	return safeNameRe.MatchString(name) && !strings.HasPrefix(name, ".") && !strings.Contains(name, "..")
}

// utf16Len UTF-16 code units 长度（对齐 TS text.length，全项目字符口径统一）
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16Slice 按 UTF-16 units 截断（对齐 TS slice(0, n)）
func utf16Slice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		c := 1
		if r >= 0x10000 {
			c = 2
		}
		if count+c > n {
			break
		}
		b.WriteRune(r)
		count += c
	}
	return b.String()
}

func strOf(v interface{}) string {
	s, _ := v.(string)
	return s
}

func jsonStr(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// maxBodyBytes 请求体上限（2026-09-14 审计修：旧实现无上限，恶意/失控客户端
// 可灌 GB 级 JSON 撑爆内存；256MB 已远大于任何正常请求——/inject 的大图 base64
// 也在其内）
const maxBodyBytes = 256 << 20

// tryReadJSON 读 JSON body（失败 → nil，与 TS c.req.json().catch(() => null) 等价）
func tryReadJSON(r *http.Request) map[string]interface{} {
	var m map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&m); err != nil {
		return nil
	}
	return m
}

// lastN 取末尾 n 条（空 → 空数组，非 nil，JSON 输出 [] 而非 null）
func lastN[T any](xs []T, n int) []T {
	if len(xs) <= n {
		out := make([]T, 0, len(xs))
		return append(out, xs...)
	}
	out := make([]T, 0, n)
	return append(out, xs[len(xs)-n:]...)
}

// mergeMaps 浅合并（b 覆盖 a）
func mergeMaps(a, b map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
