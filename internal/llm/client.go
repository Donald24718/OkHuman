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
	BaseURL  string
	Model    string
	APIKey   string
	TimeoutMS int // 整体请求超时（complete 非流式用；流式另有空闲超时）
}

// OpenAiClient 流式/非流式双方法
type OpenAiClient struct {
	cfg  ClientConfig
	http *http.Client
}

func NewOpenAiClient(cfg ClientConfig) *OpenAiClient {
	return &OpenAiClient{cfg: cfg, http: &http.Client{}}
}

// Config 返回当前构造参数（热更新比对用）
func (c *OpenAiClient) Config() ClientConfig { return c.cfg }

// ---------- 错误类型（agent 4xx 阶梯按 HTTPError.Status 判定） ----------

// HTTPError LLM 返回非 2xx。Error() 串格式 "LLM <status>: <body前500>"
//（与 TS 版完全一致，日志/阶梯正则口径不变）
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

// Is4xx 回退阶梯触发条件：400/413/415（请求体被拒）
func (e *HTTPError) Is4xx() bool {
	return e.Status == 400 || e.Status == 413 || e.Status == 415
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
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// ---------- 非流式（压缩请求用） ----------

type wireMessage struct {
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

type wireToolCall struct {
	ID       *string      `json:"id"`
	Function *wireFunction `json:"function"`
}

type wireFunction struct {
	Name      *string `json:"name"`
	Arguments *string `json:"arguments"`
}

type usageDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

func (c *OpenAiClient) Complete(ctx context.Context, messages []types.Message, tools []types.ToolSpec) (*types.Response, error) {
	data, err := c.buildBody(messages, tools, false)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := c.newReq(cctx, data)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 10000))
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	var j struct {
		Choices []struct {
			FinishReason *string     `json:"finish_reason"`
			Message      *wireMessage `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int           `json:"prompt_tokens"`
			CompletionTokens int           `json:"completion_tokens"`
			PromptTokensDetails *usageDetails `json:"prompt_tokens_details"`
		} `json:"usage"`
		Timings *struct {
			CacheN int `json:"cache_n"`
		} `json:"timings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil, fmt.Errorf("LLM 响应 JSON 解析失败：%v", err)
	}
	respOut := &types.Response{}
	if j.Usage != nil {
		cached := 0
		if j.Usage.PromptTokensDetails != nil {
			cached = j.Usage.PromptTokensDetails.CachedTokens
		} else if j.Timings != nil {
			cached = j.Timings.CacheN
		}
		respOut.Usage = types.Usage{
			PromptTokens:     j.Usage.PromptTokens,
			CompletionTokens: j.Usage.CompletionTokens,
			CachedTokens:     cached,
		}
	}
	if len(j.Choices) > 0 {
		respOut.FinishReason = j.Choices[0].FinishReason
		if m := j.Choices[0].Message; m != nil {
			if m.Content != nil {
				respOut.Content = *m.Content
			}
			respOut.ReasoningContent = m.ReasoningContent
			var tcs []types.ToolCall
			for _, tc := range m.ToolCalls {
				if tc.Function == nil || tc.Function.Name == nil {
					continue
			}
				id := newCallID()
				if tc.ID != nil && *tc.ID != "" {
					id = *tc.ID
				}
				args := "{}"
				if tc.Function.Arguments != nil {
					args = *tc.Function.Arguments
				}
				tcs = append(tcs, types.ToolCall{ID: id, Type: "function",
					Function: types.Function{Name: *tc.Function.Name, Arguments: args}})
			}
			if len(tcs) > 0 {
				respOut.ToolCalls = tcs
			}
		}
	}
	return respOut, nil
}

// ---------- 流式（主循环用） ----------

// sseFrame llama.cpp SSE 帧（choices 为空 + usage = 尾块）
type sseFrame struct {
	Usage *struct {
		PromptTokens      int            `json:"prompt_tokens"`
		CompletionTokens  int            `json:"completion_tokens"`
		PromptTokensDetails *usageDetails `json:"prompt_tokens_details"`
	} `json:"usage"`
	Timings *struct {
		CacheN int `json:"cache_n"`
	} `json:"timings"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			ReasoningContent *string `json:"reasoning_content"`
			Content          *string `json:"content"`
			ToolCalls        []wireToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

type wireToolCallDelta struct {
	Index    *int         `json:"index"`
	ID       *string      `json:"id"`
	Function *wireFunction `json:"function"`
}

type tcBuilder struct {
	ID   string
	Name string
	Args string
}

func (c *OpenAiClient) CompleteStream(ctx context.Context, messages []types.Message, tools []types.ToolSpec, onDelta func(types.Delta), idleMs int) (*types.Response, error) {
	data, err := c.buildBody(messages, tools, true)
	if err != nil {
		return nil, err
	}
	// 空闲超时：每 wire 块重置。缺省 idleMs=0 → max(timeout_ms, 10 分钟)
	//（主循环宽松值，慢 prefill 不被误杀）；压缩路径传 stream_idle_ms。
	idleTimeout := time.Duration(idleMs) * time.Millisecond
	if idleMs == 0 {
		idleTimeout = time.Duration(math.Max(float64(c.cfg.TimeoutMS), 600000)) * time.Millisecond
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
	finishReason := new(string)
	usage := types.Usage{}
	tcAcc := map[int]*tcBuilder{}
	var tcOrder []int

	br := bufio.NewReaderSize(resp.Body, 1<<16)
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
		cached := 0
		if f.Usage.PromptTokensDetails != nil {
			cached = f.Usage.PromptTokensDetails.CachedTokens
		} else if f.Timings != nil {
			cached = f.Timings.CacheN
		}
		*usage = types.Usage{
			PromptTokens:     f.Usage.PromptTokens,
			CompletionTokens: f.Usage.CompletionTokens,
			CachedTokens:     cached,
		}
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
