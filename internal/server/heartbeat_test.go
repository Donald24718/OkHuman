package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

// postJSON 向内存中的 App 发 POST 并解码响应（配置热更测试用）
func postJSON(ap http.Handler, path string, body interface{}, out interface{}) int {
	srv := httptest.NewServer(ap)
	defer srv.Close()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
	resp, err := srv.Client().Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// TestHeartbeatDueTable 空闲心跳触发判定（2026-10-17）：口径是"只有 run 结束才
// 重置计时"——正在 run 不触发；到点触发；未到点不触发；开关关不触发。
func TestHeartbeatDueTable(t *testing.T) {
	const min = int64(60000)
	// B 是"上次 run 结束"的基准时刻：口径是 run 结束才重置，所以用例一律
	// 以 B 为原点算偏移（last=0 是"计时基准未初始化"，另有单独用例覆盖）。
	const B = int64(1_000_000_000)
	cases := []struct {
		name   string
		now    int64
		last   int64
		cfg    config.HeartbeatCfg
		run    bool
		expect bool
	}{
		{"到点触发", B + 30*min, B, config.HeartbeatCfg{Enabled: true, IntervalMin: 30}, false, true},
		{"差一分钟不触发", B + 30*min - 1, B, config.HeartbeatCfg{Enabled: true, IntervalMin: 30}, false, false},
		{"正在 run 不计时", B + 90*min, B, config.HeartbeatCfg{Enabled: true, IntervalMin: 30}, true, false},
		{"开关关不触发", B + 999*min, B, config.HeartbeatCfg{Enabled: false, IntervalMin: 30}, false, false},
		{"run 刚结束即重置", B + 31*min, B + 30*min, config.HeartbeatCfg{Enabled: true, IntervalMin: 30}, false, false},
		{"间隔可配成 1 分钟", B + 2*min, B, config.HeartbeatCfg{Enabled: true, IntervalMin: 1}, false, true},
		{"间隔非法退回默认 30", B + 2*min, B, config.HeartbeatCfg{Enabled: true, IntervalMin: 0}, false, false},
		{"间隔非法但已过 30 分钟", B + 31*min, B, config.HeartbeatCfg{Enabled: true, IntervalMin: -5}, false, true},
		{"计时基准未初始化不触发", B + 999*min, 0, config.HeartbeatCfg{Enabled: true, IntervalMin: 30}, false, false},
	}
	for _, c := range cases {
		if got := heartbeatDue(c.now, c.last, c.cfg, c.run); got != c.expect {
			t.Errorf("%s: heartbeatDue=%v 期望 %v", c.name, got, c.expect)
		}
	}
}

// TestHeartbeatStatusNextDue /status 可观测字段：空闲时给下次触发时刻，正在 run
// 或开关关时给 null（"正在 run 不计时"语义）。
func TestHeartbeatStatusNextDue(t *testing.T) {
	cfg := config.HeartbeatCfg{Enabled: true, IntervalMin: 30}
	st := heartbeatStatus(cfg, 1000, false)
	if st["next_due_ms"] == nil {
		t.Fatalf("空闲时应给出下次触发时刻")
	}
	if got := st["next_due_ms"].(int64); got != 1000+30*60000 {
		t.Errorf("下次触发时刻=%v 期望 %v", got, 1000+30*60000)
	}
	if st["next_due_ms"] = nil; false {
	}
	if heartbeatStatus(cfg, 1000, true)["next_due_ms"] != nil {
		t.Errorf("正在 run 时下次触发时刻应为 null")
	}
	if heartbeatStatus(config.HeartbeatCfg{Enabled: false, IntervalMin: 30}, 1000, false)["next_due_ms"] != nil {
		t.Errorf("开关关时下次触发时刻应为 null")
	}
}

// TestHeartbeatConfigHotUpdate 心跳配置走 WebUI 同一条 /config 路径：合法值即时
// 生效（COW，无需重启）；interval_min 非法（0 / 超 1440）必须被拒——0 等于把
// agent 变成自循环烧 LLM，是这套功能最危险的误配。
func TestHeartbeatConfigHotUpdate(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	ap := New(st, nil)

	var out map[string]interface{}
	code := postJSON(ap, "/config", map[string]interface{}{
		"patch": map[string]interface{}{"heartbeat": map[string]interface{}{
			"enabled": true, "interval_min": 45, "prompt": "自定义心跳词",
		}},
	}, &out)
	if code != 200 {
		t.Fatalf("合法心跳配置被拒：code=%d body=%v", code, out)
	}
	hb := a.Cfg().Heartbeat
	if !hb.Enabled || hb.IntervalMin != 45 || hb.Prompt != "自定义心跳词" {
		t.Errorf("热更未生效：%+v", hb)
	}

	for _, bad := range []int{0, -1, 1441} {
		code := postJSON(ap, "/config", map[string]interface{}{
			"patch": map[string]interface{}{"heartbeat": map[string]interface{}{"interval_min": bad}},
		}, &out)
		if code != 400 {
			t.Errorf("interval_min=%d 应被拒（400），实际 %d", bad, code)
		}
	}
	if got := a.Cfg().Heartbeat.IntervalMin; got != 45 {
		t.Errorf("非法值不应改动已生效配置：现=%d 期望 45", got)
	}
}

// TestHeartbeatConfigVisibleToUI GET /config 必须把 heartbeat 段回给前端
// （WebUI 配置页按 CFG_SCHEMA 从 global 取值渲染；段缺失 → 三项控件全是空值）。
func TestHeartbeatConfigVisibleToUI(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	ap := New(st, nil)

	srv := httptest.NewServer(ap)
	defer srv.Close()
	var out map[string]interface{}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/config", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /config: %v", err)
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&out)

	global, _ := out["global"].(map[string]interface{})
	hb, ok := global["heartbeat"].(map[string]interface{})
	if !ok {
		t.Fatalf("/config 的 global 里没有 heartbeat 段：%v", global)
	}
	for _, k := range []string{"enabled", "interval_min", "prompt"} {
		if _, ok := hb[k]; !ok {
			t.Errorf("heartbeat 缺字段 %s（WebUI 对应控件会拿不到值）", k)
		}
	}
	if hb["prompt"] != config.DefaultHeartbeatPrompt {
		t.Errorf("默认心跳提示词未回传：%v", hb["prompt"])
	}
	// hot_sections 必须含 heartbeat：前端据此判断"即时生效"还是"需重启"
	if hs, ok := out["hot_sections"].([]interface{}); !ok || !containsStr(hs, "heartbeat") {
		t.Errorf("hot_sections 缺 heartbeat：%v", out["hot_sections"])
	}
}

