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
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/inject"
	"okhuman/internal/llm"
	"okhuman/internal/tools"
	"okhuman/internal/types"
)

// RunOptions 一轮 run 的参数
type RunOptions struct {
	// 落盘根目录：截断工具结果写 <dataDir>/tool-results/<时间戳>.log
	DataDir string
	// 工具结果回填上限（字符，可配置，默认 50000）：超出截断 + 全文落文件
	ResultLimit   int
	DoomWarnAfter int
	// 前台执行超时（毫秒）：超时不取消，转后台 + 双向通知（§7.3）
	FgTimeoutMS int
	// 工具轮次上限（2026-10-05）：一轮 run 最多跑多少批工具调用。
	// §8 死循环检测只认"完全相同"的调用——模型每次微调参数（时间戳、line+1）
	// 就能永久绕开，所以需要次数兜底。触达后注入收尾提示并给模型最后一次作答
	// 机会（与强停同一收尾方式，不硬 return）。0/负 = defaultMaxToolRounds。
	MaxToolRounds int
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

// injectFallbackMode 4xx 回退阶梯的模式（2026-10-05 提为类型化常量）。
// 原实现在"组装降级请求"和"回写降级侧车"两处各写一份字符串字面量，一次拼写
// 漂移（一边 "newest"、一边 "demote-newest"）让 demote-newest 长期退化成
// demote-all——把会话里全部附件永久降级。同一个模式值只允许有一处定义。
type injectFallbackMode string

const (
	fbDemoteNewest injectFallbackMode = "demote-newest"
	fbDemoteAll    injectFallbackMode = "demote-all"
	fbTruncate     injectFallbackMode = "truncate"
)

// defaultMaxToolRounds 未显式配置时的工具轮次上限（见 RunOptions.MaxToolRounds）
const defaultMaxToolRounds = 100

// stoppedPlaceholder /stop 中断且尚无增量流出时落会话的文本；Run 的返回值
// 与之同源（2026-10-05 修：旧实现会话里存占位、返回空串，POST /chat 的 reply
// 字段会拿到空值）
const stoppedPlaceholder = "（已中断：用户手动停止）"

// strongStopNoAnswer 强停收尾后模型仍未给出正文时的兜底回复
// （2026-10-05：旧实现原样返回空串 → 前端出现空气泡）
const strongStopNoAnswer = "（本轮已因重复工具调用被强制停止，模型未给出最终回答。请参考上方工具结果。）"

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
	injectDir     string // 注入侧车目录（4xx 降级成功回写用，2026-09-19；""=不回写）
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
func New(client llm.LlmClient, systemPrompt string, cm *ctxmgr.Manager, injectDir string) *Agent {
	return &Agent{llm: client, systemPrompt: systemPrompt, cm: cm, injectDir: injectDir}
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
			"scope":        e.Scope,
			"detail":       e.Detail,
			"before_chars": e.BeforeChars,
			"after_chars":  e.AfterChars,
			"rounds":       e.Rounds,
			"text":         e.Text,
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
	// 异常截断（2026-09-15）：有 thinking 但无正文也无工具调用 → 不是"纯文本回复"：提示模型 + 重试一次
	if isTruncated(resp) {
		r2, err2 := a.retryTruncation(runCtx, emit, &totals)
		if err2 != nil {
			return a.finishRunError(err2, runCtx, emit, &totals)
		}
		resp = r2
	}
	// 纯文本回复 → 本轮结束
	if len(resp.ToolCalls) == 0 {
		return a.finishText(resp, emit, &totals)
	}
	// 注意：必须等 runLoop 完成后再 return（Go 无 JS 的 return-promise 陷阱，
	// defer 顺序天然正确）
	return a.runLoop(runCtx, resp, emit, &totals, opt)
}

// isTruncated 异常截断判定（2026-09-15）：响应有 thinking 但无正文也无工具调用。
//  服务端提前收笔（Q2 量化提前 stop token / MTP 投机解码异常）——OkHuman 请求不发
//  max_tokens（纯协议层），客户端没有截断理由，流是干净结束的 HTTP 200。
func isTruncated(resp *types.Response) bool {
	return len(resp.ToolCalls) == 0 &&
		strings.TrimSpace(resp.Content) == "" &&
		resp.ReasoningContent != nil &&
		strings.TrimSpace(*resp.ReasoningContent) != ""
}

// retryTruncation 异常截断处置：WebUI 事件 + 向模型注入提示 + 重试一次 llmCall。
//  只重试一次（防死循环）；重试仍截断 → emit 放弃事件后原样返回。
func (a *Agent) retryTruncation(ctx context.Context, emit func(types.AgentEvent), totals *TokenTotals) (*types.Response, error) {
	emit(types.AgentEvent{"type": "trunc_warn", "detail": "有 thinking 但无正文也无工具调用 → 异常截断，提示模型重发"})
	a.cm.AddMessage(&types.RawEntry{Role: "user", Content: "（系统提示：你上一条响应有思考内容，但未输出正文或工具调用，判定为异常截断。请基于当前上下文重新输出完整正文或工具调用。）"})
	resp, err := a.llmCall(ctx, emit, totals)
	if err != nil {
		return nil, err
	}
	if isTruncated(resp) {
		emit(types.AgentEvent{"type": "trunc_warn", "detail": "重试后仍无正文或工具调用 → 本轮放弃（请查 LLM 服务端日志的 stop 原因）"})
	}
	return resp, nil
}

// finishRunError runUserMessage 顶层错误收尾：AgentStoppedError → partial 收尾；
// runCtx 已取消（压缩流被 /stop 中断等）→ 空 partial 收尾；其他 → 上抛
func (a *Agent) finishRunError(err error, runCtx context.Context, emit func(types.AgentEvent), totals *TokenTotals) (string, error) {
	var s *AgentStoppedError
	if errors.As(err, &s) {
		// 返回值与落会话的文本同源（不给 ""，否则 HTTP /chat 的 reply 拿到空值）
		return a.finishStopped(s.Partial, emit, totals), nil
	}
	if runCtx.Err() != nil {
		// 压缩流被 /stop 中断（非 AgentStoppedError）→ 按"用户停止"收尾（空 partial），
		// 不当系统错误抛（否则 drain 会广播 error 卡）
		return a.finishStopped(PartialContent{}, emit, totals), nil
	}
	return "", err
}

// finishStopped /stop 中断收尾：partial 内容存 assistant 消息，发 stopped 事件。
// partial 正文已由 delta 逐块流出（流式），不重发全量。
// 返回值 = 实际写入会话的正文（partial 为空时用占位文案），调用方原样返回即可。
func (a *Agent) finishStopped(partial PartialContent, emit func(types.AgentEvent), totals *TokenTotals) string {
	body := partial.Text
	if body == "" {
		body = stoppedPlaceholder
	}
	var reasoning *string
	if partial.Thinking != "" {
		rc := partial.Thinking
		reasoning = &rc
	}
	a.cm.AddMessage(&types.RawEntry{Role: "assistant", Content: body, ReasoningContent: reasoning})
	emit(types.AgentEvent{"type": "stopped"})
	emit(types.AgentEvent{"type": "done", "usage": totals})
	return body
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
		// 截断 + 全文落盘（失败时不给假路径，见 formatTruncatedWithHead）
		return a.formatTruncatedWithHead(opt, result, opt.ResultLimit, head, "")
	}
	return head + result
}

