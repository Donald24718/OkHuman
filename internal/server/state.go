package server

import (
	"os"
	"path/filepath"
	"strconv"
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
	"okhuman/internal/types"
)

// CM 当前上下文管理器（/reset 后指向新会话）
func (a *AgentState) CM() *ctxmgr.Manager { return a.rt.Load().CM }

// Agent 当前 agent 运行时（/reset 后指向新实例）

// Agent 当前 agent 运行时（/reset 后指向新实例）
func (a *AgentState) Agent() *agent.Agent { return a.rt.Load().Agent }

// BG 当前后台编排器（/reset 后指向新实例）

// BG 当前后台编排器（/reset 后指向新实例）
func (a *AgentState) BG() *background.BackgroundOrchestrator { return a.rt.Load().BG }

// roundBuf 轮次事件缓冲（重放用）

// roundBuf 轮次事件缓冲（重放用）
type roundBuf struct {
	Round  int
	Frames []string
	// StartLen run_start 时刻会话消息数——当前轮的条目从下标 StartLen 起
	// （run_start 广播在 AddMessage(user) 之前，见 agent.run）。
	// 刷新时 /history 截断到 StartLen、/events 重放整轮 → 不重复不丢失。
	StartLen int
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
	a.rt.Store(&agentRuntime{CM: cm, Agent: agent.New(client, systemPrompt, cm), BG: background.NewOrchestrator()})
	return a
}

// ---------- 状态结构（对应 TS AgentState / AppState） ----------

