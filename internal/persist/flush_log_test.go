// FlushLog 必须等到已排队的日志**真正写进文件**，而不是只等队列空。
// 旧实现看的是 len(logCh)：单写手把最后一条从队列取走后长度就归零，那一行却还没落盘。
// 为了让这个窗口可复现，最后一条刻意写得很大（写它本身要花时间），旧实现在这里必红。
package persist

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFlushLogWaitsForQueuedLines(t *testing.T) {
	d := t.TempDir()
	p, err := Init(d)
	if err != nil {
		t.Fatal(err)
	}
	const small = 20
	p.logMu.Lock() // 先卡住写手：它会取走第一条，但写不下去
	for i := 0; i < small; i++ {
		p.Log(fmt.Sprintf("第%d条", i))
	}
	p.Log("大一条压队：" + strings.Repeat("X", 2<<20)) // 2MB：写它需要可观测的时间
	done := make(chan struct{})
	go func() { p.FlushLog(); close(done) }()
	time.Sleep(50 * time.Millisecond) // 让写手停在"已取出、正等 logMu"这个状态上
	p.logMu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("FlushLog 没有返回")
	}
	b, err := os.ReadFile(filepath.Join(d, LogFile))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < small; i++ {
		if !bytes.Contains(b, []byte(fmt.Sprintf("第%d条", i))) {
			t.Fatalf("FlushLog 返回时第%d条还没落盘", i)
		}
	}
	if n := bytes.Count(b, []byte("大一条压队")); n != 1 {
		t.Fatalf("FlushLog 返回时那条 2MB 的日志没写全（出现 %d 次，文件 %d 字节）", n, len(b))
	}
}
