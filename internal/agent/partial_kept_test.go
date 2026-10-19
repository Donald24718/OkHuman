package agent

// 回归（2026-10-19）：流式调用中途报错（空闲超时 / 畸形帧达上限 / 网络断 / 上游 5xx）时，
// 已经逐块推给前端的正文曾被整段丢弃——刷新后查无痕迹，模型下一轮也不知道自己写过什么。
// 修后：中断前的正文入账为 assistant 消息并标注中断，错误仍原样上抛、本轮仍终止。
import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/types"
)

type cutStream struct{}

func (cutStream) Complete(context.Context, []types.Message, []types.ToolSpec) (*types.Response, error) {
	return nil, errors.New("unused")
}
func (cutStream) CompleteStream(_ context.Context, _ []types.Message, _ []types.ToolSpec, onDelta func(types.Delta), _ int) (*types.Response, error) {
	onDelta(types.Delta{Thinking: "先想一段"})
	onDelta(types.Delta{Text: "已经生成的半截正文丢了就找不回来"})
	return nil, errors.New("上游流被掐断（模拟空闲超时/网络断）")
}

func TestPartialKeptOnStreamError(t *testing.T) {
	dir := t.TempDir()
	cm := ctxmgr.NewManager(cutStream{}, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示", dir)
	a := New(cutStream{}, "系统提示", cm)
	opt := testRunOpt()
	opt.DataDir = dir
	var streamed string
	opt.OnEvent = func(e types.AgentEvent) {
		if e["type"] == "delta" {
			if s, ok := e["text"].(string); ok {
				streamed += s
			}
		}
	}
	if _, err := a.Run(context.Background(), "问", opt); err == nil {
		t.Fatalf("应当报错")
	}
	t.Logf("前端当时实际看到过：%q", streamed)
	var kept bool
	for _, m := range cm.Session().Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			if s, ok := m.Content.(string); ok && s != "" {
				kept = kept || s != ""
			}
		}
	}
	n := 0
	for _, m := range cm.Session().Messages {
		if m.Role == "assistant" {
			n++
		}
	}
	if n == 0 {
		t.Errorf("已生成 %d 字符并推给前端，但会话里没有 assistant 消息 → 那半截丢失（消息 %d 条：%v）", len(streamed), len(cm.Session().Messages), fmt.Sprint(cm.Session().Messages))
	}
	var body string
	for _, m := range cm.Session().Messages {
		if m.Role == "assistant" {
			body, _ = m.Content.(string)
		}
	}
	if !strings.Contains(body, "已经生成的半截正文丢了就找不回来") || !strings.Contains(body, "中断") {
		t.Errorf("入账的 assistant 消息应含已生成正文并标注中断，实际：%q", body)
	}
	if !kept {
		t.Errorf("入账消息正文不应为空")
	}
}
