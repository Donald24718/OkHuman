// 手动压缩的前端事件（compress / compress_delta）此前没有任何测试覆盖，而它的接线代码
// 一度在两条路径上各抄一份（2026-11-13 四方审计 C1）。这里钉住两件事：
// 压缩过程必须发出事件；调用结束后监听必须注销干净（否则下一次压缩会把事件投给旧回调）。
package agent

import (
	"context"
	"strings"
	"testing"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/types"
)

type summaryClient struct{}

func (summaryClient) CompleteStream(context.Context, []types.Message, []types.ToolSpec,
	func(types.Delta), int) (*types.Response, error) {
	return &types.Response{Content: "这是压缩出来的总结"}, nil
}

func compressAgent(t *testing.T) (*Agent, *ctxmgr.Manager) {
	t.Helper()
	cm := ctxmgr.NewManager(summaryClient{}, ctxmgr.Cfg{
		MaxTokens: 100000, KeepRecentChars: 200, HardTruncChars: 200,
		CharsPerToken: 1, KeepSummaries: 5, SummaryMaxChars: 30000,
	}, "系统提示", t.TempDir())
	for i := 0; i < 8; i++ {
		cm.AddMessage(&types.RawEntry{Role: "user", Content: strings.Repeat("记录内容", 60)})
	}
	return New(summaryClient{}, "系统提示", cm), cm
}

func TestCompressNowWithEventsEmitsThenUnwires(t *testing.T) {
	a, cm := compressAgent(t)
	var got []string
	detail, ok := a.CompressNowWithEvents(func(e types.AgentEvent) {
		if s, _ := e["type"].(string); s != "" {
			got = append(got, s)
		}
	})
	if !ok {
		t.Fatalf("压缩应成功：%s", detail)
	}
	if !contains(got, "compress") {
		t.Errorf("没有收到压缩结果事件：%v", got)
	}
	// 再压一次：事件不得再投给上一次调用留下的回调。
	before := len(got)
	for i := 0; i < 8; i++ {
		cm.AddMessage(&types.RawEntry{Role: "user", Content: strings.Repeat("第二批记录", 60)})
	}
	if _, ok := cm.CompressNow(); !ok {
		t.Fatal("夹具没能造出第二次压缩，注销无从验证")
	}
	if len(got) != before {
		t.Errorf("调用结束后监听仍在投递事件：%v", got[before:])
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
