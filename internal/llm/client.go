package llm

// OpenAI 兼容 LLM 客户端（llama.cpp /v1/chat/completions）。
// 采样参数（temperature / max_tokens / enable_thinking）一律不发——由
// llama-server 启动参数预设，请求级下发会改模板渲染破 KV 缓存（A/B 实测坐实）。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"okhuman/internal/types"
)

// ClientConfig 客户端构造参数
type ClientConfig struct {
	BaseURL   string
	Model     string
	APIKey    string
	TimeoutMS int // 整体请求超时（complete 非流式用；流式另有空闲超时）
	// StreamIdleMS 主循环流式空闲超时（2026-10-18 A5，来自 llm.stream_idle_sec）：
	// CompleteStream 传 idleMs=0 时用这个值；**未配置或 ≤0 时仍回落到 max(llm.timeout_sec, 10 分钟)**
	// （见 idleFallback）。default.json 不含 stream_idle_sec，所以回落就是默认生效路径。。
	StreamIdleMS int
	// MaxResponseBytes 单次响应总量上限（字节，0 → DefaultMaxResponseBytes）
	MaxResponseBytes int64
}

// OpenAiClient 流式/非流式双方法
type OpenAiClient struct {
	cfg  ClientConfig
	http *http.Client
	logf func(string) // 可观测事件（畸形帧等）落日志；未注入则只计数不打印
}

// SetLogger 注入日志通道（畸形帧等协议层异常需要留痕，否则"回复少了一截"无从查起）。
func (c *OpenAiClient) SetLogger(fn func(string)) { c.logf = fn }

func NewOpenAiClient(cfg ClientConfig) *OpenAiClient {
	return &OpenAiClient{cfg: cfg, http: &http.Client{}}
}

// ---------- 响应体量闸（2026-10-18 健壮性修 A） ----------
//
// 闸放在 reader 层：正文/思考/工具参数/SSE 单行的所有累加都源自读到的字节，
// 一处计数即覆盖全部累加点，不必逐个检查。

// DefaultMaxResponseBytes 未配置时的响应总量上限（256MB，与 HTTP 请求体同档）
const DefaultMaxResponseBytes int64 = 256 << 20

// maxToolCallSlots 单次响应 tool_calls 槽位上限：总量闸管的是字节，海量小帧
// 仍能把按 index 建的槽位表撑大（index 由上游给），故单独设条目闸。
const maxToolCallSlots = 4096

// ResponseTooLargeError LLM 响应超过上限 → 本轮终止。独立错误类型：不会被
// 压缩路径"只重试 IdleTimeoutError"的逻辑误判成可重试。
type ResponseTooLargeError struct {
	LimitBytes int64
	GotBytes   int64
	What       string // "body" | "tool_call_slots"
}

func (e *ResponseTooLargeError) Error() string {
	if e.What == "tool_call_slots" {
		return fmt.Sprintf("LLM 响应 tool_calls 槽位超过上限 %d（已见 %d）→ 本轮终止", e.LimitBytes, e.GotBytes)
	}
	return fmt.Sprintf("LLM 响应体量超过上限 %d 字节（已读 %d）→ 本轮终止", e.LimitBytes, e.GotBytes)
}

// cappedReader 计数并在超过上限时报错
type cappedReader struct {
	r   io.Reader
	max int64
	n   int64
}

func (r *cappedReader) over() bool { return r.n > r.max }

func (r *cappedReader) overErr() error {
	return &ResponseTooLargeError{LimitBytes: r.max, GotBytes: r.n, What: "body"}
}

func (r *cappedReader) Read(p []byte) (int, error) {
	if r.over() {
		return 0, r.overErr()
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.over() {
		return n, r.overErr()
	}
	return n, err
}

func (c *OpenAiClient) maxRespBytes() int64 {
	if c.cfg.MaxResponseBytes > 0 {
		return c.cfg.MaxResponseBytes
	}
	return DefaultMaxResponseBytes
}

// ---------- 错误类型 ----------
//
// 2026-10-18：agent 侧的"LLM 4xx 三级回退阶梯"已整体删除（按状态码猜附件问题
// 是按 llama.cpp 行为写死的，换后端就不成立）。现在 LLM 报错原样上抛、本轮终止，
// 附件处置由人显式做（POST /strip-injects 永久回写侧车）。Status 字段保留——
// 排障要看状态码，但已没有任何按状态码分支的控制流。

// HTTPError LLM 返回非 2xx。Error() 串格式 "LLM <status>: <body前500>"
// （与 TS 版一致，日志口径不变）
// MalformedStreamError 流式响应里畸形帧过多（2026-10-18 复查修 2）：少量畸形帧
// （代理截断、半行、非标准实现）记日志 + 计数后继续；多到这个量级说明上游在持续
// 吐我们读不懂的东西，继续跑只会得到一条"少了不知道多少"的回复 → 明确报错。
type MalformedStreamError struct {
	Count int
	Bytes int
}

func (e *MalformedStreamError) Error() string {
	return fmt.Sprintf("流式响应畸形帧过多（%d 帧 / %d 字节解析失败）", e.Count, e.Bytes)
}

const (
	maxMalformedFrames = 200 // 超过即判定上游异常，终止本轮
	malformedLogLimit  = 5   // 明细日志最多打几条，防刷屏
)

func snippetForLog(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	b := e.Body
	if len(b) > 500 {
		b = b[:500]
	}
	return fmt.Sprintf("LLM %d: %s", e.Status, b)
}

// IdleTimeoutError 流式空闲超时（无 wire 字节超过 idleMs，含 ": ping" 注释帧）。
// 压缩路径据此重试（最多 3 次），3 次仍无响应 → 硬截断兜底。
type IdleTimeoutError struct {
	IdleMS int
	Cause  error
}

func (e *IdleTimeoutError) Error() string {
	return fmt.Sprintf("LLM 流式空闲超时（%dms 无 wire 字节）：%v", e.IdleMS, e.Cause)
}

// ---------- 请求体组装 ----------

func wrapTools(tools []types.ToolSpec) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]interface{}{"type": "function", "function": t})
	}
	return out
}