// toolLogSeq 工具结果文件名序号（2026-09-14 审计修：旧文件名仅秒精度，同一秒
// 两次截断——后台通知（独立 goroutine）与轮内结果可并发——撞名互相覆盖，
// 前一条消息里的 log 路径会指向后一条的内容）
var toolLogSeq atomic.Uint64

// writeToolLog 被截断的工具结果全文落文件
// （<dataDir>/tool-results/tool-<时间戳>-<序号>.log），返回绝对路径。
// 文件名秒精度 + 单调序号（tool- 前缀，与 session-records 的 ctx- 区分）。
//
// 失败时返回 ok=false（**不再返回一个"看似路径"的失败标记字符串**——
// 旧实现返回 "[文件写入失败：写入失败]"，被调用方无条件当路径拼进提示，
// 导致模型去 cat 一个不存在的路径且不知全文已丢。见 writeToolLogOK 注释。
func (a *Agent) writeToolLog(opt RunOptions, full string) string {
	path, _ := a.writeToolLogOK(opt, full)
	return path
}

// writeToolLogOK writeToolLog 的显式成败版：成功 → (绝对路径, true)；
// 失败 → (不含路径的说明, false)。调用方据此决定提示文案。
func (a *Agent) writeToolLogOK(opt RunOptions, full string) (string, bool) {
	dir := filepath.Join(opt.DataDir, "tool-results")
	ts := time.Now().UTC().Format("2006-01-02_15-04-05")
	path := filepath.Join(dir, fmt.Sprintf("tool-%s-%d.log", ts, toolLogSeq.Add(1)))
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if err := os.WriteFile(path, []byte(full), 0o644); err == nil {
			return path, true
		}
	}
	return "[落盘失败]", false
}

