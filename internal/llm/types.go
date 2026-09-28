package llm

import (
	"context"

	"okhuman/internal/types"
)

// LlmClient 客户端接口（主循环与压缩共用同一实例）。
// Complete：非流式聚合（压缩请求用，无需流式显示）。
// CompleteStream：流式——onDelta 逐块回调（WebUI 实时显示），返回聚合响应。
//
//	ctx：/stop 中止上下文——cancel 后 HTTP 断开，llama.cpp 检测客户端断连
//	     即取消当前生成（中断模型侧处理，2026-08-29）。
//	idleMs：空闲超时毫秒——无 wire 字节（含 ": ping" 注释帧）超此值 →
//	     IdleTimeoutError。0 = 缺省 max(timeout_ms, 10 分钟)（主循环，
//	     容忍慢 prefill）；压缩路径传 stream_idle_ms（默认 90000，快失败）。
type LlmClient interface {
	Complete(ctx context.Context, messages []types.Message, tools []types.ToolSpec) (*types.Response, error)
	CompleteStream(ctx context.Context, messages []types.Message, tools []types.ToolSpec, onDelta func(types.Delta), idleMs int) (*types.Response, error)
}
