package agent

// Agent 主循环：一轮消息内的 LLM 迭代 + 工具执行 + 死循环检测（§8）
// + 前台超时转后台（§7.3）。
//
// 一轮流程：
//   消息 → [LLM → (工具调用 → 执行 → 结果回填 → 再 LLM)*] → 文本回复
//
// 前台超时转后台（§7.3，2026-08-25 定）：
//   工具调用与"前台超时"赛跑：
//     - 超时前完成 → 正常回填结果；
//     - 超时 → 不取消，转入后台继续执行，立即回填一条"已转后台"的 tool 消息，
//       模型不必傻等（可继续干活或直接收尾）；
//   双向通知：
//     - 转后台时 → 模型收到"已转后台、出结果会自动通知"（上面的 tool 消息）；
//     - 后台出结果时（settle，2026-09-14 统一）→ 格式化成系统通知 push 进
//       实例消息队列（与用户追加消息同一队列、同一逻辑）+ 唤醒 drain：
//       有车（本轮在跑）= 工具循环的下次 LLM 调用之前与当批工具结果同车
//       注入（纯追加）；没车（本轮已结束）= 本轮完全结束后 drain 拿队列
//       发新轮（RunLock 串行，不抢 LLM）。
//   所有 run（用户消息 / 后台通知 / 定时任务触发 §9）由该 agent 的运行锁
//   串行，避免并发写上下文。
//
// 死循环（§8）：连续 doomWarnAfter 次（默认 3）完全相同（工具名+参数逐字段，
//   稳定序列化比较）→ 注入警告；警告后仍重复 → 强停本轮。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/llm"
	"okhuman/internal/tools"
	"okhuman/internal/types"
)

// RunOptions 一轮 run 的参数
type RunOptions struct {
	// 落盘根目录：截断工具结果写 <dataDir>/tool-results/<时间戳>.log
	DataDir string
	// 工具结果回填上限（字符，可配置，默认 50000）：超出截断 + 全文落文件
	ResultLimit int
	DoomWarnAfter int
	// 前台执行超时（毫秒）：超时不取消，转后台 + 双向通知（§7.3）
	FgTimeoutMS int
	// 轮内搭车（2026-09-14 统一）：工具循环中每次 LLM 调用前调用，取走运行期间
	// 入队的全部消息——用户追加消息与已 settle 后台任务的通知（同一消息队列、
	// 同一 splice 逻辑、取走即消费；纯追加，不加 LLM 调用）
	TakePendingUsers func() []string
	// 工具转后台时回调（由 server.runOpt 实现：注册任务 + 挂 settle 回调；
	// settle 时通知自动入消息队列，2026-09-14）
	OnBackgroundStart func(types.BackgroundStartArgs)
	OnEvent           func(types.AgentEvent)
	// 本轮消息来源（事件广播标记用，2026-08-31）：user = 单条用户消息；
	// merged = 运行中积攒的多条合并消息。
	Kind string
	// 合并轮的消息条数（kind=merged 时）
	Messages int
	// 描述性标签（事件广播展示用，如 "3 条合并 / 1234 字符"）
	Label *string
}

// TokenTotals 一轮 run 的 token 累计（done 事件 usage 字段）
type TokenTotals struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

// PartialContent 中断时 LLM 已生成的增量内容（可能为空 = 中断发生在 LLM 调用前）
type PartialContent struct {
	Thinking string
	Text     string
}

// AgentStoppedError 用户 /stop 中断（2026-08-29）：runUserMessage 顶层捕获后收尾
type AgentStoppedError struct {
	Partial PartialContent
}

func (e *AgentStoppedError) Error() string { return "agent stopped by user" }

// Agent 单实例 agent（运行锁在 server 层持有；同 agent 内 run 串行）
type Agent struct {
	mu            sync.Mutex
	llm           llm.LlmClient
	systemPrompt  string
	cm            *ctxmgr.Manager
	lastCalls     []callRec
	warned        bool
	stopRequested atomic.Bool
	callCancel    context.CancelFunc // 当前 LLM 请求的中断（/stop → fetch 断开）
	runCancel     context.CancelFunc // run 级中止（主循环 + 压缩流整体）
}

type callRec struct {
	Name string
	Key  string
}

