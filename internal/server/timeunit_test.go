package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"okhuman/internal/config"
	"okhuman/internal/llm"
	"strings"
	"testing"
)

// TestConfigPostTimeUnitMigration POST /config 收到旧 *_ms 键（旧 webui / 旧脚本）
// 必须换算成秒并生效，不能"接受但落回默认值"；响应里回显换算说明。
func TestConfigPostTimeUnitMigration(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	ap := New(NewAppState(cfg, t.TempDir(), a), nil)

	var out map[string]interface{}
	code := postJSON(ap, "/config", map[string]interface{}{
		"patch": map[string]interface{}{
			"tools": map[string]interface{}{"timeout_ms": 1200000, "fg_timeout_ms": 45000},
			"llm":   map[string]interface{}{"stream_idle_ms": 60000},
		},
	}, &out)
	if code != 200 {
		t.Fatalf("旧毫秒键补丁被拒：code=%d body=%v", code, out)
	}
	if notes, _ := out["unit_notes"].([]interface{}); len(notes) != 3 {
		t.Errorf("应回显 3 条换算说明，得 %v", out["unit_notes"])
	}
	c := a.Cfg()
	if c.Tools.TimeoutSec != 1200 || c.Tools.FgTimeoutSec != 45 {
		t.Errorf("tools 换算未生效：%+v", c.Tools)
	}
	if c.LLM.StreamIdleSec != 60 {
		t.Errorf("llm.stream_idle_sec 换算未生效：%d", c.LLM.StreamIdleSec)
	}
	// 回显的 map 不应再带废弃 *_ms 键
	b := httpGet(t, ap, "/config")
	var echo map[string]interface{}
	_ = json.Unmarshal([]byte(b), &echo)
	if _, ok := echo["global"].(map[string]interface{})["tools"].(map[string]interface{})["timeout_ms"]; ok {
		t.Error("回显里仍有 tools.timeout_ms")
	}
	if strings.Contains(b, "_ms") {
		t.Errorf("GET /config 回显里仍有 *_ms 键：%s", b)
	}
}

// TestConfigPostSecRangeGuard 秒键的范围守卫：把旧毫秒数值填进秒键（手滑最常见形态）
// 必须 400，而不是接受一个 3.5 天的超时。
func TestConfigPostSecRangeGuard(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	ap := New(NewAppState(cfg, t.TempDir(), a), nil)
	var out map[string]interface{}
	for _, bad := range []int{0, -5, 86401} {
		code := postJSON(ap, "/config", map[string]interface{}{
			"patch": map[string]interface{}{"tools": map[string]interface{}{"timeout_sec": bad}},
		}, &out)
		if code != 400 {
			t.Errorf("tools.timeout_sec=%d 应被拒（400），实际 %d body=%v", bad, code, out)
		}
	}
	for _, bad := range []int{0, 3601} {
		code := postJSON(ap, "/config", map[string]interface{}{
			"patch": map[string]interface{}{"llm": map[string]interface{}{"stream_idle_sec": bad}},
		}, &out)
		if code != 400 {
			t.Errorf("llm.stream_idle_sec=%d 应被拒（400），实际 %d", bad, code)
		}
	}
	if got := a.Cfg().Tools.TimeoutSec; got != config.Default().Tools.TimeoutSec {
		t.Errorf("非法值不应改动已生效配置：现=%d", got)
	}
}

// httpGet 取端点原文（断言回显 map 的键名）
func httpGet(t *testing.T, ap http.Handler, path string) string {
	t.Helper()
	srv := httptest.NewServer(ap)
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
