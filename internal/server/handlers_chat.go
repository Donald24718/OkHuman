package server

import (
	"net/http"
	"okhuman/internal/textutil"
	"strings"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/types"
)

// ---------- /stop：中断当前 LLM 输出（2026-08-29） ----------
// 语义：cancel 当前 LLM 请求（HTTP 断开 → llama.cpp 检测客户端断连取消生成）；
// 若正处工具执行中，本轮跑完工具后在下次 LLM 调用前强停。已生成的增量内容
// 保留进会话（partial 收尾），不会丢。
func (ap *App) handleStop(w http.ResponseWriter, r *http.Request) {
	ap.state.Agent.Agent().Stop()
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// ---------- 提示词（WebUI 提示词页，2026-08-29） ----------

// ---------- 手动压缩 / 手动剥离附件（2026-10-18，取代旧"LLM 4xx 三级回退"）----
//
// 旧阶梯靠硬编码 400/413/415 猜"请求体被拒"，是按 llama.cpp 行为定的，换后端
// 就不成立（strata 同一件事可能报别的码）。现在机器不猜，两个动作都由人按按钮。

// handleCompressNow 手动压缩上下文：不等 token 阈值，按常规分批口径立刻压最旧一批。
// run 进行中拒绝（409）——压缩会换掉会话结构（旧消息出列 + summary 替换），与在飞
// run 交错只会让下一轮看到意外的上下文。本请求会阻塞到压缩 LLM 调用结束（可能几十
// 秒到几分钟）；进度经 SSE 的 compress_delta 实时可见。
func (ap *App) handleCompressNow(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	if a.Lock.Running() {
		writeJSON(w, 409, map[string]interface{}{"error": "run 进行中：等本轮结束再手动压缩"})
		return
	}
	// 压缩期间持 RunLock（running=true）：防新 run 与压缩并发改会话结构（上面的 409
	// 只挡住"已开跑"的 run，不挡这一句之后挤进来的新 run）。
	// 事件广播到 /events：前端聊天区出现与自动压缩一样的"压缩中"流式活卡 + 最终压缩卡
	// （2026-10-18 修：此前手动压缩没人订阅压缩事件，点按钮后界面全程无输出）。
	// 注：手动压缩不造 run_start/run_end 帧（不占轮次缓冲）→ 压缩途中刷新页面看不到
	// 已流出的部分，压完的卡片由 /status 的 compress 日志与 /history 兜住。
	var detail string
	var did bool
	a.Lock.Run(func() error {
		detail, did = a.Agent().CompressNowWithEvents(func(e types.AgentEvent) {
			broadcastEvent(ap.state, e)
		})
		return nil
	})
	// detail 里带两个落盘路径（全文存档 / 总结文本），UI 直接显示
	// 只回 ok：曾经的 "did" 与它同值、无人读（2026-11-12 审查）
	writeJSON(w, 200, map[string]interface{}{"ok": did, "detail": detail})
}

// handleStripInjects 手动剥离全部外部注入附件：逐个把侧车永久回写成文本提示 +
// 清解析器缓存 → 后续 compose 不再发原附件内容（永久，不是本轮请求级）。
// run 进行中**允许**：坏附件卡住 run 时这正是自救路径，且它只改"引用解析出什么"，
// 不动会话结构（不增删消息 → 无索引漂移）。

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
	h, queuedBefore := enqueueAndWake(ap.state, a, message, types.QueueKindUser, makeOnEvent(ap.state, a))
	position := queuedBefore + 1
	broadcastEvent(ap.state, types.AgentEvent{"type": "enqueued", "message": message, "position": position, "running": running})
	go func() { <-h.done }() // 防 unhandledRejection 等价：错误广播由 drain 统一处理
	writeJSON(w, 200, map[string]interface{}{"ok": true, "queued": queuedBefore, "position": position, "running": running})
}

