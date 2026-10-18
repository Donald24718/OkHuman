package inject

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestStripSidecarAtomicComplete 剥离侧车改原子写（C2）后：内容必须是完整可解析
// JSON，目录里不留 .tmp-*。侧车是"下次 compose 才读"的文件——半截 JSON 等于附件
// 静默消失，或下次加载直接失败。
func TestStripSidecarAtomicComplete(t *testing.T) {
	dir := t.TempDir()
	id := "inj-7"
	p := filepath.Join(dir, id+".json")
	if err := os.WriteFile(p, []byte(`[{"type":"text","text":"原附件内容"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !StripSidecar(dir, id) {
		t.Fatal("StripSidecar 返回 false")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var parts []map[string]interface{}
	if err := json.Unmarshal(b, &parts); err != nil {
		t.Fatalf("剥离后侧车不是完整 JSON（疑似半截）：%v content=%q", err, b)
	}
	if len(parts) != 1 {
		t.Errorf("剥离桩内容异常：%v", parts)
	}
	ms, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if len(ms) != 0 {
		t.Errorf("留下 .tmp-* 残留：%v", ms)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Errorf("权限应保持 0644，实际 %s", st.Mode().Perm())
	}
}
