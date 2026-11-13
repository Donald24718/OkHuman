// Package server HTTP 服务（移植自 TS src/server.ts）：
// 端点全集与 TS 逐字段同形（插件 qq-channel/cron 依赖的协议不变）：
//
//	GET  /health /status /queue /history /attachments
//	GET  /events (SSE 常驻广播) /prompts /prompts/file /config
//	GET  / /favicon.ico /marked.min.js (WebUI 静态)
//	POST /enqueue /inject /reset /stop
//	POST /prompts/file /prompts/reload /config
//
// 单实例模型（§11）：一个进程 = 一个 agent；事件广播集 + 消息队列 +
// 活动 drain + 轮次缓冲集中在 AppState，上行入口（/enqueue /inject）
// 共享同一 drain（splice 队列不并发）。
//
// 文件划分（2026-11-12 拆分，纯搬动、不改逻辑）：本文件只留路由与安全边界；
// state.go = 状态类型与装配；runq.go = 队列/跑轮/心跳；configpatch.go = 配置补丁
// 与提示词热重载；sse.go = SSE 连接与事件帧；handlers_*.go = 各类端点；
// httputil.go = JSON/名称校验等小工具。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"okhuman/internal/agent"
	"okhuman/internal/background"
	"okhuman/internal/config"
	ctxmgr "okhuman/internal/context"
	"okhuman/internal/inject"
)

// noisyCounter 单个高频端点的计数与上次汇总时刻。
type noisyCounter struct {
	count    int64     // 累计次数
	loggedAt int64     // 上次打汇总时的 count
	lastLog  time.Time // 上次打汇总的时刻
}

// writeOriginOK 写请求的来源校验：0 表示放行，否则返回状态码与理由。
//
// 为什么要（2026-10-19）：API 没有任何鉴权，监听 127.0.0.1 时唯一的屏障是浏览器
// 同源策略——而那道屏障对"简单请求"不设防：POST + Content-Type: text/plain 不触发
// preflight，任何网页上的 JS 都能把 /enqueue、/config、/reset、/inject 打到
// http://127.0.0.1:8451，服务器照样执行（响应读不到，副作用已经发生）。本机 agent
// 有 bash 能力，于是"主人打开了一个恶意网页"可以直接变成"实例被人下指令"。
// DNS rebinding 还能把 Host 变成 evil.example 指到本机。
//
// 规则：Host 必须是本机地址（挡住 rebinding）；带 Origin 的请求必须来自本机同源
// （挡住跨站页面）。curl / 脚本 / 本机插件不带 Origin，照常放行——不改变任何
// 现有本机调用方的用法。

func (noFlusher) Flush() {}

func (ap *App) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, faviconSVG)
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
	recordPath, recordedMsgs, recordErr := "", 0, error(nil)
	injErr := error(nil)
	a.Lock.Run(func() error {
		// 重置前先把旧会话全文落盘（2026-10-18 用户定）：下面整体换掉 Manager，
		// 旧会话原文除压缩存档外就无处可寻了。放在换入之前 → 文件名带的还是旧会话号。
		recordPath, recordedMsgs, recordErr = a.CM().DumpSessionRecord("重置会话")
		// 收件目录随会话清空。删除失败必须看得见，而且这时**不能**把 InjectSeq 归零：
		// 新附件会从 inj-1 重新编号，盖住残留的旧侧车（与当年"重启后附件编号复用"同形态）。
		// 2026-11-13 四方审计 #16。
		injErr = os.RemoveAll(ap.injectDirOf())
		a.SessionNo.Add(1)
		ap.state.mu.Lock()
		ap.state.MessageQueue = nil // 未发消息随会话清除（2026-08-31）
		ap.state.Injects = nil      // 上下文注入附件随会话清除（2026-09-11）
		if injErr == nil {
			ap.state.InjectSeq = 0 // 只有收件目录真被清空才重新从 1 编号
		}
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
		a.rt.Store(&agentRuntime{CM: newCM, Agent: agent.New(client, sp, newCM), BG: background.NewOrchestrator()})
		if ap.hooks != nil && ap.hooks.OnSessionRebuilt != nil {
			ap.hooks.OnSessionRebuilt(a)
		}
		return nil
	})
	if injErr != nil {
		ap.state.logf("[reset] 收件目录清空失败：%v（附件计数保持递增，避免新附件盖住残留侧车）", injErr)
	}
	if recordPath != "" {
		ap.state.logf("[reset] 旧会话全文已落盘：%s（%d 条消息）", recordPath, recordedMsgs)
	} else if recordErr != nil {
		// 那是旧会话除压缩存档外唯一的全文存档，写失败必须留痕（2026-11-13 复审）
		ap.state.logf("[reset] 旧会话全文存档**失败**（%d 条消息未落盘）：%v", recordedMsgs, recordErr)
	}
	writeJSON(w, 200, map[string]interface{}{
		"ok":          true,
		"session":     int(a.SessionNo.Load()),
		"record":      recordPath, // 旧会话全文文件（空会话为 ""；写失败见 record_error）
		"record_msgs": recordedMsgs,
		"record_error": func() string { // 旧会话全文存档写失败必须看得见（2026-11-13）
			if recordErr == nil {
				return ""
			}
			return recordErr.Error()
		}(),
		"inject_error": func() string { // 收件目录没清干净也必须看得见
			if injErr == nil {
				return ""
			}
			return injErr.Error()
		}(),
	})
}