// New 新建 agent
func New(client llm.LlmClient, systemPrompt string, cm *ctxmgr.Manager) *Agent {
	return &Agent{llm: client, systemPrompt: systemPrompt, cm: cm}
}

// SetLLM 运行时热更新 LLM 客户端（WebUI 配置页改 llm 段后）
func (a *Agent) SetLLM(client llm.LlmClient) {
	a.mu.Lock()
	a.llm = client
	a.mu.Unlock()
}

// SetSystemPrompt 运行时热更新系统提示词（WebUI 提示词页重载后）
func (a *Agent) SetSystemPrompt(p string) {
	a.mu.Lock()
	a.systemPrompt = p
	a.mu.Unlock()
}

// Stop /stop 入口：中断当前 LLM 输出（含 llama.cpp 侧生成）；工具执行完后的下一次 LLM 前强停
func (a *Agent) Stop() {
	a.stopRequested.Store(true)
	a.mu.Lock()
	cancel := a.callCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// run 级中止（2026-09-01，压缩改流式后）：中断进行中的压缩流（覆盖整轮）。
	a.mu.Lock()
	runCancel := a.runCancel
	a.mu.Unlock()
	if runCancel != nil {
		runCancel()
	}
}

// Run 处理一条用户消息（HTTP /chat 入口）：跑完整轮工具循环，返回最终文本。
// 调用方必须持有运行锁（同 agent 内串行）。
func (a *Agent) Run(ctx context.Context, userText string, opt RunOptions) (string, error) {
	return a.runUserMessage(ctx, userText, opt)
}

// runUserMessage 通用 run 内核：注入一条 user 消息 → LLM 迭代（工具循环）→ 最终文本
func (a *Agent) runUserMessage(ctx context.Context, userText string, opt RunOptions) (string, error) {
	emit := opt.OnEvent
	if emit == nil {
		emit = func(types.AgentEvent) {}
	}
	kind := opt.Kind
	if kind == "" {
		kind = "user"
	}

	a.mu.Lock()
	a.lastCalls = nil
	a.warned = false
	a.mu.Unlock()
	a.stopRequested.Store(false)
	totals := TokenTotals{}

	// run 级中止（2026-09-01，压缩改流式后 /stop 可中断压缩流）：覆盖整轮（主循环 +
	// 滚动 summary 压缩）。主循环的 llmCall 另有 callCancel（只管单次调用），runCancel
	// 经 cm.SetRunSignal 传给压缩流，两者都挂在 /stop 上。
	runCtx, runCancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.runCancel = runCancel
	a.mu.Unlock()
	a.cm.SetRunSignal(&runCtx)

	offCompressDelta := a.cm.AddCompressDeltaListener(func(scope string, d types.Delta) {
		if d.Thinking != "" || d.Text != "" {
			ev := types.AgentEvent{"type": "compress_delta", "scope": scope}
			if d.Thinking != "" {
				ev["thinking"] = d.Thinking
			}
			if d.Text != "" {
				ev["text"] = d.Text
			}
			emit(ev)
		}
	})
	offCompress := a.cm.AddCompressListener(func(e ctxmgr.CompressEvent) {
		ev := types.AgentEvent{
			"type":         "compress",
			"scope":       e.Scope,
			"detail":      e.Detail,
			"before_chars": e.BeforeChars,
			"after_chars": e.AfterChars,
			"rounds":      e.Rounds,
			"text":        e.Text,
		}
		if e.Thinking != nil {
			ev["thinking"] = *e.Thinking
		}
		emit(ev)
	})

	defer func() {
		// 正常/中断/出错统一收尾（defer 保证恰好一次）
		emit(types.AgentEvent{"type": "run_end"})
		offCompress()
		offCompressDelta()
		a.cm.SetRunSignal(nil)
		a.mu.Lock()
		a.runCancel = nil
		a.mu.Unlock()
		runCancel()
		a.stopRequested.Store(false)
	}()

	// run 生命周期标记（2026-08-31 事件广播用）：前端据 run_start 建流式占位、
	// run_end 定型——刷新/重开页面后从 /events 续接也能正确对齐轮次。
	start := types.AgentEvent{
		"type":          "run_start",
		"kind":          kind,
		"message_chars": utf16Len(userText),
	}
	if opt.Label != nil {
		start["label"] = *opt.Label
	}
	if opt.Messages > 0 {
		start["messages"] = opt.Messages
	}
	emit(start)

	a.cm.AddMessage(&types.RawEntry{Role: "user", Content: userText})
	// 广播本轮实际发出的消息文本（2026-08-31）：/events 重放帧含它，
	// 刷新/重开页面后"刷新瞬间在跑那轮"的 user 气泡不丢（前端按文本去重不重画）。
	emit(types.AgentEvent{"type": "run_msg", "message": userText, "kind": kind})

	resp, err := a.llmCall(runCtx, emit, &totals)
	if err != nil {
		return a.finishRunError(err, runCtx, emit, &totals)
	}
	// 纯文本回复 → 本轮结束
	if len(resp.ToolCalls) == 0 {
		return a.finishText(resp, emit, &totals)
	}
	// 注意：必须等 runLoop 完成后再 return（Go 无 JS 的 return-promise 陷阱，
	// defer 顺序天然正确）
	return a.runLoop(runCtx, resp, emit, &totals, opt)
}