func (c *OpenAiClient) buildBody(messages []types.Message, tools []types.ToolSpec, stream bool) ([]byte, error) {
	body := map[string]interface{}{
		"model":    c.cfg.Model,
		"messages": messages,
		"stream":   stream,
	}
	if stream {
		body["stream_options"] = map[string]interface{}{"include_usage": true}
	}
	if len(tools) > 0 {
		body["tools"] = wrapTools(tools)
		body["tool_choice"] = "auto"
	}
	return json.Marshal(body)
}

func (c *OpenAiClient) newReq(ctx context.Context, data []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.BaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+c.cfg.APIKey)
	}
	return req, nil
}

func newCallID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源失败不能静默：全零 id 会让同一轮的多个 tool_call 互撞（应答错配）
		return fmt.Sprintf("call_fb%x", time.Now().UnixNano())
	}
	return "call_" + hex.EncodeToString(b[:])
}

type wireFunction struct {
	Name      *string `json:"name"`
	Arguments *string `json:"arguments"`
}

type usageDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// ---------- 流式（主循环用） ----------

// sseFrame llama.cpp SSE 帧（choices 为空 + usage = 尾块）
// wireUsage / wireTimings：非流式响应与 SSE 尾块里这两段 JSON 是同一个形状，
// 此前各自匿名声明一遍，折成 types.Usage 的代码也各写一遍（缓存命中数的优先级
// 一改就只改一边，2026-11-12 归并）。
type wireUsage struct {
	PromptTokens        int           `json:"prompt_tokens"`
	CompletionTokens    int           `json:"completion_tokens"`
	PromptTokensDetails *usageDetails `json:"prompt_tokens_details"`
}

type wireTimings struct {
	CacheN int `json:"cache_n"`
}

// usageOf 把 wire 上的 usage 折成 types.Usage。缓存命中优先取
// usage.prompt_tokens_details.cached_tokens（OpenAI 形状），没有才退 llama.cpp 的
// timings.cache_n —— 两条服务给的不是同一个字段，这个优先级是有意的，别改。
func usageOf(u *wireUsage, t *wireTimings) types.Usage {
	cached := 0
	if u != nil && u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	} else if t != nil {
		cached = t.CacheN
	}
	return types.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		CachedTokens:     cached,
	}
}

