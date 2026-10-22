package config

// 配置：一切可调参数集中于此，零硬编码。
// 来源优先级：默认值 < config/default.json < config/user.json < 环境变量

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// Config 全部可调参数
type Config struct {
	Server       ServerCfg       `json:"server"`
	LLM          LLMCfg          `json:"llm"`
	SystemPrompt SystemPromptCfg `json:"system_prompt"`
	Data         DataCfg         `json:"data"`
	Context      ContextCfg      `json:"context"`
	Tools        ToolsCfg        `json:"tools"`
	Doom         DoomCfg         `json:"doom"`
	Heartbeat    HeartbeatCfg    `json:"heartbeat"`
}

type ServerCfg struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type LLMCfg struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
	// TimeoutSec 非流式 complete 的整体请求超时（秒）。
	TimeoutSec int `json:"timeout_sec"`
	// StreamIdleSec 主循环流式空闲超时（秒，2026-10-18 A5）：连续这么久没有任何
	// wire 字节（含 ": ping" 注释帧）→ 判空闲超时、本轮报错收尾。此前主循环
	// 没有可配口径：agent 传 idleMs=0，客户端回落到 max(timeout_ms, 10 分钟)，
	// 后端卡住就白挂 10 分钟，且与压缩路径的 context.stream_idle_sec 口径不一致。
	StreamIdleSec int `json:"stream_idle_sec"`
	// MaxResponseBytes LLM 单次响应总量上限（字节，2026-10-18 健壮性修 A）：
	// 入站方向此前只有两处封顶（工具输出 32MB、HTTP 请求体 256MB），唯独"LLM
	// 往回吐"这一路不设防——正文/思考/工具参数逐块累加、SSE 单行无上限、非流式
	// 直接 Decode 整个 body。上游（代理层/后端）出 bug 或被换掉时疯狂吐内容 →
	// 本进程内存无界增长（数据目录在 tmpfs，等于两头一起吃）。空闲计时器帮不上：
	// 它每有字节就重置，慢速无限流可以永远跑。0 → 用内置默认 256MB。
	MaxResponseBytes int64 `json:"max_response_bytes"`
}

type SystemPromptCfg struct {
	Dir string `json:"dir"`
}

type DataCfg struct {
	Dir string `json:"dir"`
}

type ContextCfg struct {
	MaxTokens       int     `json:"max_tokens"`
	KeepRecentChars int     `json:"keep_recent_chars"`
	HardTruncChars  int     `json:"hard_trunc_chars"`
	StreamIdleSec   int     `json:"stream_idle_sec"` // 压缩流空闲超时（秒）
	CharsPerToken   float64 `json:"chars_per_token"`
}

type ToolsCfg struct {
	FgTimeoutSec int `json:"fg_timeout_sec"` // 前台执行超时（秒，超时转后台）
	ResultLimit  int `json:"result_limit"`
	// TimeoutSec 命令总时长上限（秒）：超时 SIGKILL 整个命令进程组。
	// 转后台不重置此计时器——后台任务同样受约束（长驻服务须 setsid 脱离）。
	TimeoutSec int `json:"timeout_sec"`
}

type DoomCfg struct {
	WarnAfter int `json:"warn_after"`
}

// HeartbeatCfg 空闲心跳（2026-10-17 用户定）：agent 持续一段时间没有 run → 系统
// 主动把 Prompt 作为一条消息入队并开启一轮 run。
//
// 计时口径（用户明确）：**只有 run 结束才重置计时**——正在 run 期间不计时，run
// 一结束重新开始计；期间任何 run 结束都会把计时器归零。默认 Enabled=false
// （不擅自改变现有行为，WebUI 配置页可开）。
type HeartbeatCfg struct {
	Enabled     bool   `json:"enabled"`
	IntervalMin int    `json:"interval_min"` // 空闲多少分钟后触发（<=0 按默认）
	Prompt      string `json:"prompt"`       // 触发时发给模型的提示词
}

