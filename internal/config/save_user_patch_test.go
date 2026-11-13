package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readMap(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("写出来的不是合法 JSON：%v\n%s", err, b)
	}
	return m
}

func writeTree(t *testing.T, defaultJSON string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if defaultJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "default.json"), []byte(defaultJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// 8451 事故锁死的一条：剥冗余的基线必须是 Default()+default.json，不能是裸 Default()。
// 场景：default.json 把端口设成别的实例的 8452，user.json 里的 8451 恰好**等于出厂默认**，
// 但它正是把这台实例的身份掰回来的一条 → 拿裸 Default() 比会把它当冗余删掉，
// 下次重启端口被 default.json 偷成 8452（撞端口崩死）。
func TestSaveUserPatchKeepsIdentityKeyThatEqualsFactoryDefault(t *testing.T) {
	root := writeTree(t, `{"server":{"port":8452}}`)
	port := float64(Default().Server.Port) // 出厂默认值本身
	if err := SaveUserPatch(root, map[string]interface{}{"server": map[string]interface{}{"port": port}}); err != nil {
		t.Fatal(err)
	}
	got := readMap(t, filepath.Join(root, "config", "user.json"))
	srv, _ := got["server"].(map[string]interface{})
	if srv == nil || srv["port"] == nil {
		t.Fatalf("等于出厂默认、但对抗 default.json 的键被删了（基线取错）：%v", got)
	}
	if srv["port"] != port {
		t.Errorf("值不该变：%v", srv["port"])
	}
}

// 真冗余（等于 Default() 且 default.json 没意见）应被剥掉，文件保持最小。
func TestSaveUserPatchStripsTrueRedundancy(t *testing.T) {
	root := writeTree(t, "")
	d := Default()
	patch := map[string]interface{}{"context": map[string]interface{}{
		"max_tokens":        float64(d.Context.MaxTokens), // = 出厂默认 → 剥
		"keep_recent_chars": float64(d.Context.KeepRecentChars) + 12345,
	}}
	if err := SaveUserPatch(root, patch); err != nil {
		t.Fatal(err)
	}
	got := readMap(t, filepath.Join(root, "config", "user.json"))
	ctx, _ := got["context"].(map[string]interface{})
	if ctx == nil {
		t.Fatal("context 段应存在（有真覆盖）")
	}
	if _, bad := ctx["max_tokens"]; bad {
		t.Errorf("等于出厂默认的键应被剥掉：%v", ctx)
	}
	if ctx["keep_recent_chars"] == nil {
		t.Errorf("真覆盖必须留着：%v", ctx)
	}
}

// 半截 JSON：拒绝保存，原文件一个字不动（旧实现按空基线合并，会把主人配置清空）。
func TestSaveUserPatchRefusesToWriteOverHalfWrittenJSON(t *testing.T) {
	root := writeTree(t, "")
	file := filepath.Join(root, "config", "user.json")
	half := `{"server":{"port":8451`
	if err := os.WriteFile(file, []byte(half), 0o644); err != nil {
		t.Fatal(err)
	}
	err := SaveUserPatch(root, map[string]interface{}{"tools": map[string]interface{}{"timeout_sec": float64(123)}})
	if err == nil {
		t.Fatal("看不懂的配置上必须拒绝写入")
	}
	b, _ := os.ReadFile(file)
	if string(b) != half {
		t.Errorf("原文件不该被动过：%q", b)
	}
}

// 首次保存（user.json 不存在）要能建出来，且嵌套补丁是深合并不是整段替换。
func TestSaveUserPatchFirstWriteAndDeepMerge(t *testing.T) {
	root := writeTree(t, "")
	file := filepath.Join(root, "config", "user.json")
	if err := SaveUserPatch(root, map[string]interface{}{"llm": map[string]interface{}{"model": "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserPatch(root, map[string]interface{}{"llm": map[string]interface{}{"base_url": "b"}}); err != nil {
		t.Fatal(err)
	}
	got := readMap(t, file)
	llm, _ := got["llm"].(map[string]interface{})
	if llm == nil || llm["model"] == nil || llm["base_url"] == nil {
		t.Fatalf("第二次补丁把第一段的兄弟键覆盖了：%v", got)
	}
}