type sseFrame struct {
	Usage   *wireUsage   `json:"usage"`
	Timings *wireTimings `json:"timings"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			ReasoningContent *string             `json:"reasoning_content"`
			Content          *string             `json:"content"`
			ToolCalls        []wireToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

type wireToolCallDelta struct {
	Index    *int          `json:"index"`
	ID       *string       `json:"id"`
	Function *wireFunction `json:"function"`
}

type tcBuilder struct {
	ID   string
	Name string
	Args string
}

// idleFallback idleMs=0（主循环）时的空闲超时口径：
// 优先 llm.stream_idle_sec（配置，WebUI 可改）；未配/非正 → 旧行为 max(timeout_ms, 10min)。
func (c *OpenAiClient) idleFallback() time.Duration {
	if c.cfg.StreamIdleMS > 0 {
		return time.Duration(c.cfg.StreamIdleMS) * time.Millisecond
	}
	return time.Duration(math.Max(float64(c.cfg.TimeoutMS), 600000)) * time.Millisecond
}

func (c *OpenAiClient) CompleteStream(ctx context.Context, messages []types.Message, tools []types.ToolSpec, onDelta func(types.Delta), idleMs int) (*types.Response, error) {
	// 测试里发现的真隐患：processFrame 无条件调 onDelta，调用方传 nil 就 panic
	//（生产里 agent 总传闭包所以没炸过）。归一化一次，下游不必处处防。
	if onDelta == nil {
		onDelta = func(types.Delta) {}
	}
	data, err := c.buildBody(messages, tools, true)
	if err != nil {
		return nil, err
	}
	// 空闲超时：每 wire 块重置。缺省 idleMs=0 → 取客户端配置的 stream_idle_sec
	//（主循环口径，2026-10-18 A5；未配则回落 max(timeout_ms, 10 分钟)）；
	// 压缩路径显式传 context.stream_idle_sec。
	idleTimeout := time.Duration(idleMs) * time.Millisecond
	if idleMs == 0 {
		idleTimeout = c.idleFallback()
	}
	// /stop（父 ctx）与空闲超时（idleTimer）两路独立 cancel 源
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idleFired int32
	var idleTimer *time.Timer
	resetIdle := func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
		idleTimer = time.AfterFunc(idleTimeout, func() {
			atomic.StoreInt32(&idleFired, 1)
			cancel()
		})
	}
	resetIdle()
	defer func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
	}()

	req, err := c.newReq(reqCtx, data)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if atomic.LoadInt32(&idleFired) == 1 {
			return nil, &IdleTimeoutError{IdleMS: int(idleTimeout / time.Millisecond), Cause: err}
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 10000))
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}

	content := ""
	reasoning := ""
	badFrames, badBytes := 0, 0
	finishReason := new(string)
	usage := types.Usage{}
	tcAcc := map[int]*tcBuilder{}
	var tcOrder []int

	br := bufio.NewReaderSize(&cappedReader{r: resp.Body, max: c.maxRespBytes()}, 1<<16)
	for {
		line, rerr := br.ReadString('\n')
		if n := len(line); n > 0 {
			resetIdle() // 有数据 = 活动，重置空闲计时（长生成不被绝对超时杀）
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(line[5:])
				if payload != "[DONE]" && payload != "" {
					var f sseFrame
					if json.Unmarshal([]byte(payload), &f) == nil {
						c.processFrame(&f, &content, &reasoning, finishReason, &usage, tcAcc, &tcOrder, onDelta)
						if len(tcAcc) > maxToolCallSlots {
							return nil, &ResponseTooLargeError{LimitBytes: maxToolCallSlots,
								GotBytes: int64(len(tcAcc)), What: "tool_call_slots"}
						}
					} else {
						// 畸形帧：旧实现是 if err == nil { 处理 }，失败侧什么都没有 →
						// 那一帧内容静默蒸发，回复短了一截却查不到原因。
						badFrames++
						badBytes += len(payload)
						if c.logf != nil && badFrames <= malformedLogLimit {
							c.logf(fmt.Sprintf("[llm] 畸形帧 #%d（%d 字节，解析失败）：%s", badFrames, len(payload), snippetForLog(payload)))
						}
						if badFrames >= maxMalformedFrames {
							if c.logf != nil {
								c.logf(fmt.Sprintf("[llm] 畸形帧已达 %d 帧 → 终止本轮（上游在持续吐读不懂的内容）", badFrames))
							}
							return nil, &MalformedStreamError{Count: badFrames, Bytes: badBytes}
						}
					}
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if atomic.LoadInt32(&idleFired) == 1 {
				return nil, &IdleTimeoutError{IdleMS: int(idleTimeout / time.Millisecond), Cause: rerr}
			}
			return nil, rerr
		}
	}

	toolCalls := make([]types.ToolCall, 0, len(tcOrder))
	for _, i := range tcOrder {
		t := tcAcc[i]
		if t.Name == "" {
			continue
		}
		id := t.ID
		if id == "" {
			id = newCallID()
		}
		args := t.Args
		if args == "" {
			args = "{}"
		}
		toolCalls = append(toolCalls, types.ToolCall{ID: id, Type: "function",
			Function: types.Function{Name: t.Name, Arguments: args}})
	}
	respOut := &types.Response{
		Content:      content,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
		BadFrames:    badFrames, // 少量畸形帧时照常返回，但把"丢了几帧"带出来
	}
	if reasoning != "" {
		respOut.ReasoningContent = &reasoning
	}
	return respOut, nil
}

func (c *OpenAiClient) processFrame(f *sseFrame, content, reasoning *string, finishReason *string, usage *types.Usage,
	tcAcc map[int]*tcBuilder, tcOrder *[]int, onDelta func(types.Delta)) {
	// usage 块（llama.cpp 放尾块，choices 为空）
	if f.Usage != nil {
		*usage = usageOf(f.Usage, f.Timings)
	}
	if len(f.Choices) == 0 {
		return
	}
	ch := f.Choices[0]
	if ch.FinishReason != nil {
		*finishReason = *ch.FinishReason
	}
	if d := ch.Delta.ReasoningContent; d != nil && *d != "" {
		*reasoning += *d
		onDelta(types.Delta{Thinking: *d})
	}
	if d := ch.Delta.Content; d != nil && *d != "" {
		*content += *d
		onDelta(types.Delta{Text: *d})
	}
	for _, tc := range ch.Delta.ToolCalls {
		i := 0
		if tc.Index != nil {
			i = *tc.Index
		}
		b := tcAcc[i]
		if b == nil {
			b = &tcBuilder{}
			tcAcc[i] = b
			*tcOrder = append(*tcOrder, i)
		}
		if tc.ID != nil && *tc.ID != "" {
			b.ID = *tc.ID
		}
		if tc.Function != nil {
			if tc.Function.Name != nil {
				b.Name += *tc.Function.Name
			}
			if tc.Function.Arguments != nil {
				b.Args += *tc.Function.Arguments
			}
		}
	}
}
