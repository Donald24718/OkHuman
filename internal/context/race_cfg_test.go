// 手动压缩进行中（模型还在跑）时热更配置：detail 用到的窗口配置必须是锁内取到的副本。
package context

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"okhuman/internal/llm"
	"okhuman/internal/types"
)

type slowCompressClient struct{}

func (slowCompressClient) CompleteStream(context.Context, []types.Message, []types.ToolSpec, func(types.Delta), int) (*types.Response, error) {
	time.Sleep(120 * time.Millisecond)
	return &types.Response{Content: "总结正文"}, nil
}

func TestCompressNowConcurrentSetCfg(t *testing.T) {
	m := compressFixture(t, slowCompressClient{})
	var running atomic.Bool
	running.Store(true)
	done := make(chan struct{})
	go func() { m.CompressNow(); running.Store(false); close(done) }()
	start := time.Now()
	n := 0
	// 并发窗口必须覆盖到 detail 拼串那一刻（第一次写只跑固定时长，读完就停了，
	// 变异时测不到 race）。
	for running.Load() && time.Since(start) < 5*time.Second {
		m.mu.Lock()
		cfg := m.cfg
		m.mu.Unlock()
		cfg.KeepSummaries = 1
		m.SetCfg(cfg)
		n++
	}
	<-done
	if n == 0 {
		t.Fatal("并发窗口没形成，测不到")
	}
	_ = llm.LlmClient(slowCompressClient{})
}
