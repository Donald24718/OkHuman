// CompressNow 的返回值必须与真实结果一致（2026-11-13 交叉审查）。
// 旧实现无条件 return true + "已提交压缩…新增 1 条总结"，模型报错/空输出时
// 会话一点没变，webui 却弹"已压缩"。
package context

import (
	"context"
	"okhuman/internal/llm"
	"okhuman/internal/types"
	"strings"
	"testing"
)

type stubCompressClient struct{ err error }

func (c stubCompressClient) CompleteStream(context.Context, []types.Message, []types.ToolSpec, func(types.Delta), int) (*types.Response, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &types.Response{}, nil // 空输出
}

func compressFixture(t *testing.T, client llm.LlmClient) *Manager {
	t.Helper()
	m := NewManager(client, Cfg{
		MaxTokens: 100000, KeepRecentChars: 200, HardTruncChars: 200,
		CharsPerToken: 1, KeepSummaries: 5, SummaryMaxChars: 30000,
	}, "系统提示", t.TempDir())
	for i := 0; i < 8; i++ {
		m.AddMessage(&types.RawEntry{Role: "user", Content: strings.Repeat("记录内容", 60)})
	}
	return m
}

func TestCompressNowFailsOnEmptyOutput(t *testing.T) {
	m := compressFixture(t, stubCompressClient{})
	before := len(m.Session().Messages)
	detail, ok := m.CompressNow()
	if ok {
		t.Errorf("模型空输出时仍报成功：%s", detail)
	}
	if !strings.Contains(detail, "空输出") {
		t.Errorf("detail 没说清失败原因：%s", detail)
	}
	if len(m.Session().Messages) != before {
		t.Errorf("失败的压缩不该改会话：%d → %d", before, len(m.Session().Messages))
	}
	if len(m.Session().Summaries) != 0 {
		t.Errorf("失败的压缩不该有总结：%d 条", len(m.Session().Summaries))
	}
}

// 三次空闲超时 → 硬截断：会话确实变了（ok=true），但文案不能声称新增了总结。
func TestCompressNowHardTruncateDoesNotClaimSummary(t *testing.T) {
	m := compressFixture(t, stubCompressClient{err: &llm.IdleTimeoutError{IdleMS: 1, Cause: context.DeadlineExceeded}})
	detail, ok := m.CompressNow()
	if !ok {
		t.Errorf("硬截断确实改了会话，应回 true：%s", detail)
	}
	if strings.Contains(detail, "新增 1 条总结") || strings.Contains(detail, "已提交压缩") {
		t.Errorf("硬截断没有生成总结，文案却这么写：%s", detail)
	}
	if !strings.Contains(detail, "硬截断") {
		t.Errorf("文案应说明降级成了硬截断：%s", detail)
	}
	// 硬截断会放一条指向全文存档的占位 summary（既有设计），这里只要求文案不冒充
	// "模型已生成总结"，且会话确实被改短了。
	if len(m.Session().Messages) >= 8 {
		t.Errorf("硬截断应当改短会话，实际仍 %d 条", len(m.Session().Messages))
	}
}
