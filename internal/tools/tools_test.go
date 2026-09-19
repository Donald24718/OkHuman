package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cappedBuffer 行为（2026-09-14 审计 H3 修）：前 n 字节保留、超出丢弃但计数、
// 管道持续排空（Write 永不报错、永不阻塞）。
func TestCappedBufferWithinCap(t *testing.T) {
	c := newCappedBuffer(10)
	if n, err := c.Write([]byte("12345")); err != nil || n != 5 {
		t.Fatalf("write1: n=%d err=%v", n, err)
	}
	if s := c.String(); s != "12345" {
		t.Fatalf("未超限时内容应为原文，got %q", s)
	}
}

func TestCappedBufferOverCap(t *testing.T) {
	c := newCappedBuffer(6)
	c.Write([]byte("abcdef")) // 恰好填满
	c.Write([]byte("XYZ"))    // 全部超出
	s := c.String()
	if !strings.Contains(s, "abcdef") {
		t.Fatalf("正文丢失: %q", s)
	}
	if !strings.Contains(s, "输出截断") || !strings.Contains(s, "丢弃") {
		t.Fatalf("缺截断说明: %q", s)
	}
	// 说明在正文之前（保头部截断可存活）
	if strings.Index(s, "输出截断") > strings.Index(s, "abcdef") {
		t.Fatalf("截断说明应在正文之前: %q", s)
	}
	if c.over != 3 {
		t.Fatalf("over=%d want 3", c.over)
	}
	// 继续写仍不报错（管道排空语义）
	if n, err := c.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("超限后 write: n=%d err=%v", n, err)
	}
	if c.over != 7 {
		t.Fatalf("over=%d want 7", c.over)
	}
}

func TestCappedBufferPartialOver(t *testing.T) {
	c := newCappedBuffer(4)
	c.Write([]byte("123456")) // 4 保留 + 2 丢弃
	s := c.String()
	// 说明在前、正文在后
	if strings.Index(s, "输出截断") > strings.Index(s, "1234") {
		t.Fatalf("截断说明应在正文之前: %q", s)
	}
	if !strings.HasSuffix(s, "1234") {
		t.Fatalf("保留正文 1234 缺失: %q", s)
	}
	if c.over != 2 {
		t.Fatalf("over=%d want 2", c.over)
	}
}

// TestBashViaTempScript 方案 B（2026-09-15 移植自 TS 4a5182c）：命令文本进 /tmp 临时脚本、
// 不进 argv——根治 pkill/pgrep -f 自匹配自杀；进程退出后脚本删除。
// 验证：1) 运行期没有任何进程的 argv 含命令文本（在脚本文件里，不在 argv）；
//       2) 输出正常；3) 退出后无新增临时脚本（跑前快照比对——同机其他 OkHuman
//       实例的并发脚本不算残留）。
func TestBashViaTempScript(t *testing.T) {
	marker := "okhuman-planb-marker-7731"
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "okhuman-bash-*.sh"))
	done := make(chan string, 1)
	go func() {
		out, err := ExecuteTool("bash", map[string]interface{}{
			"command":         "sleep 1; echo " + marker,
			"timeout_seconds": 10,
		})
		if err != nil {
			done <- "ERR:" + err.Error()
			return
		}
		done <- out
	}()
	// 运行期扫 /proc：命令文本不应出现在任何进程 argv 里
	time.Sleep(300 * time.Millisecond)
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(cmdline) == 0 {
			continue
		}
		if strings.Contains(string(cmdline), marker) {
			t.Fatalf("命令文本出现在进程 %s 的 argv（应在 /tmp 脚本里，不在 argv）", e.Name())
		}
	}
	out := <-done
	if !strings.Contains(out, marker) {
		t.Fatalf("输出不含 marker: %q", out)
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "okhuman-bash-*.sh"))
	beforeSet := map[string]bool{}
	for _, f := range before {
		beforeSet[f] = true
	}
	for _, f := range after {
		if !beforeSet[f] {
			t.Errorf("临时脚本未删除（新增残留）: %s", f)
		}
	}
}
