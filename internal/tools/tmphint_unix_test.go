//go:build !windows

package tools

import (
	"strings"
	"testing"
)

// TestToolDescription_NoTmpWarningOnUnix Unix 上 /tmp 真实可写，
// 工具描述**不得**出现 Windows 的规避提示（那是 Unix 上的错误信息）。
//
// 同时守护 Unix 侧描述的**逐字稳定性**：detachHint 里就有 `/tmp/<名>.log`，
// 若误把 Windows 提示串进来，Unix 上会自相矛盾（一边教用 /tmp 一边说别用）。
func TestToolDescription_NoTmpWarningOnUnix(t *testing.T) {
	desc := buildTools(30000, 600000)[0].Description

	if tmpHint() != "" {
		t.Fatalf("Unix 侧 tmpHint 必须为空串（/tmp 在 Unix 上真实可写），实际=%q", tmpHint())
	}
	if strings.Contains(desc, "不要写 /tmp") {
		t.Errorf("Unix 工具描述混入 Windows 的 /tmp 规避提示：\n%s", desc)
	}
	// /tmp 在 Unix 描述里**应当**出现（detachHint 的 setsid 重定向目标）。
	if !strings.Contains(desc, "/tmp/") {
		t.Errorf("Unix 工具描述丢失了 /tmp 用法（detachHint 依赖它）：\n%s", desc)
	}
}
