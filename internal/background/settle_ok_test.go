package background

// 后台任务成败必须来自 types.ToolOutcome.OK 字段，不能从结果正文反推。
// 旧实现是 !strings.HasPrefix(result, tools.ToolFailPrefix)：后台命令的输出
// 恰好以"工具执行失败: "开头（grep 日志、sed 取首行、脚本自己打印这句话）
// 就被判成失败 → 通知写"已失败"而正文其实是好结果 → agent 重跑同一条命令
// （重复下载 / 重复重启服务 / 重复覆盖文件）。
import (
	"testing"
	"time"

	"okhuman/internal/tools"
	"okhuman/internal/types"
)

// settleWait 等 settle 回调（带绝对超时，避免测试永挂）。
func settleWait(t *testing.T, done <-chan *types.PendingBackgroundTask) *types.PendingBackgroundTask {
	t.Helper()
	select {
	case tk := <-done:
		return tk
	case <-time.After(3 * time.Second):
		t.Fatal("settle 回调 3s 内未触发")
		return nil
	}
}

// TestSettleOKIgnoresFailPrefixInText（核心回归）：正文**以失败前缀开头**但
// 工具层成功 → OK 必须为 true。旧实现在这条上必然失败。
func TestSettleOKIgnoresFailPrefixInText(t *testing.T) {
	o := NewOrchestrator()
	done := make(chan *types.PendingBackgroundTask, 1)
	pr := make(chan types.ToolOutcome, 1)
	o.OnBackgroundStart(types.BackgroundStartArgs{
		CallID:    "c1",
		ToolName:  "bash",
		StartedAt: time.Now().UnixMilli() - 1234,
		Promise:   pr,
		OnSettled: func(tk *types.PendingBackgroundTask) { done <- tk },
	})
	// 后台跑 grep 抓日志：输出第一行就是失败前缀，但命令本身跑成功了
	pr <- types.ToolOutcome{Text: tools.ToolFailPrefix + "some log line", OK: true}

	tk := settleWait(t, done)
	if !tk.OK {
		t.Errorf("正文以失败前缀开头但工具层成功 → OK 必须 true，实际 false（旧 bug 复发）")
	}
	if !tk.Settled {
		t.Errorf("Settled 应为 true")
	}
	if tk.Result != tools.ToolFailPrefix+"some log line" {
		t.Errorf("Result 必须原样保留正文（含前缀，给模型看）：%q", tk.Result)
	}
	if tk.DurationMs <= 0 {
		t.Errorf("DurationMs 应 >0，实际 %d", tk.DurationMs)
	}
}

// TestSettleOKFalseFromField：工具层真出错 → OK=false（前缀仍在文本里）。
func TestSettleOKFalseFromField(t *testing.T) {
	o := NewOrchestrator()
	done := make(chan *types.PendingBackgroundTask, 1)
	pr := make(chan types.ToolOutcome, 1)
	o.OnBackgroundStart(types.BackgroundStartArgs{
		CallID:    "c2",
		ToolName:  "bash",
		StartedAt: time.Now().UnixMilli(),
		Promise:   pr,
		OnSettled: func(tk *types.PendingBackgroundTask) { done <- tk },
	})
	pr <- types.ToolOutcome{Text: tools.ToolFailPrefix + "参数 command 缺失或不是字符串", OK: false}

	tk := settleWait(t, done)
	if tk.OK {
		t.Errorf("工具层出错必须 OK=false")
	}
}

// TestSettleOKTrueForFailedCommandExitCode：语义边界不变——bash 退出码非 0
// 仍是 OK=true（工具层没炸）。本次改动不偷偷扩大"失败"的定义。
func TestSettleOKTrueForFailedCommandExitCode(t *testing.T) {
	o := NewOrchestrator()
	done := make(chan *types.PendingBackgroundTask, 1)
	pr := make(chan types.ToolOutcome, 1)
	o.OnBackgroundStart(types.BackgroundStartArgs{
		CallID:    "c3",
		ToolName:  "bash",
		StartedAt: time.Now().UnixMilli(),
		Promise:   pr,
		OnSettled: func(tk *types.PendingBackgroundTask) { done <- tk },
	})
	pr <- types.ToolOutcome{Text: "退出码: 1\n", OK: true}

	tk := settleWait(t, done)
	if !tk.OK {
		t.Errorf("命令退出码非 0 必须仍 OK=true（工具层成功），实际 false")
	}
}
