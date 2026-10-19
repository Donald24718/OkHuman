package agent

// 验证：中断入账的 partial 会不会进下一轮请求、JSON 形状是否 OpenAI 兼容
import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/types"
)

type recordingCut struct {
	mu   sync.Mutex
	seen [][]types.Message
	n    int
}

func (r *recordingCut) Complete(context.Context, []types.Message, []types.ToolSpec) (*types.Response, error) {
	return nil, errors.New("unused")
}
func (r *recordingCut) CompleteStream(_ context.Context, msgs []types.Message, _ []types.ToolSpec, onDelta func(types.Delta), _ int) (*types.Response, error) {
	r.mu.Lock()
	cp := append([]types.Message(nil), msgs...)
	r.n++
	r.seen = append(r.seen, cp)
	n := r.n
	r.mu.Unlock()
	if n == 1 {
		onDelta(types.Delta{Thinking: "这段思考也想带进下轮"})
		onDelta(types.Delta{Text: "半截正文ABC"})
		return nil, errors.New("上游流被掐断")
	}
	return &types.Response{Content: "第二轮正常收笔"}, nil
}

func TestPartialWireFormatNextRound(t *testing.T) {
	dir := t.TempDir()
	c := &recordingCut{}
	cm := ctxmgr.NewManager(c, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示", dir)
	a := New(c, "系统提示", cm)
	opt := testRunOpt()
	opt.DataDir = dir
	if _, err := a.Run(context.Background(), "第一问", opt); err == nil {
		t.Fatalf("第一轮应报错")
	}
	if _, err := a.Run(context.Background(), "第二问", opt); err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) < 2 {
		t.Fatalf("应有两次请求，实际 %d", len(c.seen))
	}
	b, _ := json.MarshalIndent(c.seen[1], "", "  ")
	t.Logf("第二轮实际发给 LLM 的 messages:\n%s", b)
	var found bool
	for _, m := range c.seen[1] {
		if m.Role == "assistant" && m.Content != nil {
			if s, ok := m.Content.(string); ok && s != "" {
				found = true
				if len(m.ToolCalls) != 0 {
					t.Errorf("这条 assistant 不该带 tool_calls（会留悬空引用）")
				}
			}
		}
	}
	if !found {
		t.Errorf("partial 没进第二轮请求")
	}
}
