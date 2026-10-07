//go:build !windows

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 缺陷 3：磁盘写入失败时，提示里给出的必须是"可用的路径"，或明确说明失败 ----

// TestWriteToolLog_SuccessReturnsRealPath 成功时返回真实存在的文件路径。
func TestWriteToolLog_SuccessReturnsRealPath(t *testing.T) {
	tmp := t.TempDir()
	a := &Agent{}
	p := a.writeToolLog(RunOptions{DataDir: tmp}, "hello world")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("返回路径 %q 不存在: %v", p, err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "hello world" {
		t.Errorf("落盘内容不符: %q", string(data))
	}
}

// TestWriteToolLog_DiskFailure 磁盘失败时不得返回一个"看似路径"的字符串。
//
// 背景（2026-10-04 实证）：旧实现失败时返回 "[文件写入失败：写入失败]"，
// 而被无条件当路径拼进提示 → 模型会 `cat [文件写入失败：写入失败]`。
//
// 触发方式：把 DataDir 指向一个【普通文件】，则 <file>/tool-results 的
// MkdirAll 必然失败（不依赖权限，root 下同样有效）。
func TestWriteToolLog_DiskFailure(t *testing.T) {
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("建阻塞文件失败: %v", err)
	}

	a := &Agent{}
	p, ok := a.writeToolLogOK(RunOptions{DataDir: blocker}, "data")
	if ok {
		t.Fatalf("DataDir 是普通文件，落盘必须失败，却返回 ok=true path=%q", p)
	}
	// 失败时：返回的说明不得是一个"存在的路径"
	if _, err := os.Stat(p); err == nil {
		t.Errorf("失败时不应返回存在的路径: %q", p)
	}
	if !strings.Contains(p, "失败") {
		t.Errorf("失败说明 %q 应明确含'失败'字样", p)
	}
	t.Logf("落盘失败返回值 = %q（ok=false）", p)
}

// TestTruncationHint_OnWriteFailure 触发截断且落盘失败时，回填文本不得包含"去 cat 该路径"的假指引。
func TestTruncationHint_OnWriteFailure(t *testing.T) {
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("建阻塞文件失败: %v", err)
	}

	a := &Agent{}
	opt := RunOptions{DataDir: blocker, ResultLimit: 10}
	body := strings.Repeat("X", 100)
	got := a.formatTruncatedForModel(opt, body, opt.ResultLimit)

	// 失败时不得指引模型去 cat（会指向不存在的路径）
	if strings.Contains(got, "用 bash 的 cat / grep / tail 查看") {
		t.Errorf("落盘失败却仍指引模型去 cat: %q", got)
	}
	// 应如实告知不可找回
	if !strings.Contains(got, "落盘失败") && !strings.Contains(got, "无法找回") {
		t.Errorf("落盘失败时应如实说明，实际: %q", got)
	}
	t.Logf("落盘失败时的回填文本 = %q", got)
}

// ---- 缺陷 4：路径含空格时必须可安全复制 ----

// TestTruncationHint_PathQuotedWhenSpaces 路径含空格时应加引号包裹。
func TestTruncationHint_PathQuotedWhenSpaces(t *testing.T) {
	tmp := t.TempDir()
	sp := filepath.Join(tmp, "has space 空格")
	if err := os.MkdirAll(sp, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	a := &Agent{}
	opt := RunOptions{DataDir: sp, ResultLimit: 10}
	body := strings.Repeat("Y", 100)
	got := a.formatTruncatedForModel(opt, body, opt.ResultLimit)

	// 取出提示中的路径片段，检查是否被引号包裹
	if strings.Contains(got, "has space") {
		// 路径含空格 → 必须用引号（" 或 `）包裹，供模型安全复制
		hasQuote := strings.Contains(got, `"`) || strings.Contains(got, "`")
		if !hasQuote {
			t.Errorf("含空格路径未被引号包裹，模型裸拼 cat 会失败: %q", got)
		}
	} else {
		t.Errorf("提示中未出现路径: %q", got)
	}
}

// TestTruncationHint_ContainsSizeAndPath 正常路径下提示含 size 与路径。
func TestTruncationHint_Normal(t *testing.T) {
	tmp := t.TempDir()
	a := &Agent{}
	opt := RunOptions{DataDir: tmp, ResultLimit: 20}
	body := strings.Repeat("Z", 200)
	got := a.formatTruncatedForModel(opt, body, opt.ResultLimit)
	if !strings.Contains(got, fmt.Sprintf("%d", utf16Len(body))) {
		t.Errorf("提示应含完整结果字符数 %d: %q", utf16Len(body), got)
	}
	if !strings.Contains(got, "tool-results") {
		t.Errorf("提示应含 log 路径: %q", got)
	}
	if !strings.Contains(got, "cat / grep / tail") {
		t.Errorf("提示应含抓回指引: %q", got)
	}
}
