package config

// 配置：一切可调参数集中于此，零硬编码。
// 来源优先级：默认值 < config/default.json < config/user.json < 环境变量

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"okhuman/internal/persist"
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
	// AttachmentTokens：一个附件（图片/音频等非文本 part）按多少 token 计。
	// 直接按 token 记，不再用"固定 1000 字符"那种间接口径。0 = 不计附件。
	AttachmentTokens int `json:"attachment_tokens"`
	// KeepSummaries：上下文里保留几条历史总结（滑动窗口，超了从头部丢弃，不合并）。
	KeepSummaries int `json:"keep_summaries"`
	// SummaryMaxChars：单条总结正文的硬上限（utf16 计数，思考不计）。超出直接截断，
	// 并在注入时提醒模型这条是硬截断的。
	SummaryMaxChars int `json:"summary_max_chars"`
}

type ToolsCfg struct {
	FgTimeoutSec int `json:"fg_timeout_sec"` // 前台执行超时（秒，超时转后台）
	ResultLimit  int `json:"result_limit"`
	// TimeoutSec 命令总时长上限（秒）：超时 SIGKILL 整个命令进程组。
	// 转后台不重置此计时器——后台任务同样受约束（长驻服务须脱离进程组，写法见 tools 描述）。
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
// KnownKeys 某配置段允许写的键（取自结构体的 json tag）。POST /config 用它拒绝打错字的键：
// 未知键此前会被原样接受并落进 user.json，用户以为改了，其实什么都没生效
// （2026-11-13 四方审计 E3）。别名（*_ms）在调用前已由 NormalizeTimeUnits 换算掉。
func KnownKeys(section string) map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(Config{})
	for i := 0; i < rt.NumField(); i++ {
		if jsonName(rt.Field(i).Tag.Get("json")) != section {
			continue
		}
		st := rt.Field(i).Type
		if st.Kind() == reflect.Pointer {
			st = st.Elem()
		}
		if st.Kind() != reflect.Struct {
			return out // 非对象段：不校验（返回空集 = 调用方跳过）
		}
		// 段里若有嵌套结构，这里只列到一层、嵌套键会被误拒 → 整段跳过校验。
		// 现状所有段都是扁平的；真要加嵌套时，改成生成 a.b 再放开。
		for j := 0; j < st.NumField(); j++ {
			if k := st.Field(j).Type.Kind(); k == reflect.Struct || k == reflect.Map {
				return map[string]bool{}
			}
		}
		for j := 0; j < st.NumField(); j++ {
			if n := jsonName(st.Field(j).Tag.Get("json")); n != "" {
				out[n] = true
			}
		}
		return out
	}
	return out
}

