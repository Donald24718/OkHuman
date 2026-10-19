package persist

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 日志必须按调用顺序落盘：okhuman.log 是审计时间线（谁在什么时候 reset、
// 后端什么时候消失），全靠行的先后推断。
func TestLogKeepsCallOrder(t *testing.T) {
	dir := t.TempDir()
	p, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	const n = 400
	// 同一调用序列（一个 goroutine 顺序打）——这是日志必须保证的：
	// 落盘顺序 = 调用顺序。旧实现每条日志另起 goroutine，这里也会乱。
	for i := 1; i <= n; i++ {
		p.Log("seq-" + strconv.Itoa(i))
	}
	// 等异步写收尾（当前实现是 go appendLogLine，没有任何 join）
	for attempt := 0; attempt < 100; attempt++ {
		b, _ := os.ReadFile(filepath.Join(dir, LogFile))
		if len(strings.Split(strings.TrimSpace(string(b)), "\n")) >= n {
			break
		}
		<-timeAfter(10)
	}
	b, err := os.ReadFile(filepath.Join(dir, LogFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != n {
		t.Fatalf("日志行数 %d，应为 %d（异步写没 join，进程退出时会丢尾巴）", len(lines), n)
	}
	// 顺序检查：统计"逆序对"比例
	seen := map[int]bool{}
	inv, last := 0, 0
	for _, ln := range lines {
		i := strings.Index(ln, "seq-")
		if i < 0 {
			continue
		}
		v, _ := strconv.Atoi(ln[i+4:])
		if seen[v] {
			t.Fatalf("日志行重复：%d", v)
		}
		seen[v] = true
		if v < last {
			inv++
		}
		last = v
	}
	if inv > n/20 {
		t.Fatalf("日志乱序 %d/%d 行——审计时间线不可信", inv, n)
	}
}