// ---------- /events：常驻事件广播（2026-08-31 方案A） ----------
// 订阅本实例的全部 run 事件（run_start/delta/tool_*/done/…）。前端一个常驻
// EventSource，刷新/重开页面后续接——不再丢"刷新瞬间正在流式那轮"的显示。
// handleEvents SSE 常驻事件流（QQ 桥 / voice-chat 等外部消费者的通道，
// **协议字节级不变**：同样的 event:/data: 帧、同样的 hello 与本轮重放）。
//
// 2026-10-18 健壮性修 B：写 socket 从 run 的 goroutine 挪到**本 handler 自己的
// goroutine**——run 只把帧放进 state.Evt 内存缓冲，这里按节奏从缓冲取、往本连接
// 写。慢客户端只会卡住自己这个 handler，卡不到 run（旧实现里 run 直接写 socket：
// 一个挂起的标签页能让本轮被判空闲超时；压缩更会被连累——3 次空闲重试后触发
// 硬截断，最老一批上下文没有 summary 就没了）。
// 不踢客户端：本连接落后超过缓冲窗口时，缺口由 /history 兜底（QQ 桥本来就有
// /history 兜底轮询）。

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
	summaries := make([]map[string]interface{}, 0, len(sess.Summaries))
	for _, sm := range sess.Summaries {
		summaries = append(summaries, map[string]interface{}{
			"content":      sm.Content,
			"seq_from":     sm.SeqFrom,
			"seq_to":       sm.SeqTo,
			"created_at":   sm.CreatedAt,
			"chars":        textutil.UTF16Len(sm.Content),
			"orig_chars":   sm.OrigChars,
			"truncated":    sm.Truncated,
			"summary_file": sm.SummaryFile,
			"record_file":  sm.RecordFile,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"session": int(a.SessionNo.Load()), "summaries": summaries, "entries": entries})
}

// ---------- /enqueue ----------

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

	summaries := make([]map[string]interface{}, 0, len(sess.Summaries))
	for _, sm := range sess.Summaries {
		summaries = append(summaries, map[string]interface{}{
			"chars":      textutil.UTF16Len(sm.Content),
			"orig_chars": sm.OrigChars,
			"truncated":  sm.Truncated,
			"seq_from":   sm.SeqFrom,
			"seq_to":     sm.SeqTo,
			"created_at": sm.CreatedAt,
			"preview":    textutil.UTF16Slice(sm.Content, 120),
		})
	}

	// raw_tail：末 5 条消息（chars/preview 口径与 TS 一致）
	msgs := sess.Messages
	from := len(msgs) - 5
	if from < 0 {
		from = 0
	}
	// 字符数走 context 那把尺子（attachment_tokens 可配、tool_calls/reasoning 也计入），
	// 与同一次响应里的 msg_chars 同口径。旧写法在这里自己算一遍、附件按写死的 1000 计，
	// 两处数对不上（2026-11-12 归并）。预览仍只取文本 parts。
	est := ctxmgr.NewEst(cfg.Context.CharsPerToken, cfg.Context.AttachmentTokens)
	rawTail := make([]map[string]interface{}, 0, len(msgs)-from)
	for i := from; i < len(msgs); i++ {
		e := &msgs[i]
		var text strings.Builder
		if s, ok := e.Content.(string); ok {
			text.WriteString(s)
		} else {
			for _, p := range types.AsParts(e.Content) {
				if p.Type == "text" {
					text.WriteString(p.Text)
				}
			}
		}
		rawTail = append(rawTail, map[string]interface{}{
			"role":    e.Role,
			"chars":   est.MessageChars(*e),
			"preview": textutil.UTF16Slice(text.String(), 120),
		})
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
	spChars := textutil.UTF16Len(a.SystemPrompt)
	a.promptMu.Unlock()

	writeJSON(w, 200, map[string]interface{}{
		"session":   int(a.SessionNo.Load()),
		"running":   running,
		"heartbeat": heartbeatStatus(cfg.Heartbeat, ap.state.LastRunEnd.Load(), running),
		"system_prompt": map[string]interface{}{
			"fallback": fallback,
			"files":    files,
			"chars":    spChars,
		},
		"context":     ctxmgr.ContextStatsOf(sess, cfg.Context.CharsPerToken, cfg.Context.AttachmentTokens, cfg.Context.KeepSummaries, cfg.Context.SummaryMaxChars),
		"compress":    compress,
		"doom":        doom,
		"backgrounds": bgs,
		"summaries":   summaries,
		"raw_tail":    rawTail,
	})
}