func jsonName(tag string) string {
	if tag == "" || tag == "-" {
		return ""
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		return tag[:i]
	}
	return tag
}

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
		Context:      ContextCfg{MaxTokens: 150000, KeepRecentChars: 60000, HardTruncChars: 150000, StreamIdleSec: 90, CharsPerToken: 1.5, AttachmentTokens: 500, KeepSummaries: 5, SummaryMaxChars: 30000},
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
			return nil, fmt.Errorf("配置 %s 解析失败：%w", f, err)
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
		n, err := parseInt(v)
		if err != nil {
			// 不能静默忽略：写错端口却照常起，人只会看到"实例跑到别的端口上了"
			// （2026-11-12 审查：parseInt 修严之后，旧毛病从"当成 8451"变成"整个变量被忽略"）。
			fmt.Fprintf(os.Stderr, "[config] OKHUMAN_PORT=%q 不是端口号，已忽略（保持 %d）\n", v, cfg.Server.Port)
		} else {
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

// IntervalMinOrDefault 空闲心跳阈值；未配或配成非正数时退回默认值。
// 两处消费方（/status 可观测字段、触发判定）此前各写一遍 "if <=0 取默认"，
// 默认值一改就只改一边（2026-11-12 归并）。
func (h HeartbeatCfg) IntervalMinOrDefault() int {
	if h.IntervalMin <= 0 {
		return Default().Heartbeat.IntervalMin
	}
	return h.IntervalMin
}

func parseInt(s string) (int, error) {
	// 不用 fmt.Sscanf：它会把尾部垃圾吞掉（"8451x" 当成 8451），端口这类值写错
	// 就会静默绑到别处（2026-11-12 审查）。
	return strconv.Atoi(strings.TrimSpace(s))
}

// DeepMerge 把 patch 深合并进 cfg（热更新 / 补丁落盘共用）。
// 返回新 Config（不修改原对象）；patch 非对象值直接覆盖。
func DeepMerge(cfg *Config, patch map[string]interface{}) *Config {
	NormalizeTimeUnits(patch)
	return deepMergeConfig(cfg, patch)
}

// 并发：本函数的读-改-写不是原子的，**调用方负责串行化**（main.cfgSaveMu 持有）。
// SaveUserPatch 把补丁合并进 <root>/config/user.json —— 热更新唯一的落盘路径。
// 这里三条规则都是踩过坑加的（2026-11-12 从 main 挪进来：规则住在 main 里时，
// config 的测试覆盖不到，而它决定的正是"下次重启生效的是什么"）：
//  1. 读不懂就不写：user.json 是半截 JSON（外部编辑中断/掉电写一半）时拒绝保存。
//     旧写法静默按空基线合并写回，会把主人的端口/模型/上下文上限一次清空 ——
//     8451 那次"端口被偷"就是这个形态。
//  2. 剥冗余叶子的基线 = Default() + config/default.json，不是裸 Default()（2026-09-15 修）：
//     default.json 覆盖过的键，user 里"等于出厂默认"的值正是把它掰回来的覆盖，删掉即
//     静默改变生效配置（实例身份被偷成别的端口 → 重启撞端口崩死）。
//  3. 原子写，错误如实返回（调用方必须处理，不能只说"已保存"）。
//
// 时间单位不在这里换算：调用方先过 DeepMerge，它对传入 patch 就地 NormalizeTimeUnits，
// 所以到这里的键已是 *_sec。
func SaveUserPatch(root string, patch map[string]interface{}) error {
	file := filepath.Join(root, "config", "user.json")
	var cur map[string]interface{}
	raw, err := os.ReadFile(file)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("config/user.json 读不了，拒绝保存（以免覆盖原配置）：%w", err)
		}
		// 文件不存在 = 首次保存，空基线是对的
	} else {
		var d interface{}
		if uerr := json.Unmarshal(raw, &d); uerr != nil {
			return fmt.Errorf("config/user.json 解析失败（%w），拒绝在看不懂的配置上合并写入：%s", uerr, file)
		}
		m, ok := d.(map[string]interface{})
		if !ok {
			return fmt.Errorf("config/user.json 顶层不是对象，拒绝写入：%s", file)
		}
		cur = m
	}
	next := map[string]interface{}{}
	mergeMap(next, cur)
	mergeMap(next, patch)

	var base map[string]interface{}
	if b, err := json.Marshal(Default()); err == nil {
		var m map[string]interface{}
		if json.Unmarshal(b, &m) == nil {
			base = m
		}
	}
	if s, err := os.ReadFile(filepath.Join(root, "config", "default.json")); err == nil {
		var ds map[string]interface{}
		if json.Unmarshal(s, &ds) == nil {
			mergeMap(base, ds)
		}
	}
	for secName, sec := range next {
		secM, ok := sec.(map[string]interface{})
		if !ok {
			continue
		}
		bsec, _ := base[secName].(map[string]interface{})
		for k, v := range secM {
			if bv, exists := bsec[k]; exists && jsonEqualRaw(v, bv) {
				delete(secM, k)
			}
		}
		if len(secM) == 0 {
			delete(next, secName)
		}
	}
	return persist.WriteJSONPath(file, next)
}

// jsonEqualRaw 两值 JSON 相等（同类型同值）。只用于上面"这个键的生效值和基线一样吗"，
// 两个入参都来自 JSON 解码后的 map，所以按序列化结果比是等价的。
func jsonEqualRaw(a, b interface{}) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
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
	// 附件 token 成本：负数按 0（不计）处理；0 是合法值，不回填默认。
	if ctx.AttachmentTokens < 0 {
		ctx.AttachmentTokens = 0
	}
}
