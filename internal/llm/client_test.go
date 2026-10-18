package llm

import (
	"testing"
	"time"
)

// TestIdleFallback 主循环空闲超时口径（2026-10-18 A5）：配了 stream_idle_ms 就用它；
// 没配才回落旧行为 max(timeout_ms, 10 分钟)。此前主循环不可配，后端卡住白挂 10 分钟。
func TestIdleFallback(t *testing.T) {
	c := NewOpenAiClient(ClientConfig{TimeoutMS: 300000, StreamIdleMS: 120000})
	if got := c.idleFallback(); got != 120*time.Second {
		t.Errorf("配置值未生效：得 %v，期望 120s", got)
	}
	// 未配置 → 旧口径 max(timeout_ms, 600000)
	c2 := NewOpenAiClient(ClientConfig{TimeoutMS: 300000})
	if got := c2.idleFallback(); got != 600*time.Second {
		t.Errorf("未配置时应回落 max(timeout,10min)：得 %v", got)
	}
	c3 := NewOpenAiClient(ClientConfig{TimeoutMS: 900000})
	if got := c3.idleFallback(); got != 900*time.Second {
		t.Errorf("未配置且 timeout 更大时应取 timeout：得 %v", got)
	}
}