// finishRunError runUserMessage 顶层错误收尾：AgentStoppedError → partial 收尾；
// runCtx 已取消（压缩流被 /stop 中断等）→ 空 partial 收尾；其他 → 上抛
func (a *Agent) finishRunError(err error, runCtx context.Context, emit func(types.AgentEvent), totals *TokenTotals) (string, error) {
	if s, ok := err.(*AgentStoppedError); ok {
		a.finishStopped(s.Partial, emit, totals)
		return s.Partial.Text, nil
	}
	if runCtx.Err() != nil {
		// 压缩流被 /stop 中断（非 AgentStoppedError）→ 按"用户停止"收尾（空 partial），
		// 不当系统错误抛（否则 drain 会广播 error 卡）
		a.finishStopped(PartialContent{}, emit, totals)
		return "", nil
	}
	return "", err
}

// finishStopped /stop 中断收尾：partial 内容存 assistant 消息，发 stopped 事件。
// partial 正文已由 delta 逐块流出（流式），不重发全量。
func (a *Agent) finishStopped(partial PartialContent, emit func(types.AgentEvent), totals *TokenTotals) {
	body := partial.Text
	if body == "" {
		body = "（已中断：用户手动停止）"
	}
	var reasoning *string
	if partial.Thinking != "" {
		rc := partial.Thinking
		reasoning = &rc
	}
	a.cm.AddMessage(&types.RawEntry{Role: "assistant", Content: body, ReasoningContent: reasoning})
	emit(types.AgentEvent{"type": "stopped"})
	emit(types.AgentEvent{"type": "done", "usage": totals})
}

// bgNoticePrefix 后台完成通知的固定开头——轮内搭车时据此区分"系统通知"与
// 用户追加消息（不加【用户追加】前缀，2026-09-14）。
const bgNoticePrefix = "（系统通知：后台任务完成"

// FormatBackgroundNotice 后台完成通知的格式化（settle 入队与轮内搭车共用，
// 2026-09-14：settle 时由 server 回调调用，结果 push 进消息队列）：
// 头部一行状态（工具名 + 成功/失败 + 耗时），正文为完整结果；超 ResultLimit
// 时截断 + 全文落 <dataDir>/tool-results（与工具结果同一机制）。
func (a *Agent) FormatBackgroundNotice(task *types.PendingBackgroundTask, opt RunOptions) string {
	result := task.Result
	durSec := fmt.Sprintf("%.1f", float64(task.DurationMs)/1000)
	status := "已成功"
	if !task.OK {
		status = "已失败"
	}
	status = fmt.Sprintf("%s（耗时 %ss）", status, durSec)
	head := fmt.Sprintf("（系统通知：后台任务完成——你此前调用 %s 时前台等待超时，已转入后台继续执行，现%s。结果如下：\n", task.ToolName, status)
	if utf16Len(result) > opt.ResultLimit {
		logPath := a.writeToolLog(opt, result)
		tail := fmt.Sprintf("\n[已截断：完整结果 %d 字符在 %s，用 bash 的 cat / grep / tail 查看。]", utf16Len(result), logPath)
		budget := opt.ResultLimit - utf16Len(head) - utf16Len(tail)
		if budget < 0 {
			budget = 0
		}
		return head + utf16Slice(result, budget) + tail
	}
	return head + result
}

