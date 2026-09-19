package agent

import (
	"context"
	"strings"
	"testing"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

// newTestAgent 建带 FakeLLM 的 agent（压缩阈值拉满，测试内不触发压缩）
func newTestAgent(steps []llm.FakeStep) (*Agent, *llm.FakeLLM) {
	fake := llm.NewFakeLLM(steps)
	cm := ctxmgr.NewManager(fake, ctxmgr.Cfg{
		MaxTokens: 1000000, KeepRecentChars: 60000, HardTruncChars: 50000,
		StreamIdleMS: 30000, CharsPerToken: 2,
	}, "系统提示", t0TempDir())
	return New(fake, "系统提示", cm), fake
}

func t0TempDir() string { return "/tmp/okhuman-agent-test" }

// TestTruncationRetry 异常截断检测（2026-09-15 移植自 TS bf7090b）：
// 响应有 thinking 但无正文也无工具调用 → emit trunc_warn + 注入提示重试一次，
// 重试正常 → 本轮正常结束（重试后的正文为回复）。
func TestTruncationRetry(t *testing.T) {
	thinking := "思考了很久但忘了写正文……"
	a, _ := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "", Reasoning: &thinking}, // 第一次：异常截断
		{Type: "text", Text: "重试成功"},              // 重试：正常回复
	})
	var warns []string
	opt := testRunOpt(&warns)
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if !strings.Contains(reply, "重试成功") {
		t.Errorf("回复应为重试结果，实际: %q", reply)
	}
	if len(warns) != 1 {
		t.Errorf("应恰好 1 条 trunc_warn，实际 %d 条: %v", len(warns), warns)
	}
}

// TestTruncationGiveUp 重试后仍截断 → 第 2 条 trunc_warn（放弃事件）后原样收尾（空正文）。
func TestTruncationGiveUp(t *testing.T) {
	thinking := "思考了很久但忘了写正文……"
	a, _ := newTestAgent([]llm.FakeStep{
		{Type: "text", Text: "", Reasoning: &thinking},
		{Type: "text", Text: "", Reasoning: &thinking}, // 重试仍截断
	})
	var warns []string
	opt := testRunOpt(&warns)
	reply, err := a.Run(context.Background(), "你好", opt)
	if err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if reply != "" {
		t.Errorf("放弃轮回复应为空，实际: %q", reply)
	}
	if len(warns) != 2 {
		t.Errorf("应有 2 条 trunc_warn，实际 %d 条: %v", len(warns), warns)
	}
}

func testRunOpt(warns *[]string) RunOptions {
	return RunOptions{
		DataDir:       "/tmp/okhuman-agent-test",
		ResultLimit:   50000,
		DoomWarnAfter: 3,
		FgTimeoutMS:   30000,
		Kind:          "user",
		OnEvent: func(e types.AgentEvent) {
			if e["type"] == "trunc_warn" {
				if d, ok := e["detail"].(string); ok {
					*warns = append(*warns, d)
				}
			}
		},
	}
}
