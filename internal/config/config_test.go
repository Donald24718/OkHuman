package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeTimeUnits 旧毫秒键 → 新秒键（2026-10-18 单位统一）：
// 换算向上取整、旧键剔除；新旧键并存时新键优先；非数值旧键忽略。
func TestNormalizeTimeUnits(t *testing.T) {
	patch := map[string]interface{}{
		"llm":     map[string]interface{}{"timeout_ms": float64(300000)},
		"context": map[string]interface{}{"stream_idle_ms": float64(90000)},
		"tools": map[string]interface{}{
			"fg_timeout_ms": float64(30000),
			"timeout_ms":    float64(60000000),
		},
	}
	notes := NormalizeTimeUnits(patch)
	if len(notes) != 4 {
		t.Fatalf("应有 4 条换算说明，得 %d：%v", len(notes), notes)
	}
	get := func(sec, key string) interface{} {
		return patch[sec].(map[string]interface{})[key]
	}
	if got := get("llm", "timeout_sec"); got != float64(300) {
		t.Errorf("llm.timeout_sec = %v，期望 300", got)
	}
	if got := get("context", "stream_idle_sec"); got != float64(90) {
		t.Errorf("context.stream_idle_sec = %v，期望 90", got)
	}
	if got := get("tools", "fg_timeout_sec"); got != float64(30) {
		t.Errorf("tools.fg_timeout_sec = %v，期望 30", got)
	}
	if got := get("tools", "timeout_sec"); got != float64(60000) {
		t.Errorf("tools.timeout_sec = %v，期望 60000", got)
	}
	for sec, oldKey := range map[string]string{"llm": "timeout_ms", "context": "stream_idle_ms", "tools": "fg_timeout_ms"} {
		if _, ok := patch[sec].(map[string]interface{})[oldKey]; ok {
			t.Errorf("%s.%s 未被剔除（落盘会带上废弃键）", sec, oldKey)
		}
	}

	// 新旧并存：新键优先，旧键丢弃并说明
	p2 := map[string]interface{}{"llm": map[string]interface{}{"timeout_ms": float64(300000), "timeout_sec": float64(45)}}
	n2 := NormalizeTimeUnits(p2)
	if got := p2["llm"].(map[string]interface{})["timeout_sec"]; got != float64(45) {
		t.Errorf("新旧并存时新键应优先，得 %v", got)
	}
	if len(n2) != 1 || !strings.Contains(n2[0], "已废弃") {
		t.Errorf("应说明旧键被忽略，得 %v", n2)
	}

	// 向上取整：500ms → 1s（不被抹成 0）
	p3 := map[string]interface{}{"llm": map[string]interface{}{"timeout_ms": float64(500)}}
	NormalizeTimeUnits(p3)
	if got := p3["llm"].(map[string]interface{})["timeout_sec"]; got != float64(1) {
		t.Errorf("500ms 应向上取整为 1s，得 %v", got)
	}

	// 非数值旧键：忽略并说明，不 panic
	p4 := map[string]interface{}{"tools": map[string]interface{}{"timeout_ms": "600000"}}
	n4 := NormalizeTimeUnits(p4)
	if _, ok := p4["tools"].(map[string]interface{})["timeout_ms"]; ok {
		t.Error("非数值旧键应被剔除")
	}
	if len(n4) != 1 {
		t.Errorf("应有说明，得 %v", n4)
	}
}

// TestLoadMigratesLegacyMsFiles 旧配置文件（还写着 *_ms）不改也能跑：Load 自动换算，
// 语义与改单位前一致。这是单位切换最大的坑——旧 timeout_ms=300000 若被当秒就是 3.5 天。
func TestLoadMigratesLegacyMsFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{
  "llm": {"base_url": "http://127.0.0.1:9/v1", "model": "m", "timeout_ms": 120000},
  "context": {"stream_idle_ms": 45000},
  "tools": {"fg_timeout_ms": 15000, "timeout_ms": 900000}
}`
	if err := os.WriteFile(filepath.Join(root, "config", "user.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.TimeoutSec != 120 {
		t.Errorf("LLM.TimeoutSec = %d，期望 120", cfg.LLM.TimeoutSec)
	}
	if cfg.Context.StreamIdleSec != 45 {
		t.Errorf("Context.StreamIdleSec = %d，期望 45", cfg.Context.StreamIdleSec)
	}
	if cfg.Tools.FgTimeoutSec != 15 || cfg.Tools.TimeoutSec != 900 {
		t.Errorf("Tools = %+v，期望 fg 15 / total 900", cfg.Tools)
	}
	// 未出现的键保持出厂默认（A5 新增项）
	if cfg.LLM.StreamIdleSec != 120 {
		t.Errorf("LLM.StreamIdleSec = %d，期望出厂默认 120", cfg.LLM.StreamIdleSec)
	}
}

// TestApplyPatchInPlaceMigrates 热更新路径也过换算（旧 webui/旧脚本发旧键不会落回默认值）。
func TestApplyPatchInPlaceMigrates(t *testing.T) {
	cfg := Default()
	ApplyPatchInPlace(cfg, map[string]interface{}{"tools": map[string]interface{}{"timeout_ms": float64(1200000)}})
	if cfg.Tools.TimeoutSec != 1200 {
		t.Errorf("Tools.TimeoutSec = %d，期望 1200（旧毫秒键应换算）", cfg.Tools.TimeoutSec)
	}
	// 换算后的 map 再落盘不应带废弃键
	m := cfgToMap(cfg)
	if _, ok := m["tools"].(map[string]interface{})["timeout_ms"]; ok {
		t.Error("cfgToMap 不应带 timeout_ms")
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "_ms") {
		t.Errorf("落盘 map 里仍有 *_ms 键：%s", b)
	}
}