// writeToolLog 被截断的工具结果全文落文件（<dataDir>/tool-results/tool-<时间戳>.log），
// 返回绝对路径。文件名秒精度（tool- 前缀，与 session-records 的 ctx- 区分）。
func (a *Agent) writeToolLog(opt RunOptions, full string) string {
	dir := filepath.Join(opt.DataDir, "tool-results")
	ts := time.Now().UTC().Format("2006-01-02_15-04-05")
	path := filepath.Join(dir, "tool-"+ts+".log")
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if err := os.WriteFile(path, []byte(full), 0o644); err == nil {
			return path
		}
	}
	return fmt.Sprintf("[文件写入失败：%s]", "写入失败")
}

// llmCall 一次 LLM 调用（记 token 统计）。流式：delta 实时 emit（WebUI 边生成边显示）；
// /stop 时 cancel → fetch 断开 → llama.cpp 取消生成；本方法把已收的增量包成
// AgentStoppedError 抛出（由 runUserMessage 收尾）。
//
// LLM 4xx 三级回退阶梯（2026-09-13，用户拍板"粗暴版"）：
// ① 正常请求 → ② 4xx：最近一条带附件的消息降级文本提示重试 → ③ 仍 4xx：全部附件降级重试
// → ④ 仍 4xx：触发兜底硬截断压缩后重试（再败则抛，轮次死，人工介入）。
// 降级是请求级：不动会话/侧车/附件列表，下轮请求原样恢复；触发限 400/413/415
//（请求体被拒），其余错误（5xx/401/404/网络错）终止阶梯原样抛（配置类错误不配吃硬截断）。
func (a *Agent) llmCall(ctx context.Context, emit func(types.AgentEvent), totals *TokenTotals) (*types.Response, error) {
	// 上一轮 /stop 挂起后若本轮已重置（stopRequested=false 说明新 run），这里正常走
	if a.stopRequested.Load() {
		// 工具执行完后回到 LLM 调用前用户已 /stop → 直接强停本轮
		return nil, &AgentStoppedError{Partial: PartialContent{}}
	}
	a.mu.Lock()
	client := a.llm
	a.mu.Unlock()
	callCtx, callCancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.callCancel = callCancel
	a.mu.Unlock()

	partial := PartialContent{}
	onDelta := func(d types.Delta) {
		if d.Thinking != "" {
			partial.Thinking += d.Thinking
			emit(types.AgentEvent{"type": "delta", "thinking": d.Thinking})
		}
		if d.Text != "" {
			partial.Text += d.Text
			emit(types.AgentEvent{"type": "delta", "text": d.Text})
		}
	}
	isLlm4xx := isLlm4xxErr

	var resp *types.Response
	var ladderErr error
	stopped := func() bool { return a.stopRequested.Load() || callCtx.Err() != nil }

	messages := a.cm.Compose(nil)
	firstErr := a.streamOnce(callCtx, client, messages, onDelta, &resp)
	if firstErr != nil {
		// /stop：cancel 导致 fetch 读流抛错 → 转为 AgentStoppedError（带 partial）
		if stopped() {
			return nil, &AgentStoppedError{Partial: partial}
		}
		if !isLlm4xx(firstErr) {
			return nil, firstErr
		}
		// ===== 三级阶梯（有界，不循环）=====
		var modes []string
		if a.sessionHasInject() {
			modes = []string{"demote-newest", "demote-all", "truncate"}
		} else {
			modes = []string{"truncate"}
		}
		for _, mode := range modes {
			action := "触发兜底硬截断压缩"
			switch mode {
			case "demote-newest":
				action = "最近一条带附件的降级文本提示"
			case "demote-all":
				action = "全部附件降级文本提示"
			}
			fmt.Printf("[llm] LLM 4xx（%s）→ %s重试\n", truncateStr(firstErr.Error(), 200), action)
			if mode == "truncate" {
				a.cm.ForceHardTruncate()
			}
			switch mode {
			case "demote-newest":
				messages = a.cm.Compose(&ctxmgr.ComposeOptions{DemoteInjects: "newest"})
			case "demote-all":
				messages = a.cm.Compose(&ctxmgr.ComposeOptions{DemoteInjects: "all"})
			default:
				messages = a.cm.Compose(nil)
			}
			err2 := a.streamOnce(callCtx, client, messages, onDelta, &resp)
			if resp != nil {
				detail := truncateStr(firstErr.Error(), 300)
				fmt.Printf("[llm] 4xx 回退阶梯：%s 重试成功（首次报错：%s）\n", mode, detail)
				emit(types.AgentEvent{"type": "inject_fallback", "mode": mode, "detail": detail})
				break
			}
			if stopped() {
				return nil, &AgentStoppedError{Partial: partial}
			}
			if !isLlm4xx(err2) {
				return nil, err2 // 非 4xx → 阶梯终止
			}
			// 仍 4xx → 下一级
		}
		if resp == nil {
			ladderErr = fmt.Errorf("LLM 4xx 回退阶梯耗尽：%s", truncateStr(firstErr.Error(), 300))
		}
	}
	a.mu.Lock()
	a.callCancel = nil
	a.mu.Unlock()

	if resp == nil {
		if ladderErr == nil {
			ladderErr = fmt.Errorf("llmCall: 4xx 阶梯耗尽")
		}
		return nil, ladderErr
	}
	totals.PromptTokens += resp.Usage.PromptTokens
	totals.CompletionTokens += resp.Usage.CompletionTokens
	totals.CachedTokens += resp.Usage.CachedTokens
	return resp, nil
}