// quoteHintPath 把路径用反引号包裹，供模型安全复制（路径可能含空格/中文，
// 裸拼 `cat <path>` 会因空格分词失败——Windows 用户名常见 "John Doe"）。
func quoteHintPath(p string) string {
	return "`" + p + "`"
}

// formatTruncatedForModel 生成"截断后回填给模型"的正文：
// 成功落盘 → 前 limit 字符 + 含【引号包裹路径】的抓回指引；
// 落盘失败 → 前 limit 字符 + 如实告知"全文已不可找回"（**不给假路径**）。
//
// head/tail 预算：调用方可传入 head 占用（后台通知场景），确保总长贴近 limit。
func (a *Agent) formatTruncatedForModel(opt RunOptions, result string, limit int) string {
	return a.formatTruncatedWithHead(opt, result, limit, "", "")
}

// formatTruncatedWithHead 带 head/tail 保留的通用实现。
//   - head：截断区之前必须保留的前缀（如后台通知的状态行），参与预算；
//   - tailExtra：截断提示之外必须追加的固定尾部（通常为空）。
func (a *Agent) formatTruncatedWithHead(opt RunOptions, result string, limit int, head, tailExtra string) string {
	total := utf16Len(result)
	logPath, ok := a.writeToolLogOK(opt, result)
	var tail string
	if ok {
		tail = fmt.Sprintf("\n[已截断：完整结果 %d 字符在 %s，用 bash 的 cat / grep / tail 查看。]",
			total, quoteHintPath(logPath))
	} else {
		// 落盘失败：不给假路径，如实告知全文不可找回，模型据此自行决定是否重跑。
		tail = fmt.Sprintf("\n[已截断：完整结果 %d 字符（全文落盘失败，无法找回，仅保留前部）。]",
			total)
	}
	budget := limit - utf16Len(head) - utf16Len(tail) - utf16Len(tailExtra)
	if budget < 0 {
		budget = 0
	}
	return head + utf16Slice(result, budget) + tail + tailExtra
}

