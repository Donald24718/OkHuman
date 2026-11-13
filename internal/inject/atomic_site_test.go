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
	ok, err := StripSidecar(dir, id)
	if err != nil {
		t.Fatalf("StripSidecar 报错：%v", err)
	}
	if !ok {
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

// TestStripSidecarReportsWriteFailure 写不进去时必须把原因回出来（不是只说"没剥"）。
// 场景：收件目录只读（盘满/权限都会走这条）。/strip-injects 靠它告诉用户为什么是 0 个。
func TestStripSidecarReportsWriteFailure(t *testing.T) {
	dir := t.TempDir()
	id := "inj-20261112-0001"
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(`[{"type":"text","text":"原图"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // 只读：建不了临时文件
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	ok, err := StripSidecar(dir, id)
	if err == nil {
		t.Fatal("只读目录应报写盘失败，实得 nil")
	}
	if ok {
		t.Fatal("写失败却报成功")
	}
	t.Logf("回出的原因：%v", err)
}

// TestStripSidecarMissingIsNotFailure 侧车不存在 = 没什么可剥，不该报错误。
func TestStripSidecarMissingIsNotFailure(t *testing.T) {
	ok, err := StripSidecar(t.TempDir(), "inj-20261112-0002")
	if err != nil || ok {
		t.Fatalf("不存在的侧车应 (false, nil)，实得 (%v, %v)", ok, err)
	}
}
