package server

// 回归测试（2026-10-19）：重启续接时 inject 侧车还在磁盘上，而 InjectSeq 是内存值。
// 修前：新注入复用 inj-1 → AtomicWriteFile 覆盖旧会话同号侧车（旧引用张冠李戴，无报错），
// 且旧附件在附件列表里不可见（compose 照发，用户管不到）。
// 修后：InjectSeq 启动时越过磁盘最大序号；仍被会话引用的旧附件回填列表。
import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

func TestInjectSeqAndListSurviveRestart(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	dir := filepath.Join(cfg.Data.Dir, "inject")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := `[{"type":"image_url","image_url":{"url":"data:image/png;base64,VERIFIED_OLD_IMAGE"}}]`
	_ = old
	if err := os.WriteFile(filepath.Join(dir, "inj-1.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	// 模拟重启后的进程：会话（已恢复）引用 inj-1，inject 目录里旧侧车还在
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	a.CM().AddMessage(&types.RawEntry{Role: "user", Content: []types.ContentPart{types.RefPart("inj-1")}})
	st := NewAppState(cfg, t.TempDir(), a)
	if st.InjectSeq != 1 {
		t.Errorf("InjectSeq 应越过磁盘已有最大序号 1，实际 %d", st.InjectSeq)
	}
	if len(st.Injects) != 1 || st.Injects[0].ID != "inj-1" {
		t.Errorf("重启后仍被引用的旧附件应回填列表，实际 %+v", st.Injects)
	}
	ap := New(st, nil)
	srv := httptest.NewServer(ap)
	defer srv.Close()

	b, _ := json.Marshal(map[string]interface{}{"content": []map[string]string{{"type": "text", "text": "NEW_TEXT_TODAY"}}})
	resp, err := srv.Client().Post(srv.URL+"/inject", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	t.Logf("/inject 返回: %v", out)
	raw, _ := os.ReadFile(filepath.Join(dir, "inj-1.json"))
	t.Logf("注入后 inj-1.json 实际内容: %s", raw)
	if !bytes.Contains(raw, []byte("VERIFIED_OLD_IMAGE")) {
		t.Errorf("旧会话的 inj-1 侧车被新附件覆盖，旧引用张冠李戴。覆盖后=%s", raw)
	}
	if out["id"] == "inj-1" {
		t.Errorf("新注入复用了旧会话的 id inj-1")
	}
	// 新侧车应落在 inj-2，且旧 inj-1 与新 inj-2 各自独立
	if _, err := os.Stat(filepath.Join(dir, "inj-2.json")); err != nil {
		t.Errorf("新附件应写 inj-2.json：%v", err)
	}
}