// llmCall 一次 LLM 调用（记 token 统计）。流式：delta 实时 emit（WebUI 边生成边显示）；
// /stop 时 cancel → fetch 断开 → llama.cpp 取消生成；本方法把已收的增量包成
// AgentStoppedError 抛出（由 runUserMessage 收尾）。
//
// LLM 4xx 三级回退阶梯（2026-09-13，用户拍板"粗暴版"）：
// ① 正常请求 → ② 4xx：最近一条带附件的消息降级文本提示重试 → ③ 仍 4xx：全部附件降级重试
// → ④ 仍 4xx：触发兜底硬截断压缩后重试（再败则抛，轮次死，人工介入）。
// 降级是永久的（用户设计意图）：降级成功即回写侧车为文本提示，被拒附件不恢复（2026-09-19 修，原实现是请求级、下轮原样恢复→持久坏附件每轮复发）；触发限 400/413/415
// （请求体被拒），其余错误（5xx/401/404/网络错）终止阶梯原样抛（配置类错误不配吃硬截断）。
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
	// 2026-10-05 修：cancel 必须在【每条】返回路径上被调用（原实现只在成功路径
	// 清字段，stopped/非4xx/阶梯耗尽三条错误路径都漏了 cancel）。今天的上游 ctx
	// 是 runCtx、轮末必然被 cancel，泄漏有界；一旦哪天挂到长生命周期 ctx 上就是
	// 真泄漏。用 defer 彻底免掉这件事（注意：这两个 defer 靠 LIFO 顺序，先清字段）。
	defer func() {
		a.mu.Lock()
		a.callCancel = nil
		a.mu.Unlock()
	}()
	defer callCancel()

	partial := PartialContent{}
	// rollbackPartial 阶梯重试前丢弃已累积的增量，并通知前端回滚本轮已渲染的内容
	// （2026-10-05：光重置变量救不了 UI——增量早已 emit 出去，屏幕上会留着"被丢弃的
	// 半截首流 + 重试全文"拼接出来的脏内容）
	rollbackPartial := func() {
		if partial == (PartialContent{}) {
			return
		}
		partial = PartialContent{}
		emit(types.AgentEvent{"type": "delta_reset"})
	}
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
		var modes []injectFallbackMode
		if a.sessionHasInject() {
			modes = []injectFallbackMode{fbDemoteNewest, fbDemoteAll, fbTruncate}
		} else {
			modes = []injectFallbackMode{fbTruncate}
		}
		for _, mode := range modes {
			action := "触发兜底硬截断压缩"
			var demoted []string
			switch mode {
			case fbDemoteNewest:
				action = "最近一条带附件的降级文本提示"
				messages, demoted = a.cm.ComposeWithDemotion(&ctxmgr.ComposeOptions{DemoteInjects: ctxmgr.DemoteNewest})
			case fbDemoteAll:
				action = "全部附件降级文本提示"
				messages, demoted = a.cm.ComposeWithDemotion(&ctxmgr.ComposeOptions{DemoteInjects: ctxmgr.DemoteAll})
			default:
				messages, demoted = a.cm.ComposeWithDemotion(nil)
			}
			fmt.Printf("[llm] LLM 4xx（%s）→ %s重试\n", truncateStr(firstErr.Error(), 200), action)
			if mode == fbTruncate {
				a.cm.ForceHardTruncate()
			}
			rollbackPartial()
			err2 := a.streamOnce(callCtx, client, messages, onDelta, &resp)
			if resp != nil {
				detail := truncateStr(firstErr.Error(), 300)
				fmt.Printf("[llm] 4xx 回退阶梯：%s 重试成功（首次报错：%s）\n", mode, detail)
				emit(types.AgentEvent{"type": "inject_fallback", "mode": string(mode), "detail": detail})
				// 2026-09-19：降级成功 → 回写侧车（治本）。降级本是请求级（不动侧车），
				// 对持久坏附件（内容损坏的图片，每次调用必 400）每轮都先 4xx 再降级；
				// 回写后侧车变文本提示，后续 resolve 不再发坏 part。
				// 2026-10-05：回写范围取 Compose 实际降级的 id（demoted），不再由本
				// 函数按"哪些消息带附件"重算——重算那版用错了 mode，把 demote-newest
				// 扩成了全量降级。
				if mode != fbTruncate {
					dir := a.injectDir
					for _, id := range demoted {
						if inject.PersistDemotion(dir, id) {
							ctxmgr.ClearResolverCacheEntry(id)
							fmt.Printf("[inject] %s 降级回写侧车（后续调用不再发原 part）\n", id)
						}
					}
				}
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
	r, err := client.CompleteStream(ctx, messages, tools.Specs(), onDelta, 0)
	if err == nil {
		*respOut = r
	}
	return err
}

// isLlm4xxErr LLM 4xx 判定（400/413/415 = 请求体被拒；与 TS 正则等价）。
// 用 errors.As 而非类型断言：上游一旦用 fmt.Errorf("…: %w", err) 包一层，
// 断言会漏判 → 4xx 阶梯被整体绕开。
func isLlm4xxErr(e error) bool {
	var h *llm.HTTPError
	if !errors.As(e, &h) {
		return false
	}
	return h.Is4xx()
}

// sessionHasInject 会话里是否存在附件引用（inject_ref）——4xx 阶梯只对"有附件"的会话降级
//
// 注（2026-10-05）：原先这里还有一个 demotedInjectIDs(mode)，由 agent 侧按同样的
// mode 字符串重算"哪些附件被降级"，供侧车回写使用。它与 Compose 里的降级判定是
// 同一件事的两份实现，两边字符串一漂移就出事（demote-newest 历史上退化成全量降级）。
// 现已删除：降级范围由 ComposeResult.DemotedInjectIDs 单点给出。
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

// maxToolRounds 本轮工具轮次上限（0/负 → defaultMaxToolRounds）
func maxToolRounds(opt RunOptions) int {
	if opt.MaxToolRounds > 0 {
		return opt.MaxToolRounds
	}
	return defaultMaxToolRounds
}

// runLoop 工具循环：从一次"带 tool_calls 的 LLM 响应"开始，逐个执行工具并回填，
// 直到模型返回纯文本（或强停后收尾）。
//
// 两道刹车（2026-10-05）：
//  1. §8 死循环检测：连续 N 次【完全相同】的调用 → 警告 → 再犯强停；
//  2. 轮次上限 MaxToolRounds：兜住"每次改一点点参数"（时间戳、line+1）的情况
//     ——那种写法 1 永远不触发，只能靠次数。触达后注入收尾提示并给模型最后一次
//     作答机会，不硬 return。
func (a *Agent) runLoop(ctx context.Context, resp *types.Response, emit func(types.AgentEvent), totals *TokenTotals, opt RunOptions) (string, error) {
	roundLimit := maxToolRounds(opt)
	for round := 1; ; round++ {
		if round > roundLimit {
			// 触达轮次上限：不再执行工具，给模型一次收尾机会（与强停同一收尾方式）
			limitMsg := fmt.Sprintf("（系统：本轮工具调用已达上限 %d 次，请停止调用工具，基于已有信息直接回答用户。）", roundLimit)
			a.cm.AddMessage(&types.RawEntry{Role: "user", Content: limitMsg})
			emit(types.AgentEvent{"type": "round_limit", "detail": fmt.Sprintf("工具调用轮次达上限 %d，收尾本轮", roundLimit)})
			return a.tailCall(ctx, emit, totals)
		}
		calls := resp.ToolCalls
		if len(calls) == 0 {
			return a.finishText(resp, emit, totals) // 防御：纯文本收尾
		}
		// 先存 assistant 消息（带 tool_calls，原样回传）
		a.cm.AddMessage(&types.RawEntry{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: calls})

		// 逐个执行（并行工具调用也按序回填，保持 tool 消息与 tool_calls 一一对应）
		// 死循环（§8）：连续 N 次完全相同 → 注入警告；警告后仍重复 → 强停。
		// 警告/强停消息都在本批【全部】tool 结果之后插入——本批每个 tool_call
		// 都必须有 tool 消息，否则严格校验的 LLM 端点会直接 400（2026-10-05 修）。
		stopped := false
		doomWarnPending := false
		for ci, tc := range calls {
			args := parseArgs(tc.Function.Arguments)
			a.mu.Lock()
			a.lastCalls = append(a.lastCalls, callRec{Name: tc.Function.Name, Key: stableKey(tc.Function.Name, args)})
			trimCallRecs(&a.lastCalls, opt.DoomWarnAfter) // 滑窗，见 trimCallRecs 注释
			sameRun := a.isSameRunLocked(opt.DoomWarnAfter)
			warned := a.warned
			a.mu.Unlock()

			if warned && sameRun {
				// 警告后仍重复 → 强停：本条与【本批剩余全部】调用都不执行，各补一条
				// 合成 tool 结果——少补任何一个都会让 assistant.tool_calls 失配。
				syn := "（此调用已被系统强停：连续完全相同的调用，见随后系统消息）"
				for j := ci; j < len(calls); j++ {
					c2 := calls[j]
					a.cm.AddMessage(&types.RawEntry{Role: "tool", ToolCallID: &c2.ID, Content: syn})
					emit(types.AgentEvent{"type": "tool_result", "name": c2.Function.Name, "call_id": c2.ID, "truncated": false, "total_chars": 0, "content": syn})
				}
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
			// §7.3 前台赛跑：超时前完成 → 正常回填；超时 → 转后台，回填"已转后台"通知。
			// 定时器用 Stop 收尾即可（2026-10-05）：Go 1.23+ 起定时器通道是无缓冲的，
			// 已触发且无人接收时那个值直接被丢弃 → 通道里永远没有"需要排出的旧值"，
			// 再写 `<-timer.C` 排空反而会永久阻塞（goroutine 泄漏）。
			fgTimer := time.NewTimer(time.Duration(opt.FgTimeoutMS) * time.Millisecond)
			select {
			case outcome := <-execDone:
				fgTimer.Stop()
				raw := outcome
				totalChars := utf16Len(raw)
				truncated := false
				if totalChars > opt.ResultLimit {
					// 截断：全文落 log 文件（失败则如实说明），正文留前 resultLimit 字符 +
					// 抓回指引（路径用反引号包裹，模型可安全复制）。
					raw = a.formatTruncatedForModel(opt, raw, opt.ResultLimit)
					truncated = true
				}
				a.cm.AddMessage(&types.RawEntry{Role: "tool", ToolCallID: &tc.ID, Content: raw})
				// content = 截断后 agent 实际回填给模型的内容（超长时 = 前 resultLimit 字符 + log 路径提示）
				emit(types.AgentEvent{"type": "tool_result", "name": tc.Function.Name, "call_id": tc.ID, "truncated": truncated, "total_chars": totalChars, "content": raw})
			case <-fgTimer.C:
				// 转后台（不取消）：立即回填通知，模型不必傻等
				startedAt := time.Now().UnixMilli()
				bgNotice := fgTimeoutNotice(opt.FgTimeoutMS)
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
			return a.tailCall(ctx, emit, totals)
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

// tailCall 收尾调用：给模型最后一次作答机会，并且【不再执行任何工具】
// （2026-10-05：强停/触达轮次上限的目的就是停止工具循环——再跑一轮工具等于
// 绕开刚做的决定）。若模型这次仍然只给 tool_calls、没有正文，原来的实现会
// 原样返回空串 → 前端出现空气泡，这里兜一句明确文案。
func (a *Agent) tailCall(ctx context.Context, emit func(types.AgentEvent), totals *TokenTotals) (string, error) {
	tail, err := a.llmCall(ctx, emit, totals)
	if err != nil {
		return a.finishRunError(err, ctx, emit, totals)
	}
	if len(tail.ToolCalls) > 0 {
		emit(types.AgentEvent{"type": "doom_tail_pending", "detail": "收尾调用仍返回工具调用，已丢弃（本轮不再执行工具）"})
		if strings.TrimSpace(tail.Content) == "" {
			tail.Content = strongStopNoAnswer
		}
	}
	return a.finishText(tail, emit, totals)
}

// trimCallRecs lastCalls 滑窗（2026-10-05）：run 内每个工具调用都追加一条记录，
// 而 callRec.Key 里存的是"工具名 + 参数的完整 JSON"——bash 的 command 动辄几 KB，
// 长任务会无限堆积。§8 判定只看尾部 doomWarnAfter 条，因此保留 DoomWarnAfter+1
// 条就够（多留一条用于判断"连续序列是否刚被打断"）。
func trimCallRecs(rc *[]callRec, doomWarnAfter int) {
	if doomWarnAfter <= 0 || len(*rc) <= doomWarnAfter+1 {
		return
	}
	keep := (*rc)[len(*rc)-(doomWarnAfter+1):]
	*rc = append([]callRec(nil), keep...)
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

// fgTimeoutNotice 前台等待超时后回填给模型的通知（2026-10-05）。
// 秒数取一位小数并【向下取整】：命中的目的是告诉模型"等了多久"，向上取整会把
// 1.9 秒说成"超过 2 秒"——那是句假话；0.999 秒也只能说"超过 0.9 秒"。
func fgTimeoutNotice(fgTimeoutMS int) string {
	sec := float64(fgTimeoutMS) / 1000
	if sec < 0 {
		sec = 0
	}
	sec = math.Floor(sec*10) / 10
	return fmt.Sprintf("（系统：此调用前台等待超过 %.1f 秒未完成，已自动转入后台继续执行。无需等待，可继续其他工作或直接基于已有信息回复；任务完成后系统会自动通知结果。）", sec)
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