type Hooks struct {
	OnSessionRebuilt func(a *AgentState)
	OnConfigSaved    func(patch map[string]interface{}) error // 返回 error = 内存已生效但落盘失败
}

// App HTTP 应用（实现 http.Handler）

// ---------- WebUI 静态页面（2026-08-29） ----------

// favicon（浏览器自动请求；无文件时返回内联 SVG，消除 404 噪音）
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><circle cx="50" cy="50" r="46" fill="#0e0e12"/><circle cx="38" cy="44" r="9" fill="#e05252"/><circle cx="64" cy="58" r="7" fill="#4a9eff"/><text x="50" y="88" font-size="28" text-anchor="middle" fill="#ddd">Ok</text></svg>`

// ---------- 上下文注入（2026-09-11，厄运定：内核只给接口，识别/格式是插件的事） ----------

// INJECT_MAX 预算软上限（2026-09-12 修复）：活跃附件不再强制清（清最旧会断
// KV 前缀匹配）；侧车只在"会话引用已消失（被压缩吃掉）"时清；超此数只警告。
const INJECT_MAX = 8

// ---------- 只读端点 ----------

func (ap *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	cfg := a.Cfg()
	sess := a.CM().Session()
	writeJSON(w, 200, map[string]interface{}{
		"ok":      true,
		"session": int(a.SessionNo.Load()),
		"context": ctxmgr.ContextStatsOf(sess, cfg.Context.CharsPerToken, cfg.Context.AttachmentTokens, cfg.Context.KeepSummaries, cfg.Context.SummaryMaxChars),
		"llm":     cfg.LLM.BaseURL,
	})
}

// ---------- 工具函数 ----------

// safeName 提示词文件名合法性（防穿越）
var safeNameRe = regexp.MustCompile(`^[\w.\-]+$`)

// ---------- 运行时配置热更新（WebUI 配置页，2026-08-29，2026-09-09 单实例化） ----------

var hotSections = []string{"llm", "context", "tools", "doom", "heartbeat"}

func (ap *App) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := ap.state.Agent.Cfg() // COW 读：不可变对象，直接序列化
	buf, _ := json.Marshal(cfg)
	var global map[string]interface{}
	_ = json.Unmarshal(buf, &global)
	restart := make([]string, 0, len(restartSections))
	for _, s := range restartSections {
		restart = append(restart, fmt.Sprintf("%s（改后需重启进程）", s))
	}
	// global 是本 handler 刚 Unmarshal 出来的、没人共享 → 直接加键。原来调 mergeMaps
	// 复制一份，而那个包级函数全仓只有这一处用、名字却像公用工具（2026-11-12：删掉它，
	// 别让后来人以为包里有公用 map 合并，去依赖一个只服务这里的拼接）。
	global["restart_required"] = restart
	writeJSON(w, 200, map[string]interface{}{
		"global":       global,
		"hot_sections": hotSections,
		"effective":    cfg,
		"llm_client":   llmCfgOf(cfg.LLM),
	})
}

// App HTTP 应用（实现 http.Handler）
type App struct {
	state    *AppState
	hooks    *Hooks
	resolver atomic.Pointer[inject.Resolver] // /strip-injects 清缓存（读）与 /reset 重建（写）并发时原子安全（2026-09-14 审计修）
}

// New 构建单实例应用。

// New 构建单实例应用。
func New(state *AppState, hooks *Hooks) *App {
	ap := &App{state: state, hooks: hooks}
	// 上下文注入（2026-09-11）：组装请求时把 inject_ref 解析成真实 parts
	// 载入回退（2026-09-13）：侧车载入失败 → 移除附件 + 报错文字存根回传 LLM
	ap.reconfigureResolver()
	return ap
}

// ServeHTTP 访问日志（2026-10-18 复查修 3，同日补两条实测缺陷）：以前实例根本不
// 记请求路径，"有没有人在用某个端点"永远无法回答（判断 /chat/stream 能不能删、
// 外部插件有没有按已发出去的旧文档调用，都卡在这）。只记 method/path/status/耗时，
// **绝不记 body 与 query**：body 里是会话内容与附件路径，query 里也无必要信息。
//
// 逐条记：所有非 GET（写操作、队列、重置、注入、配置改动）、任何非 2xx（404/400
// 正是"有人在调我们不知道的东西"的信号）、以及不在轮询清单里的 GET。
// 只计数：轮询类 GET 且状态正常（见 noisyGetPaths）。
// /events（SSE）特殊：建立与结束各记一行——只在 handler 返回后记的话，一条活着
// 的连接在日志里完全不存在（实测 QQ 桥明明连着，/events 却是 0 条，只能靠 ss 确认）。
func (ap *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// 写操作的来源守卫（2026-10-19）：见 writeOriginOK。GET 不拦——没有 CORS 头时
	// 跨源脚本读不到响应体，而副作用全在写端点上。
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if code, why := ap.writeOriginOK(r); code != 0 {
			ap.state.logf("[http] %s %s → %d 拒绝（来源不可信）：%s", r.Method, path, code, why)
			writeJSON(w, code, map[string]interface{}{"error": why})
			return
		}
	}
	if path == "/events" {
		ap.state.logf("[http] %s %s → 连接建立", r.Method, path)
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		ap.route(rec, r)
		ap.state.logf("[http] %s %s → 连接结束 %d (%dms)", r.Method, path, rec.status, time.Since(start).Milliseconds())
		return
	}
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	ap.route(rec, r)
	if r.Method == http.MethodGet && rec.status < 400 && noisyGetPaths[path] {
		ap.countNoisyReq(path)
		return
	}
	ap.state.logf("[http] %s %s → %d (%dms)", r.Method, path, rec.status, time.Since(start).Milliseconds())
}

// route 路由本体（与 TS 端点全集一一对应）

// broadcastEvent 广播一条事件给所有 SSE 订阅者（/events 常驻通道）。
// 每个订阅者独立隔离——某个慢客户端写失败不影响其他订阅者。
// 同时维护"轮次缓冲"（roundBuf）：run_start→run_end 的帧逐条记录，
// 新订阅者连上且正在跑时重放（见 /events），刷新后不丢已流出内容。
// maxRoundFrames 轮次缓冲帧数上限（2026-09-14 审计修：旧实现无界，超长 run
// 的 delta 帧可把内存撑大；超限后停止追加——实时流不受影响，仅新连接重放
// 不完整）
const maxRoundFrames = 100000

// countNoisyReq 累计一次高频请求；距上次汇总满 noisyLogWindow 才打一行。
// 新建计数时把 lastLog 记为当下：启动后第一小时内不打（既然不逐条记，
// 开机那行也没有诊断价值），之后每小时最多一行。
func (ap *App) countNoisyReq(path string) {
	ap.state.reqMu.Lock()
	c := ap.state.reqCounts[path]
	if c == nil {
		c = &noisyCounter{lastLog: time.Now()}
		ap.state.reqCounts[path] = c
	}
	c.count++
	due := time.Since(c.lastLog) >= noisyLogWindow
	var inWindow, total int64
	if due {
		inWindow, total = c.count-c.loggedAt, c.count
		c.loggedAt = c.count
		c.lastLog = time.Now()
	}
	ap.state.reqMu.Unlock()
	if due {
		ap.state.logf("[http] GET %s 近 %v 被调 %d 次（累计 %d；高频轮询，不逐条记）", path, noisyLogWindow, inWindow, total)
	}
}

// eventPollTick 消费者从缓冲取批次的节奏（SSE 与 WebUI 短轮询同一口径）
const eventPollTick = 200 * time.Millisecond

// handleEventsPoll WebUI 短轮询取事件（2026-10-18 修 B：发车 + 前端自己取）。
// since=-1（或缺省）= 新客户端：有轮在跑就从本轮开头给（界面不空白），否则只报
// 最新号（已完成轮的历史由 /history 给）。since=N = 取 N 之后的批次。
// gap>0 = 客户端落后超过缓冲窗口，中间 gap 批已被挤掉 → 前端必须提示并走 /history 补。

// heartbeatTickMS 心跳巡检粒度：30s 一次。间隔本身以分钟配置，30s 粒度足够，
// 且空转成本可忽略（一次配置读 + 两次原子读）。
const heartbeatTickMS = 30000

// heartbeatDue 触发判定（纯函数，可测）：开关关或正在 run → 不触发；
// 距上次 run 结束 ≥ interval → 触发。interval <=0 退回默认值。

// logf 落盘日志（Log 未注入时静默，不 panic）
func (st *AppState) logf(format string, args ...interface{}) {
	if st.Log == nil {
		return
	}
	st.Log(fmt.Sprintf(format, args...))
}

const maxBodyBytes = 256 << 20

// noisyGetPaths 高频轮询的只读端点：WebUI / QQ 桥 / 语音每秒甚至每 200ms 打一次，
// 逐条记会把日志刷爆（实测启动 4 分钟就 196 行访问日志，把真正要查的东西冲掉）
// —— 这类只按端点累计次数，每 noisyLogWindow（1 小时）一行汇总。
var noisyGetPaths = map[string]bool{
	"/queue": true, "/status": true, "/health": true, "/history": true, "/events/poll": true,
}

// noisyLogWindow 高频端点汇总行的最小间隔（2026-10-19 主人定：一小时一行）。
// 按时间窗而不是按次数：按次数（曾定 3000）依赖速率恒定，WebUI 多开一个标签页
// 或关掉，换算就失真；按时间窗则与速率无关，且每行报的是这一小时内的次数，
// 速率涨跌一眼能看出来。每个端点独立计时，互不影响。
// 权衡：一小时一行意味着"某端点被异常高频调用"最晚一小时后才留痕——真要查异常，
// 靠的是逐条记的写操作与非 2xx，不是这些汇总行。
const noisyLogWindow = time.Hour

// noisyCounter 单个高频端点的计数与上次汇总时刻。

// numChecks 数值段基础校验（与 TS 表一致）
// 时间类配置统一以**秒**为单位（2026-10-18；旧 *_ms 键在 POST /config 入口自动
// 换算，见 config.NormalizeTimeUnits）。上限是防"把毫秒数值填进秒键"——旧
// timeout_ms=300000 若被当成秒就是 3.5 天，必须挡在门外。
var numChecks = map[string]func(interface{}) bool{
	"port":               func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 && n < 65536 },
	"timeout_sec":        func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 && n <= 86400 }, // ≤24h（秒）
	"keep_recent_chars":  func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 },
	"hard_trunc_chars":   func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 },
	"stream_idle_sec":    func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 && n <= 3600 },      // ≤1h（秒）
	"max_response_bytes": func(v interface{}) bool { n, ok := v.(float64); return ok && n >= 1024 && n <= 4<<30 }, // 1KB–4GiB（字节）
	"max_tokens":         func(v interface{}) bool { n, ok := v.(float64); return ok && n > 100 },
	// keep_summaries：摘要窗口条数。下限 1：0/负值在语义上是"压掉原文却一条摘要都不留"，这个组合
	// 没人要，而且会被 context.NormalizeCfg 静默兜成 5 —— 所以直接拒绝，不接受后再偷偷改写。
	// 上限 20：摘要本身也吃上下文，更多只会成倍挤掉原文。
	"keep_summaries": func(v interface{}) bool { n, ok := v.(float64); return ok && n >= 1 && n <= 20 },
	// summary_max_chars：单条摘要正文硬上限。过小（<1000）会把摘要压成索引，
	// 过大（>200k）则"摘要比原文预算还长"，自相矛盾。
	"summary_max_chars": func(v interface{}) bool { n, ok := v.(float64); return ok && n >= 1000 && n <= 200000 },
	// chars_per_token：0/负数 → 估算 Infinity/恒超阈（每条消息白压一次）；过大 →
	// 严重低估 → 永不压缩 → 撑爆模型上下文。英文 ~4 字符/token，20 已极宽松。
	"chars_per_token": func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 && n <= 20 },
	"result_limit":    func(v interface{}) bool { n, ok := v.(float64); return ok && n > 1000 },
	"fg_timeout_sec":  func(v interface{}) bool { n, ok := v.(float64); return ok && n > 0 && n <= 86400 }, // ≤24h（秒）
	"warn_after":      func(v interface{}) bool { n, ok := v.(float64); return ok && n >= 2 },
	// interval_min：0/负 → 每 30s 巡检都会触发（等于把 agent 变成自循环）；上限
	// 1440 分钟（24h）防手滑写成天级。
	"interval_min": func(v interface{}) bool { n, ok := v.(float64); return ok && n >= 1 && n <= 1440 },
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
		ap.state.logf("[inject] 附件 %s 载入失败（%s）→ 已从附件列表摘除，报错存根回传 LLM", id, reason)
	}
	res := inject.NewResolver(ap.injectDirOf(), onFail)
	ap.resolver.Store(res)
	ctxmgr.ConfigureInjectResolver(res)
}

// route 路由本体（与 TS 端点全集一一对应）
func (ap *App) route(w http.ResponseWriter, r *http.Request) {
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
	case r.Method == http.MethodPost && path == "/enqueue":
		ap.handleEnqueue(w, r)
	case r.Method == http.MethodGet && path == "/events":
		ap.handleEvents(w, r)
	case r.Method == http.MethodGet && path == "/events/poll":
		ap.handleEventsPoll(w, r)
	case r.Method == http.MethodPost && path == "/inject":
		ap.handleInject(w, r)
	case r.Method == http.MethodPost && path == "/strip-injects":
		ap.handleStripInjects(w, r)
	case r.Method == http.MethodGet && path == "/attachments":
		ap.handleAttachments(w, r)
	case r.Method == http.MethodPost && path == "/compress":
		ap.handleCompressNow(w, r)
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
		ap.handleWebuiAsset(w, r, "marked.min.js")
	case r.Method == http.MethodGet && path == "/sanitize.js":
		ap.handleWebuiAsset(w, r, "sanitize.js")
	default:
		http.NotFound(w, r)
	}
}

// ---------- 只读端点 ----------

// statusRecorder 只为拿到响应状态码。**必须显式转发 Flush**：SSE 端点里
// newSSE 做 w.(http.Flusher) 断言，内嵌 ResponseWriter 不会自动实现 Flusher，
// 忘了转发 → 断言失败 → 事件流不再 flush（表现为"连上了但收不到东西"）。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// typeChecks 布尔/字符串键的类型校验（2026-10-18）：applyMap 走 json.Unmarshal，
// 类型不符的键会被**静默跳过**、落回 Default() 的值——enabled 写成 "yes" 等于
// 把心跳悄悄关掉（Default 是 false），prompt 写成数字等于把自定义提示词换回出厂词。
// 这类补丁现在多由 agent 自己 curl 发出，手滑概率远高于人填表单，必须 400 明说，
// 不能"接受但改错"。
var typeChecks = map[string]struct {
	chk  func(interface{}) bool
	want string
}{
	"enabled": {func(v interface{}) bool { _, ok := v.(bool); return ok }, "布尔 true/false"},
	"prompt":  {func(v interface{}) bool { _, ok := v.(string); return ok }, "字符串"},
}

// webui 静态脚本（2026-09-09 marked；2026-10-19 加 sanitize）：本地化随 webui/ 部署，
// 不依赖 CDN；文件缺失时返回 404（前端 md() 会回退纯文本，页面不崩）。
func (ap *App) handleWebuiAsset(w http.ResponseWriter, r *http.Request, name string) {
	// name 是代码里的常量（不是请求参数），不存在穿越；safeName 再挡一道以防将来改成动态
	if !safeName(name) {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(ap.state.Root, "webui", name))
	if err != nil {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, name+" 未部署")
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

// ---------- 工具函数 ----------

// safeName 提示词文件名合法性（防穿越）

func (ap *App) writeOriginOK(r *http.Request) (int, string) {
	allowed := map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true, "": true}
	if h := ap.state.Agent.Cfg().Server.Host; h != "" {
		allowed[h] = true
	}
	name := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if !allowed[name] {
		return 403, fmt.Sprintf("Host %q 不是本机地址（疑似 DNS rebinding 或端口转发）", r.Host)
	}
	if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" {
		u, err := url.Parse(o)
		if err != nil {
			return 403, fmt.Sprintf("Origin %q 无法解析", o)
		}
		if !allowed[strings.Trim(u.Hostname(), "[]")] {
			return 403, fmt.Sprintf("Origin %q 不是本机同源（浏览器跨站写请求）", o)
		}
	}
	return 0, ""
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
			writeJSON(w, 400, map[string]interface{}{"error": fmt.Sprintf("未知配置段：%s（允许：llm / context / tools / doom / heartbeat / server / data / system_prompt）", k)})
			return
		}
	}
	// 旧 *_ms 键 → 新 *_sec 键（向上取整）：旧 webui / 旧脚本发旧键不会静默落回默认值。
	// 换算发生在校验与落盘之前 → 落盘的永远是新键。
	unitNotes := config.NormalizeTimeUnits(patch)
	// 数值段基础校验
	for section, secVal := range patch {
		sec, ok := secVal.(map[string]interface{})
		if !ok {
			writeJSON(w, 400, map[string]interface{}{"error": fmt.Sprintf("段 %s 需为 JSON 对象", section)})
			return
		}
		for k, v := range sec {
			if tc, ok := typeChecks[k]; ok && !tc.chk(v) {
				vb, _ := json.Marshal(v)
				writeJSON(w, 400, map[string]interface{}{"error": fmt.Sprintf("%s.%s 类型非法：%s（需 %s）", section, k, string(vb), tc.want)})
				return
			}
			if kk := config.KnownKeys(section); len(kk) > 0 && !kk[k] {
				names := make([]string, 0, len(kk))
				for n := range kk {
					names = append(names, n)
				}
				sort.Strings(names)
				writeJSON(w, 400, map[string]interface{}{"error": fmt.Sprintf(
					"未知配置键：%s.%s（这一段可写：%s）", section, k, strings.Join(names, ", "))})
				return
			}
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
	rl := applyConfigPatch(ap.state.Agent, patch, ap.state.logf)
	// 落盘结果必须如实上报（2026-10-18 复查修 1）：旧实现把 AtomicWriteFile 的错误
	// 吞掉，无论成败都打"已保存并即时生效"并回 ok:true → 盘满/只读/权限变了时，
	// 用户以为改动扛得住重启，实际重启后回到旧值，且日志里查不到失败痕迹。
	// 现在：内存里确实生效了（applyConfigPatch 已做），但 saved 单独表态。
	saved := true
	var saveErr string
	if ap.hooks != nil && ap.hooks.OnConfigSaved != nil {
		if err := ap.hooks.OnConfigSaved(patch); err != nil {
			saved = false
			saveErr = err.Error()
			ap.state.logf("[config] ⚠ 配置已在内存生效，但落盘 config/user.json 失败：%s —— 重启后会回到旧值", saveErr)
		}
	}
	if saved {
		keys := make([]string, 0, len(patch))
		for k := range patch {
			keys = append(keys, k)
		}
		ap.state.logf("[config] 全局配置已保存并即时生效（落盘 config/user.json）：%s", strings.Join(keys, ", "))
	}
	resp := map[string]interface{}{"ok": true, "scope": "global", "saved": saved,
		"applied": rl.Applied, "restart": rl.Restart, "unit_notes": unitNotes}
	if !saved {
		resp["save_error"] = saveErr
		resp["warning"] = "配置已在本次运行生效，但未能写入 config/user.json —— 重启后会回到旧值（原因见 save_error）"
	}
	writeJSON(w, 200, resp)
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

func (ap *App) handleQueue(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	ap.state.mu.Lock()
	queued := queuedTexts(ap.state.MessageQueue)
	qItems := copyQueue(ap.state.MessageQueue) // 同一把锁内取快照（锁外读队列 = 竞态）
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
	writeJSON(w, 200, map[string]interface{}{"running": running, "user_seq": userSeq,
		"queued": queued, "queued_items": qItems})
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// noisyGetPaths 高频轮询的只读端点：WebUI / QQ 桥 / 语音每秒甚至每 200ms 打一次，
// 逐条记会把日志刷爆（实测启动 4 分钟就 196 行访问日志，把真正要查的东西冲掉）
// —— 这类只按端点累计次数，每 noisyLogWindow（1 小时）一行汇总。

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

var injIDRe = regexp.MustCompile(`^inj-\d+$`)

var restartSections = []string{"server", "data", "system_prompt"} // 改后需重启进程