// DefaultHeartbeatPrompt 心跳提示词出厂默认（WebUI 里可改，存 config/user.json）。
// 措辞要点：① 明确"这是系统主动开启、不是主人发的消息"——防止模型把心跳当指令
// 来源误判；② 先自查有无该继续/收尾的事，没有就停下；③ 禁止为找事做而做无关
// 操作、禁止主动对外联系（空闲时最容易出越权动作）。
const DefaultHeartbeatPrompt = "（心跳：系统检测到你已空闲——最后一次 run 结束至今没有新的 run，现在由系统主动开启一轮。这不是主人发的消息，不构成新任务授权。请自查：挂起的后台任务是否出了结果、之前接下的活是否收尾、有没有明显该推进的下一步、有没有需要验证的结果。有就继续做；没有就一句话说明当前状态然后停下。不要为了找事做而执行无关操作，不要重复已完成的事，不要主动对外联系任何人。）"

// Default 出厂默认（2026-09-14 易迁移：data 目录默认 $HOME/.okhuman；
// 部署可用 config/default.json 或 OKHUMAN_DATA_DIR 环境变量覆盖）
func Default() *Config {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	return &Config{
		Server:       ServerCfg{Host: "127.0.0.1", Port: 8451},
		LLM:          LLMCfg{BaseURL: "http://127.0.0.1:8080/v1", Model: "local", TimeoutSec: 300, StreamIdleSec: 120, MaxResponseBytes: 256 << 20},
		SystemPrompt: SystemPromptCfg{Dir: "prompts"},
		Data:         DataCfg{Dir: filepath.Join(home, ".okhuman")},
		Context:      ContextCfg{MaxTokens: 150000, KeepRecentChars: 60000, HardTruncChars: 150000, StreamIdleSec: 90, CharsPerToken: 1.5},
		Tools:        ToolsCfg{FgTimeoutSec: 30, ResultLimit: 10000, TimeoutSec: 600},
		Doom:         DoomCfg{WarnAfter: 3},
		Heartbeat:    HeartbeatCfg{Enabled: false, IntervalMin: 30, Prompt: DefaultHeartbeatPrompt},
	}
}

// Load 读 default.json + user.json 深合并，环境变量覆盖。
// JSON 损坏 → 抛错（配置必须显式正确）。
func Load(root string) (*Config, error) {
	cfg := Default()
	for _, f := range []string{"config/default.json", "config/user.json"} {
		p := filepath.Join(root, filepath.FromSlash(f))
		data, err := os.ReadFile(p)
		if err != nil {
			continue // 文件不存在则跳过
		}
		var patch map[string]interface{}
		if err := json.Unmarshal(data, &patch); err != nil {
			return nil, fmt.Errorf("配置 %s 解析失败：%v", f, err)
		}
		for _, n := range NormalizeTimeUnits(patch) {
			fmt.Fprintf(os.Stderr, "[config] 单位迁移：%s\n", n)
		}
		cfg = deepMergeConfig(cfg, patch)
	}
	migrateContext(&cfg.Context)
	// 环境变量覆盖（便于临时切换 LLM 后端 / 落盘目录）
	if v := os.Getenv("OKHUMAN_LLM_BASE_URL"); v != "" {
		cfg.LLM.BaseURL = v
	}
	if v := os.Getenv("OKHUMAN_LLM_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("OKHUMAN_PORT"); v != "" {
		if n, err := parseInt(v); err == nil {
			cfg.Server.Port = n
		}
	}
	if v := os.Getenv("OKHUMAN_DATA_DIR"); v != "" {
		cfg.Data.Dir = v
	}
	return cfg, nil
}

// msToSec 旧毫秒键 → 新秒键（2026-10-18：配置里所有毫秒单位统一改秒）。
// 段 → { 旧键: 新键 }。
var msToSec = map[string]map[string]string{
	"llm":     {"timeout_ms": "timeout_sec", "stream_idle_ms": "stream_idle_sec"},
	"context": {"stream_idle_ms": "stream_idle_sec"},
	"tools":   {"fg_timeout_ms": "fg_timeout_sec", "timeout_ms": "timeout_sec"},
}

