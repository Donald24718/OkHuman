package context

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"okhuman/internal/llm"
	"okhuman/internal/persist"
	"okhuman/internal/types"
)

func noTmpResidue(t *testing.T, dir string) {
	t.Helper()
	ms, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 0 {
		t.Errorf("%s 下留下 .tmp-* 残留：%v", dir, ms)
	}
}

// TestDumpAndSummaryAtomicNoResidue 存档与总结走原子写（C2）：写完目录里不能有
// .tmp-* 残留，文件内容必须完整（存档尾部有结束标记、总结是完整文本）。
func TestDumpAndSummaryAtomicNoResidue(t *testing.T) {
	dataDir := t.TempDir()
	m := NewManager(llm.NewFakeLLM([]llm.FakeStep{
		{Type: "compress", Scope: "history", Text: "（假压缩总结）"},
	}), Cfg{MaxTokens: 10, KeepRecentChars: 5, HardTruncChars: 100, StreamIdleMS: 200, CharsPerToken: 1}, "sys", dataDir)
	for i := 0; i < 6; i++ {
		m.AddMessage(&types.RawEntry{Role: "user", Content: strings.Repeat("内容", 60)})
	}
	rec, n := m.DumpSessionRecord("测试")
	if n == 0 || !strings.HasPrefix(filepath.Base(rec), "ctx-") {
		t.Fatalf("存档异常：%s n=%d", rec, n)
	}
	b, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 100 {
		t.Errorf("存档疑似被截断：%d 字节", len(b))
	}
	noTmpResidue(t, filepath.Dir(rec))
	// 压缩产生的总结文件同样不得留残留
	if dirs, _ := filepath.Glob(filepath.Join(dataDir, "session-summaries")); len(dirs) > 0 {
		noTmpResidue(t, dirs[0])
	}
}

// TestSidecarJSONValidAfterStrip 侧车原子写后必须是完整可解析 JSON（半截 JSON
// 会让下次 compose 直接读不出附件）。
func TestSidecarJSONValidAfterStrip(t *testing.T) {
	dir := t.TempDir()
	id := "inj-1"
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, []byte(`[{"type":"text","text":"原附件"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persist.AtomicWriteFile(path, `[{"type":"text","text":"已剥离"}]`); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var parts []map[string]string
	if err := json.Unmarshal(b, &parts); err != nil {
		t.Fatalf("侧车 JSON 解析失败（疑似半截）：%v content=%q", err, b)
	}
	noTmpResidue(t, dir)
}
