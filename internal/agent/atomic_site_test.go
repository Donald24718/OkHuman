package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteToolLogAtomicNoResidue 工具全文落盘改原子写（C2）：不留 .tmp-*，
// 文件内容完整（工具全文是给模型 cat 回去的，半截 = 数据丢失）。
func TestWriteToolLogAtomicNoResidue(t *testing.T) {
	a := New(nil, "sys", nil)
	opt := RunOptions{DataDir: t.TempDir()}
	full := strings.Repeat("长输出内容", 4000)
	path := a.writeToolLog(opt, full)
	if !strings.HasPrefix(path, opt.DataDir) {
		t.Fatalf("未落盘或路径异常：%s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != full {
		t.Errorf("全文不完整：得 %d 字节，期望 %d", len(b), len(full))
	}
	ms, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp-*"))
	if len(ms) != 0 {
		t.Errorf("留下 .tmp-* 残留：%v", ms)
	}
}
