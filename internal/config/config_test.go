package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// configRoot 从本包回退到仓库根（internal/config → 根）
func configRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("定位仓库根失败：%v", err)
	}
	return root
}

// TestUserExampleIsValidJSON 模板必须可直接 cp 成 user.json 使用（README 快速开始就这么教）。
// 曾踩坑：模板顶部写了 // 注释行 → 严格 json.Unmarshal 报错 → 照 README 操作的用户起不来。
func TestUserExampleIsValidJSON(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(configRoot(t), "config", "user.json.example"))
	if err != nil {
		t.Fatalf("读模板失败：%v", err)
	}
	var patch map[string]interface{}
	if err := json.Unmarshal(data, &patch); err != nil {
		t.Fatalf("❌ 模板不是合法 JSON（不支持 // 注释）：%v", err)
	}
	// 未知字符串字段（如 _comment）必须是显式的说明字段，不能被当成配置段
	if v, ok := patch["_comment"]; ok {
		if _, ok := v.(string); !ok {
			t.Errorf("_comment 必须是字符串")
		}
	}
}

// TestExampleCoversDefaultToolsKeys default.json 里出现的可调项必须在模板里可见，
// 否则新增配置项容易被忘用户侧（AGENTS.md：新增配置项必须同步配置与默认值表）。
func TestExampleCoversDefaultToolsKeys(t *testing.T) {
	root := configRoot(t)
	read := func(name string) map[string]interface{} {
		data, err := os.ReadFile(filepath.Join(root, "config", name))
		if err != nil {
			t.Fatalf("读 %s 失败：%v", name, err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s 不是合法 JSON：%v", name, err)
		}
		return m
	}
	defTools, _ := read("default.json")["tools"].(map[string]interface{})
	exTools, _ := read("user.json.example")["tools"].(map[string]interface{})
	for k := range defTools {
		if _, ok := exTools[k]; !ok {
			t.Errorf("❌ config/user.json.example 缺少 tools.%s（default.json 有而模板没有，用户无从得知可调）", k)
		}
	}
}

// TestExampleRoundTripsThroughLoad 端到端：模板当 user.json 用必须能被加载且不改坏数据目录。
func TestExampleRoundTripsThroughLoad(t *testing.T) {
	root := configRoot(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "config"), 0755)
	for src, dst := range map[string]string{
		"default.json":      "default.json",
		"user.json.example": "user.json",
	} {
		data, err := os.ReadFile(filepath.Join(root, "config", src))
		if err != nil {
			t.Fatalf("读 %s 失败：%v", src, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config", dst), data, 0644); err != nil {
			t.Fatalf("写临时 %s 失败：%v", dst, err)
		}
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("❌ 模板当 user.json 时 Load 失败：%v", err)
	}
	if cfg.Tools.MaxToolRounds <= 0 {
		t.Errorf("max_tool_rounds 未生效：%d（应来自模板的 200）", cfg.Tools.MaxToolRounds)
	}
	if cfg.Data.Dir == "" {
		t.Errorf("data.dir 被模板污染为空串（模板不应写 data 段）")
	}
	// 默认价兜底必须仍在，确认 0 值没有被误写
	if cfg.Tools.TimeoutMS != Default().Tools.TimeoutMS {
		t.Errorf("timeout_ms = %d，与出厂默认 %d 不一致", cfg.Tools.TimeoutMS, Default().Tools.TimeoutMS)
	}
}

// TestEveryConfigKeyIn example/default 里不该出现 Config 结构体不认识的段名（防写错键名静默失效）。
func TestNoUnknownTopLevelSections(t *testing.T) {
	root := configRoot(t)
	known := map[string]bool{}
	rt := reflect.TypeOf(Config{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag != "" && tag != "-" {
			known[tag] = true
		}
	}
	known["_comment"] = true // 模板说明字段，故意不在结构体里
	for _, name := range []string{"default.json", "user.json.example"} {
		data, err := os.ReadFile(filepath.Join(root, "config", name))
		if err != nil {
			t.Fatalf("读 %s 失败：%v", name, err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s 非法 JSON：%v", name, err)
		}
		for k := range m {
			if !known[k] {
				t.Errorf("❌ %s 含未知顶层段 %q（拼写错误会被静默忽略，配置形同虚设）", name, k)
			}
		}
	}
}