// PromptFileInfo 提示词文件元信息（/status /prompts 返回，json 字段与 TS 一致）
type PromptFileInfo struct {
	Name  string `json:"name"`
	Chars int    `json:"chars"`
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

// AppState 单实例服务状态
type AppState struct {
	Root      string // 项目根（webui 静态文件 / config/user.json 落盘用）
	Agent     *AgentState
	mu        sync.Mutex               // 保护下面全部字段
	Evt       *eventLog                // 事件批次缓冲（发车机制内核，见 eventlog.go）
	reqMu     sync.Mutex               // 保护 reqCounts
	reqCounts map[string]*noisyCounter // 高频轮询端点计数（访问日志汇总用，不逐条记）
	// MessageQueue 待消费消息（2026-10-25 起结构化）：语义在 Kind 上，
	// 不再靠正文前缀猜"这是用户消息还是系统通知"。对外 JSON 仍投影成
	// 字符串数组（见 queuedTexts），协议字节级不变。
	MessageQueue []types.QueuedMessage
	ActiveDrain  *drainHandle
	RoundBuf     *roundBuf
	RunSeq       int
	// LastRunEnd 最近一次 run 结束的时刻（Unix 毫秒）——空闲心跳的计时基准
	// （2026-10-17）。口径由用户定：**只有 run 结束才重置**，正在 run 期间不计时。
	// 用 atomic 而非 state.mu：心跳只读它，避免与 RunLock→state.mu 锁序纠缠。
	LastRunEnd atomic.Int64
	InjectSeq  int
	Injects    []InjectInfo
	// injectInFlight 标记"已分配 ID、还没写进会话"的注入。清扫判据是"会话里已无引用"，
	// 对正在飞的那条必然为真——并发 /inject 会互相把它扫掉（2026-11-13 第四轮 A2：
	// 12 路并发实测一条不剩）。字段由 state.mu 保护。
	injectInFlight map[string]bool

	// Log 落盘日志回调（main 注入 persist.Log）。2026-10-18 健壮性修 D：server 层
	// 此前没有 logger，关键操作（手动剥离附件、重置会话落盘、附件载入失败）只能
	// fmt.Printf → 只进 stdout，okhuman.log 里查不到做过什么，审计断链。
	// nil 时静默（测试路径不注入）。
	Log func(string)
}

// logf 落盘日志（Log 未注入时静默，不 panic）

// Cfg 当前配置（COW 读：拿到的是不可变对象，无需再锁）
func (a *AgentState) Cfg() *config.Config { return a.CfgRef.Load() }

// CM 当前上下文管理器（/reset 后指向新会话）

// CompressLogEntry 压缩日志条目（/status compress 字段）
type CompressLogEntry struct {
	At        int64  `json:"at"`
	Scope     string `json:"scope"`
	Detail    string `json:"detail"`
	Before    int    `json:"before"`
	After     int    `json:"after"`
	Truncated bool   `json:"truncated"` // 摘要正文被 summary_max_chars 硬截断
	Window    int    `json:"window"`    // 当时生效的 context.keep_summaries
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

// NewAppState AppState 工厂（2026-08-31）：统一初始化事件广播集 + 消息队列 + 活动 drain + 轮次缓冲。
// cfg 参数保留兼容（配置统一存于 AgentState.CfgRef）。
func NewAppState(cfg *config.Config, root string, agent *AgentState) *AppState {
	st := &AppState{
		Root:         root,
		Agent:        agent,
		Evt:          newEventLog(),
		reqCounts:    map[string]*noisyCounter{},
		MessageQueue: []types.QueuedMessage{},
		Injects:      []InjectInfo{},
	}
	// 启动即视为"刚 run 完"：空闲计时从此刻开始，不会一启动就发心跳
	st.LastRunEnd.Store(time.Now().UnixMilli())
	// 重启续接修（2026-10-19）：侧车活在磁盘上，InjectSeq 活在内存里。重启后
	// InjectSeq 归零，而恢复的旧会话里 inject_ref 还指着 inj-1、inj-2…
	// → 新注入拿到同一个 id，AtomicWriteFile 直接盖掉旧侧车：旧消息里
	// "昨天那张图"悄悄变成今天新注入的内容，全程无报错（已用测试复现）。
	// 修法：启动时把 InjectSeq 抬到磁盘上已有的最大序号（/reset 会连目录一起
	// RemoveAll 并归零，两条路径不冲突）。
	dir := injectDir(cfg.Data.Dir)
	st.InjectSeq = maxInjectSeq(dir)
	// 顺带修可见性：会话里仍被引用的旧附件回填附件列表。此前重启后列表是空的，
	// 但 compose 每轮照样把它们发出去——侧栏看不见，用户也就删不掉/剥不掉。
	if agent != nil && agent.CM() != nil {
		seen := map[string]bool{}
		for _, m := range agent.CM().Session().Messages {
			for _, pt := range types.AsParts(m.Content) {
				if pt.Type != "inject_ref" || pt.Ref == "" || seen[pt.Ref] || !injIDRe.MatchString(pt.Ref) {
					continue
				}
				seen[pt.Ref] = true
				var created int64
				if fi, err := os.Stat(filepath.Join(dir, pt.Ref+".json")); err == nil {
					created = fi.ModTime().UnixMilli()
				}
				st.Injects = append(st.Injects, InjectInfo{ID: pt.Ref, Bytes: inject.InjectFileSize(dir, pt.Ref), CreatedAt: created})
			}
		}
	}
	return st
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

// drainHandle 活动 drain 句柄（enqueueAndWake 返回；done 关闭即本轮结束））
type drainHandle struct {
	done chan struct{}
}

// llmCfgOf config.LLMCfg → llm.ClientConfig
func llmCfgOf(c config.LLMCfg) llm.ClientConfig {
	return llm.ClientConfig{BaseURL: c.BaseURL, Model: c.Model, APIKey: c.APIKey,
		TimeoutMS: c.TimeoutSec * 1000, StreamIdleMS: c.StreamIdleSec * 1000,
		MaxResponseBytes: c.MaxResponseBytes}
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
		MaxTokens:        cfg.Context.MaxTokens,
		KeepRecentChars:  cfg.Context.KeepRecentChars,
		HardTruncChars:   cfg.Context.HardTruncChars,
		StreamIdleMS:     cfg.Context.StreamIdleSec * 1000, // 配置以秒为单位，内部按毫秒计时
		AttachmentTokens: cfg.Context.AttachmentTokens,
		CharsPerToken:    cfg.Context.CharsPerToken,
		KeepSummaries:    cfg.Context.KeepSummaries,
		SummaryMaxChars:  cfg.Context.SummaryMaxChars,
	}, sp, cfg.Data.Dir)
	m.SetSessionNo(int(a.SessionNo.Load()))
	m.AddCompressListener(func(e ctxmgr.CompressEvent) { pushCompressLog(a, e) })
	// 自我生命感知（2026-09-20）：本实例端口/pid/源码位置 + LLM 端口/pid，compose 时
	// 实时注入系统提示词首
	m.SetSelfInfo(ctxmgr.SelfInfo{Port: cfg.Server.Port, DataDir: cfg.Data.Dir, LLMBaseURL: cfg.LLM.BaseURL})
	return m
}

// maxInjectSeq 返回 inject 目录里 inj-<n>.json 的最大 n（无文件/无匹配 → 0）。
func maxInjectSeq(dir string) int {
	files, _ := filepath.Glob(filepath.Join(dir, "inj-*.json"))
	max := 0
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		if !injIDRe.MatchString(id) {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(id, "inj-")); err == nil && n > max {
			max = n
		}
	}
	return max
}

func pushCompressLog(a *AgentState, e ctxmgr.CompressEvent) {
	a.logMu.Lock()
	a.CompressLog = append(a.CompressLog, CompressLogEntry{
		At:        time.Now().UnixMilli(),
		Scope:     e.Scope,
		Detail:    e.Detail,
		Before:    e.BeforeChars,
		After:     e.AfterChars,
		Truncated: e.Truncated,
		Window:    e.Window,
	})
	a.logMu.Unlock()
}

// ---------- run 入口（对应 TS runOpt / drainLoop / startDrain；Go 侧 2026-09-14 起为 runBatch / runDrain / enqueueAndWake） ----------

// runOpt 本实例的 run 选项（用户消息 / 插件经 POST /enqueue 投递共用；每轮现取最新配置）

// AppState 单实例服务状态
