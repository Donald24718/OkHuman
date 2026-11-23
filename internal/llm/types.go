package llm

import (
	"context"

	"okhuman/internal/types"
)

// LlmClient 客户端接口（主循环与压缩共用同一实例）。
// 契约只有流式一条（2026-11-12 删除非流式 Complete：主循环与压缩都不用它，
// 压缩 2026-09-01 起改流式是为了能看思考流；留着 = 响应解析要维护两条路径）。
// CompleteStream：流式——onDelta 逐块回调（WebUI 实时显示），返回聚合响应。
//
//	ctx：/stop 中止上下文——cancel 后 HTTP 断开，llama.cpp 检测客户端断连
//	     即取消当前生成（中断模型侧处理，2026-08-29）。
//	idleMs：空闲超时毫秒——无 wire 字节（含 ": ping" 注释帧）超此值 →
//	     IdleTimeoutError。0 = 用客户端自己的 StreamIdleMS（来自 llm.stream_idle_sec，
//	     2026-10-18 A5）；压缩路径显式传 context.stream_idle_sec，快失败。
type LlmClient interface {
	CompleteStream(ctx context.Context, messages []types.Message, tools []types.ToolSpec, onDelta func(types.Delta), idleMs int) (*types.Response, error)
}