// streamOnce 一次流式调用（idleMs=0 → 客户端默认 max(timeout_ms,10min)）
func (a *Agent) streamOnce(ctx context.Context, client llm.LlmClient, messages []types.Message, onDelta func(types.Delta), respOut **types.Response) error {
	r, err := client.CompleteStream(ctx, messages, tools.TOOLS, onDelta, 0)
	if err == nil {
		*respOut = r
	}
	return err
}

// isLlm4xxErr LLM 4xx 判定（400/413/415 = 请求体被拒；与 TS 正则等价）
func isLlm4xxErr(e error) bool {
	h, ok := e.(*llm.HTTPError)
	if !ok {
		return false
	}
	return h.Is4xx()
}

// sessionHasInject 会话里是否存在附件引用（inject_ref）——4xx 阶梯只对"有附件"的会话降级
func (a *Agent) sessionHasInject() bool {
	for _, m := range a.cm.Session().Messages {
		if m.HasInjectRef() {
			return true
		}
	}
	return false
}

// finishText 纯文本收尾：存 assistant 消息 + 发事件。流式下正文/思考已由 delta
// 逐块发出，这里不再重发全量 text/thinking（避免 WebUI 重复渲染）。
func (a *Agent) finishText(resp *types.Response, emit func(types.AgentEvent), totals *TokenTotals) (string, error) {
	a.cm.AddMessage(&types.RawEntry{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent})
	emit(types.AgentEvent{"type": "done", "usage": totals})
	return resp.Content, nil
}