// NormalizeTimeUnits 把补丁 map 里的旧 *_ms 键换算成对应 *_sec 键（向上取整，
// 0.5s 不会被抹成 0），并剔除旧键；返回人类可读的换算说明（调用方打日志/回给前端）。
// 两条路都要过：Load 读 config/*.json（旧文件不改也能跑），以及 POST /config 热更新
// （旧 webui/旧脚本发旧键不会静默落回默认值——这是单位切换最大的坑：
// 旧 timeout_ms=300000 若被当成秒就是 3.5 天）。
// 新旧键同时出现时以新键为准，旧键丢弃并说明。
func NormalizeTimeUnits(patch map[string]interface{}) []string {
	var notes []string
	for section, keys := range msToSec {
		sec, ok := patch[section].(map[string]interface{})
		if !ok {
			continue
		}
		for oldK, newK := range keys {
			v, ok := sec[oldK]
			if !ok {
				continue
			}
			f, ok2 := v.(float64)
			if !ok2 {
				notes = append(notes, fmt.Sprintf("%s.%s 已废弃且不是数值 → 忽略", section, oldK))
				delete(sec, oldK)
				continue
			}
			converted := int(math.Ceil(f / 1000))
			if _, has := sec[newK]; has {
				notes = append(notes, fmt.Sprintf("%s.%s 已废弃 → 忽略（同补丁里已有 %s.%s）", section, oldK, section, newK))
			} else {
				sec[newK] = float64(converted)
				notes = append(notes, fmt.Sprintf("%s.%s=%.0f（毫秒）→ %s.%s=%d（秒）", section, oldK, f, section, newK, converted))
			}
			delete(sec, oldK)
		}
	}
	return notes
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// DeepMerge 把 patch 深合并进 cfg（热更新 / 补丁落盘共用）。
// 返回新 Config（不修改原对象）；patch 非对象值直接覆盖。
func DeepMerge(cfg *Config, patch map[string]interface{}) *Config {
	NormalizeTimeUnits(patch)
	merged := cfgToMap(cfg)
	mergeMap(merged, patch)
	out := Default()
	applyMap(out, merged)
	return out
}

// ApplyPatchInPlace 热更新用：patch 深合并进 cfg 自身（单实例全局活对象语义）
func ApplyPatchInPlace(cfg *Config, patch map[string]interface{}) {
	NormalizeTimeUnits(patch)
	merged := cfgToMap(cfg)
	mergeMap(merged, patch)
	applyMap(cfg, merged)
}

// ---------- map 层深合并（与 TS deepMerge 同语义） ----------

func mergeMap(dst, patch map[string]interface{}) {
	for k, v := range patch {
		if pm, ok := v.(map[string]interface{}); ok {
			if dm, ok := dst[k].(map[string]interface{}); ok {
				mergeMap(dm, pm)
				continue
			}
		}
		dst[k] = v
	}
}

func cfgToMap(cfg *Config) map[string]interface{} {
	data, _ := json.Marshal(cfg)
	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	return m
}

func applyMap(cfg *Config, m map[string]interface{}) {
	data, _ := json.Marshal(m)
	_ = json.Unmarshal(data, cfg)
}

func deepMergeConfig(base *Config, patch map[string]interface{}) *Config {
	merged := cfgToMap(base)
	mergeMap(merged, patch)
	out := Default()
	applyMap(out, merged)
	return out
}

// MigrateContext context 段旧形状迁移（幂等）：
// 旧条数键（keep_recent / t1_trigger / t1_batch / t2_trigger / char_limit）与
// compress_thinking 已废弃；map 层清理在 Load/热更新路径完成（见 Patch 校验），
// 此处仅兜底类型校验。
func migrateContext(ctx *ContextCfg) {
	if ctx.MaxTokens <= 0 {
		ctx.MaxTokens = Default().Context.MaxTokens
	}
	if ctx.CharsPerToken <= 0 {
		ctx.CharsPerToken = Default().Context.CharsPerToken
	}
}

// DiffAgainstBase 补丁差集（2026-08-30）：patch 中"与 base 值相同"的叶子键剔除。
// 只处理二层 {section: {key: value}} 对象。用途：user.json 落盘保持最小。
func DiffAgainstBase(base *Config, patch map[string]interface{}) map[string]interface{} {
	baseMap := cfgToMap(base)
	out := map[string]interface{}{}
	for s, secVal := range patch {
		sec, ok := secVal.(map[string]interface{})
		if !ok {
			continue
		}
		bsec, _ := baseMap[s].(map[string]interface{})
		if bsec == nil {
			bsec = map[string]interface{}{}
		}
		kept := map[string]interface{}{}
		for k, v := range sec {
			if !jsonEqual(bsec[k], v) {
				kept[k] = v
			}
		}
		if len(kept) > 0 {
			out[s] = kept
		}
	}
	return out
}

func jsonEqual(a, b interface{}) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	return aerr == nil && berr == nil && string(ab) == string(bb)
}
