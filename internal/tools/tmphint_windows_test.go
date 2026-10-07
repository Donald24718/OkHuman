//go:build windows

package tools

import (
	"strings"
	"testing"
)

// TestToolDescription_WarnsAgainstTmp Windows 上工具描述必须警告模型别写 /tmp。
//
// **为什么必须有这条（2026-10-05 实证）**：
// Windows 的 /tmp 是 MSYS 挂载点（/etc/fstab 的 `usertemp`），挂载目标由 MSYS 运行时
// 自行推导，**不受 TMP / TEMP / TMPDIR / MSYS_TMP 影响**（四组控制变量实测），
// 且目标会随宿主会话变化、可能被回收 → 写 /tmp 报 ENOENT。
// OkHuman 改不动 MSYS 挂载，只能靠**工具描述**让模型主动规避。
func TestToolDescription_WarnsAgainstTmp(t *testing.T) {
	desc := buildTools(30000, 600000)[0].Description

	if !strings.Contains(desc, "/tmp") {
		t.Fatalf("Windows 工具描述未提及 /tmp，模型会照常写 /tmp 而失败：\n%s", desc)
	}
	if !strings.Contains(desc, "$TEMP") {
		t.Errorf("Windows 工具描述未给出可用替代（$TEMP），模型无从规避：\n%s", desc)
	}
	t.Logf("描述中的临时目录提示片段：\n%s", tmpHint())
}

// TestTmpHint_NonEmpty Windows 侧 tmpHint 不得为空（空 = 提示丢失）。
func TestTmpHint_NonEmpty(t *testing.T) {
	if tmpHint() == "" {
		t.Fatal("Windows 侧 tmpHint 为空串——临时目录提示未生效")
	}
}