// runLoop 工具循环：从一次"带 tool_calls 的 LLM 响应"开始，逐个执行工具并回填，
// 直到模型返回纯文本（或强停后收尾）。不设迭代上限：靠 §8 死循环检测兜底。
func (a *Agent) runLoop(ctx context.Context, resp *types.Response, emit func(types.AgentEvent), totals *TokenTotals, opt RunOptions) (string, error) {
	for {
		calls := resp.ToolCalls
		if len(calls) == 0 {
			return a.finishText(resp, emit, totals) // 防御：纯文本收尾
		}
		// 先存 assistant 消息（带 tool_calls，原样回传）
		a.cm.AddMessage(&types.RawEntry{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: calls})

		// 逐个执行（并行工具调用也按序回填，保持 tool 消息与 tool_calls 一一对应）
		// 死循环（§8）：连续 N 次完全相同 → 注入警告；警告后仍重复 → 强停。
		// 警告/强停消息都在本批 tool 结果之后插入，保证 assistant.tool_calls 与 tool 消息严格配对。
		stopped := false
		doomWarnPending := false
		for _, tc := range calls {
			args := parseArgs(tc.Function.Arguments)
			a.mu.Lock()
			a.lastCalls = append(a.lastCalls, callRec{Name: tc.Function.Name, Key: stableKey(tc.Function.Name, args)})
			sameRun := a.isSameRunLocked(opt.DoomWarnAfter)
			warned := a.warned
			a.mu.Unlock()

			if warned && sameRun {
				// 警告后仍重复 → 强停：不执行，补合成 tool 结果保持协议完整
				syn := "（此调用已被系统强停：连续完全相同的调用，见随后系统消息）"
				a.cm.AddMessage(&types.RawEntry{Role: "tool", ToolCallID: &tc.ID, Content: syn})
				emit(types.AgentEvent{"type": "tool_result", "name": tc.Function.Name, "call_id": tc.ID, "truncated": false, "total_chars": 0, "content": syn})
				emit(types.AgentEvent{"type": "doom_stop", "detail": fmt.Sprintf("警告后仍重复完全相同调用 %s，强停本轮", tc.Function.Name)})
				stopped = true
				break
			}
			if sameRun && !warned {
				doomWarnPending = true // 先执行，整批结束后再警告
			}

			emit(types.AgentEvent{"type": "tool_call", "name": tc.Function.Name, "args": args, "call_id": tc.ID})
			execDone := make(chan string, 1)
			go func() {
				r, err := tools.ExecuteTool(tc.Function.Name, args)
				if err != nil {
					execDone <- tools.ToolFailPrefix + err.Error()
				} else {
					execDone <- r
				}
			}()
			// §7.3 前台赛跑：超时前完成 → 正常回填；超时 → 转后台，回填"已转后台"通知
			select {
			case outcome := <-execDone:
				raw := outcome
				totalChars := utf16Len(raw)
				truncated := false
				if totalChars > opt.ResultLimit {
					// 截断：全文落 log 文件，正文留前 resultLimit 字符 + 一句 log 路径提示（模型用 bash 抓回）
					logPath := a.writeToolLog(opt, raw)
					truncated = true
					raw = utf16Slice(raw, opt.ResultLimit) + fmt.Sprintf("\n[已截断：完整结果 %d 字符在 %s，用 bash 的 cat / grep / tail 查看。]", totalChars, logPath)
				}
				a.cm.AddMessage(&types.RawEntry{Role: "tool", ToolCallID: &tc.ID, Content: raw})
				// content = 截断后 agent 实际回填给模型的内容（超长时 = 前 resultLimit 字符 + log 路径提示）
				emit(types.AgentEvent{"type": "tool_result", "name": tc.Function.Name, "call_id": tc.ID, "truncated": truncated, "total_chars": totalChars, "content": raw})
			case <-time.After(time.Duration(opt.FgTimeoutMS) * time.Millisecond):
				// 转后台（不取消）：立即回填通知，模型不必傻等
				startedAt := time.Now().UnixMilli()
				bgNotice := fmt.Sprintf("（系统：此调用前台等待超过 %d 秒未完成，已自动转入后台继续执行。无需等待，可继续其他工作或直接基于已有信息回复；任务完成后系统会自动通知结果。）", roundMS(opt.FgTimeoutMS/1000))
				a.cm.AddMessage(&types.RawEntry{Role: "tool", ToolCallID: &tc.ID, Content: bgNotice})
				emit(types.AgentEvent{"type": "tool_result", "name": tc.Function.Name, "call_id": tc.ID, "truncated": false, "total_chars": 0, "backgrounded": true, "content": bgNotice})
				emit(types.AgentEvent{"type": "bg_start", "call_id": tc.ID, "tool_name": tc.Function.Name, "timeout_ms": opt.FgTimeoutMS})
				if opt.OnBackgroundStart != nil {
					opt.OnBackgroundStart(types.BackgroundStartArgs{
						CallID:    tc.ID,
						ToolName:  tc.Function.Name,
						Args:      args,
						Promise:   execDone,
						StartedAt: startedAt,
					})
				}
			}
		}

		// 轮内搭车（2026-09-14 统一）：本批工具结果之后、本次 LLM 调用之前，取走
		// 运行期间入队的全部消息——用户追加消息与已 settle 后台任务的通知（同一
		// 消息队列、同一逻辑），同车注入（纯追加：不加 LLM 调用、不加 KV 缓存代价
		// ——已缓存前缀不变）。后台通知自带"（系统通知：…）"标记，不加【用户追加】
		// 前缀；用户消息加前缀——模型明确知道是"追加指令"而非"新任务"（2026-09-09）。
		if opt.TakePendingUsers != nil {
			for _, m := range opt.TakePendingUsers() {
				if strings.HasPrefix(m, bgNoticePrefix) {
					a.cm.AddMessage(&types.RawEntry{Role: "user", Content: m})
				} else {
					a.cm.AddMessage(&types.RawEntry{Role: "user", Content: "【用户追加】" + m})
				}
				emit(types.AgentEvent{"type": "user_piggyback", "message": m})
			}
		}

		a.mu.Lock()
		needWarn := !stopped && doomWarnPending
		var warnName string
		if needWarn && len(a.lastCalls) > 0 {
			a.warned = true
			warnName = a.lastCalls[len(a.lastCalls)-1].Name
		}
		sequenceBroken := !a.isSameRunLocked(opt.DoomWarnAfter)
		a.mu.Unlock()
		if needWarn {
			warn := fmt.Sprintf("（系统警告：你已连续 %d 次用完全相同的参数调用 %s。请改变策略：换工具、换参数，或基于已有信息直接回答。）", opt.DoomWarnAfter, warnName)
			a.cm.AddMessage(&types.RawEntry{Role: "user", Content: warn})
			emit(types.AgentEvent{"type": "doom_warn", "detail": fmt.Sprintf("连续 %d 次完全相同调用 %s", opt.DoomWarnAfter, warnName)})
		}
		if sequenceBroken {
			a.mu.Lock()
			a.warned = false
			a.mu.Unlock()
		}

		if stopped {
			stopMsg := "（系统：检测到重复工具调用，已强制停止本轮。请基于已有信息直接回答用户。）"
			a.cm.AddMessage(&types.RawEntry{Role: "user", Content: stopMsg})
			// 强停后再给模型一次机会收尾
			tail, err := a.llmCall(ctx, emit, totals)
			if err != nil {
				return a.finishRunError(err, ctx, emit, totals)
			}
			return a.finishText(tail, emit, totals)
		}

		// 下一轮 LLM 调用
		next, err := a.llmCall(ctx, emit, totals)
		if err != nil {
			return a.finishRunError(err, ctx, emit, totals)
		}
		if len(next.ToolCalls) == 0 {
			return a.finishText(next, emit, totals)
		}
		resp = next
	}
}