func containsStr(xs []interface{}, want string) bool {
	for _, x := range xs {
		if s, ok := x.(string); ok && s == want {
			return true
		}
	}
	return false
}

// TestHeartbeatConfigTypeGuard 类型守卫（2026-10-18）：enabled / prompt 传错类型
// 必须 400 且不动已生效配置。不守卫的话 applyMap 的 json.Unmarshal 会跳过该键、
// 落回 Default()——enabled 静默变 false（心跳关掉）、prompt 静默换回出厂词，
// 调用方（尤其 agent 自己 curl）完全看不出来。
func TestHeartbeatConfigTypeGuard(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	ap := New(st, nil)

	var out map[string]interface{}
	code := postJSON(ap, "/config", map[string]interface{}{
		"patch": map[string]interface{}{"heartbeat": map[string]interface{}{
			"enabled": true, "interval_min": 12, "prompt": "自定义心跳词",
		}},
	}, &out)
	if code != 200 {
		t.Fatalf("合法补丁被拒：code=%d body=%v", code, out)
	}
	if ap2, ok := out["applied"].([]interface{}); !ok || len(ap2) == 0 {
		t.Errorf("合法补丁应报 applied 含 heartbeat，实际：%v", out)
	}

	bads := []map[string]interface{}{
		{"enabled": "yes"}, // 布尔写成字符串
		{"enabled": 1},     // 布尔写成数字
		{"prompt": 123},    // 字符串写成数字
		{"prompt": map[string]interface{}{"a": "b"}}, // 字符串写成对象
	}
	for _, bad := range bads {
		var o map[string]interface{}
		code := postJSON(ap, "/config", map[string]interface{}{
			"patch": map[string]interface{}{"heartbeat": bad},
		}, &o)
		if code != 400 {
			t.Errorf("补丁 %v 应被拒（400），实际 %d body=%v", bad, code, o)
		}
	}
	hb := a.Cfg().Heartbeat
	if !hb.Enabled || hb.IntervalMin != 12 || hb.Prompt != "自定义心跳词" {
		t.Errorf("非法补丁改动了已生效配置：%+v", hb)
	}
}
