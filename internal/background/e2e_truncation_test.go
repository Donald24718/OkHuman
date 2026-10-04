package background

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"okhuman/internal/types"
)

// ---- L3 端到端：真跑后台任务，结果超 256KB 且为中文，验证存储副本合法性 ----

// captureOrchestrator 建一个把 settle 回调存下来的编排器。
func newTestOrch() *BackgroundOrchestrator {
	return NewOrchestrator()
}

// TestE2E_L3_BackgroundCJKTruncationValidUTF8 端到端：
// 后台任务结果 = 10 万个中文（30 万字节，超 256KB 上限），
// 走完 OnBackgroundStart → settle → 存储截断，验证 Snapshot 里的 Result 是合法 UTF-8，
// 且能正常 JSON 序列化（WebUI /backgrounds 端点用）。
func TestE2E_L3_BackgroundCJKTruncationValidUTF8(t *testing.T) {
	o := newTestOrch()
	// 10 万个中文 = 300000 字节 > 262144（256KB）
	result := strings.Repeat("中文测试数据", 20000)
	t.Logf("原始结果：%d 字符 / %d 字节", utf8.RuneCountInString(result), len(result))
	if len(result) <= maxStoredResultBytes {
		t.Fatalf("测试构造有误：结果 %d 字节未超上限 %d", len(result), maxStoredResultBytes)
	}

	promise := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	o.OnBackgroundStart(types.BackgroundStartArgs{
		CallID:    "call-e2e-1",
		ToolName:  "bash",
		StartedAt: 0,
		Promise:   promise,
		OnSettled: func(*types.PendingBackgroundTask) { wg.Done() },
	})
	promise <- result
	wg.Wait()

	// 读取存储副本（可能刚截断，等一下 goroutine 完成 mu 区）
	snap := o.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("应有 1 个任务，实际 %d", len(snap))
	}
	stored := snap[0].Result
	t.Logf("存储副本：%d 字节", len(stored))

	if !utf8.ValidString(stored) {
		t.Errorf("🔴 存储副本不是合法 UTF-8（capBytes 在中文边界切半）")
		// 定位首个非法位置
		for i := 0; i < len(stored); i++ {
			if !utf8.ValidString(stored[:i+1]) {
				t.Logf("   首个非法前缀长度 = %d，字节 = % x", i+1, []byte(stored[max(0, i-3):i+1]))
				break
			}
		}
	}

	// JSON 序列化（/backgrounds 端点的实际行为）
	_, err := json.Marshal(snap)
	if err != nil {
		t.Errorf("🔴 JSON 序列化失败（非法 UTF-8 导致）: %v", err)
	} else {
		t.Logf("JSON 序列化成功")
	}

	// 截断说明存在
	if !strings.HasSuffix(stored, "\n[已截断]") {
		t.Errorf("应带 [已截断] 说明，实际结尾: %q", tailStr(stored, 30))
	}
}

// TestE2E_L3_JSONBoundaryCorruption 端到端：非法 UTF-8 在 JSON 编码时的实际表现。
func TestE2E_L3_JSONBoundaryCorruption(t *testing.T) {
	// 直接复现 capBytes 的产物经 JSON 编码
	corrupt := capBytes(strings.Repeat("中", 100), 1) // 旧实现返回半个字符
	t.Logf("capBytes(中×100, 1) = % x", []byte(corrupt))
	b, err := json.Marshal(map[string]string{"result": corrupt})
	t.Logf("JSON = %s  err=%v", b, err)
	if !utf8.ValidString(corrupt) {
		t.Logf("🔴 确认：截断产物非法，JSON 里会变成替换字符 \\ufffd")
		if strings.Contains(string(b), `\ufffd`) {
			t.Logf("   实测 JSON 中出现了 \\ufffd 替换字符")
		}
	}
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