// isSameRunLocked §8：最近 doomWarnAfter 次调用是否完全相同（持锁调用；
// 工具名+参数，稳定序列化比较）
func (a *Agent) isSameRunLocked(n int) bool {
	if len(a.lastCalls) < n {
		return false
	}
	tail := a.lastCalls[len(a.lastCalls)-n:]
	first := tail[0].Key
	for _, c := range tail {
		if c.Key != first {
			return false
		}
	}
	return true
}

// ---------- 辅助 ----------

// parseArgs arguments JSON 字符串 → 对象；解析失败兜底为原始字符串
func parseArgs(s string) map[string]interface{} {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		if m, ok := v.(map[string]interface{}); ok {
			return m
		}
	}
	return map[string]interface{}{"_raw": s}
}

// stableKey 稳定序列化（Go map JSON 序列化自动键排序）→ 逐字符比较（§8 参数比较口径）
func stableKey(name string, args map[string]interface{}) string {
	b, err := json.Marshal(args)
	if err != nil {
		return name + sJSON(args)
	}
	return name + string(b)
}

func sJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func roundMS(x int) int {
	return int(float64(x) + 0.5)
}

// utf16Len 字符数（UTF-16 code units，对齐 TS text.length 口径）
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

// utf16Slice 按 UTF-16 code units 截断（对齐 TS str.slice(0, n)）
func utf16Slice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i, r := range s {
		units := 1
		if r >= 0x10000 {
			units = 2
		}
		if count+units > n {
			return s[:i]
		}
		count += units
	}
	return s
}
