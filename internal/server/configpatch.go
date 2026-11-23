package server

import (
	"okhuman/internal/textutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"okhuman/internal/config"
	ctxmgr "okhuman/internal/context"
	"okhuman/internal/llm"
	"okhuman/internal/prompt"
	"okhuman/internal/tools"
)

type PatchResult struct {
	Applied []string
	Restart []string
}

// applyConfigPatch 把配置补丁应用到全局活对象 + 本实例 agent。
// 即时生效的段：llm（重建客户端）/ context（压缩参数）/
//
//	tools（fg_timeout_ms，runOpt 每轮取最新）/ doom（warn_after，每轮取最新）。
//	需重启的段：server / data / system_prompt。
//
// 返回 { applied: 即时生效的段, restart: 需要重启才生效的段 }。
func applyConfigPatch(a *AgentState, patch map[string]interface{}, logf func(format string, args ...interface{})) PatchResult {
	applied := []string{}
	restart := []string{}
	has := func(s string) bool { _, ok := patch[s]; return ok }

	if has("server") {
		restart = append(restart, "server（端口/地址，需重启进程）")
	}
	if has("data") {
		restart = append(restart, "data（落盘根目录，需重启进程）")
	}
	if has("system_prompt") {
		restart = append(restart, "system_prompt（提示词目录，需重启进程；单文件编辑请用提示词页）")
	}
	if has("llm") {
		applied = append(applied, "llm（重建 LLM 客户端，即时生效）")
	}
	if has("context") {
		applied = append(applied, "context（压缩参数，即时生效）")
	}
	if has("tools") {
		applied = append(applied, "tools（前台超时/命令总时长上限/结果上限，下条命令起生效）")
	}
	if has("doom") {
		applied = append(applied, "doom（死循环告警阈值，下轮运行生效）")
	}
	if has("heartbeat") {
		applied = append(applied, "heartbeat（开关/间隔/提示词，下一个巡检 tick ≤30s 内生效）")
	}

	// COW 热更新（2026-09-14 审计修：旧实现 ApplyPatchInPlace 原地改共享
	// *Config——runOpt / 只读端点的并发读与写入数据竞争，字符串字段撕裂读可
	// 致崩溃）：DeepMerge 生成全新不可变 Config，先更新从属对象（LLM 客户端 /
	// 压缩参数），最后原子换入 CfgRef——读方要么拿旧配置要么拿新配置，完整一致。
	old := a.Cfg()
	nc := config.DeepMerge(old, patch)
	if has("llm") && llmCfgOf(nc.LLM) != llmCfgOf(old.LLM) {
		client := llm.NewOpenAiClient(llmCfgOf(nc.LLM))
		client.SetLogger(func(m string) { logf("%s", m) }) // 热更重建的客户端也要有协议层异常留痕（畸形帧等）
		a.llmMu.Lock()
		a.LLM = client
		a.llmMu.Unlock()
		a.Agent().SetLLM(client)
		a.CM().SetLLM(client)
	}
	if has("context") {
		a.CM().SetCfg(ctxmgr.Cfg{
			MaxTokens:        nc.Context.MaxTokens,
			AttachmentTokens: nc.Context.AttachmentTokens,
			KeepRecentChars:  nc.Context.KeepRecentChars,
			HardTruncChars:   nc.Context.HardTruncChars,
			StreamIdleMS:     nc.Context.StreamIdleSec * 1000,
			CharsPerToken:    nc.Context.CharsPerToken,
			KeepSummaries:    nc.Context.KeepSummaries,
			SummaryMaxChars:  nc.Context.SummaryMaxChars,
		})
	}
	if has("tools") { // 工具超时热更新（2026-10-02）：重建工具表 + 新上限，下条命令生效
		tools.Configure(nc.Tools.FgTimeoutSec*1000, nc.Tools.TimeoutSec*1000)
	}
	// 自我生命感知输入随配置热更新（llm base_url / server 段变化）
	a.CM().SetSelfInfo(ctxmgr.SelfInfo{Port: nc.Server.Port, DataDir: nc.Data.Dir, LLMBaseURL: nc.LLM.BaseURL})
	tools.SetSelfID(nc.Server.Port, nc.Data.Dir) // 工具子进程的身份声明随配置一起走
	a.CfgRef.Store(nc)
	// tools / doom 段：由 runOpt 每轮现取，无需动作
	return PatchResult{Applied: dedupe(applied), Restart: dedupe(restart)}
}

// reloadAgentPrompt 加载提示词（WebUI 提示词页重载用）：
// 目录取自 a.PromptDir；无目录 → 保持现状（fallback 内置提示词）。
func reloadAgentPrompt(a *AgentState) prompt.Loaded {
	// promptMu 全程持锁（2026-09-14 审计修：SystemPrompt / PromptFiles /
	// PromptFallback 与只读端点（/status /prompts）的并发读竞态；重载含读盘
	// 阻塞，持锁时间略长但可接受——提示词页低频操作）
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	if a.PromptDir == "" {
		files := make([]prompt.File, 0, len(a.PromptFiles))
		for _, f := range a.PromptFiles {
			files = append(files, prompt.File{Name: f.Name, Chars: f.Chars})
		}
		return prompt.Loaded{Prompt: a.SystemPrompt, Files: files, Fallback: a.PromptFallback}
	}
	entries, err := os.ReadDir(a.PromptDir)
	if err != nil {
		r := prompt.Loaded{Prompt: prompt.DefaultSystemPrompt, Files: []prompt.File{}, Fallback: true}
		a.SystemPrompt = r.Prompt
		a.PromptFiles = nil
		a.PromptFallback = true
		a.CM().SetSystemPrompt(r.Prompt)
		a.Agent().SetSystemPrompt(r.Prompt)
		return r
	}
	var mdNames []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			mdNames = append(mdNames, e.Name())
		}
	}
	sort.Slice(mdNames, func(i, j int) bool { return prompt.NaturalCompare(mdNames[i], mdNames[j]) < 0 })
	files := make([]PromptFileInfo, 0, len(mdNames))
	chunks := make([]string, 0, len(mdNames))
	for _, name := range mdNames {
		data, err := os.ReadFile(filepath.Join(a.PromptDir, name))
		if err != nil {
			continue
		}
		text := string(data)
		files = append(files, PromptFileInfo{Name: name, Chars: textutil.UTF16Len(text)})
		chunks = append(chunks, text)
	}
	var p string
	if len(chunks) > 0 {
		p = strings.Join(chunks, "\n\n")
	} else {
		p = prompt.DefaultSystemPrompt
	}
	fallback := len(chunks) == 0
	a.SystemPrompt = p
	a.PromptFiles = files
	a.PromptFallback = fallback
	a.CM().SetSystemPrompt(p)
	a.Agent().SetSystemPrompt(p)
	pfiles := make([]prompt.File, 0, len(files))
	for _, f := range files {
		pfiles = append(pfiles, prompt.File{Name: f.Name, Chars: f.Chars})
	}
	return prompt.Loaded{Prompt: p, Files: pfiles, Fallback: fallback}
}

// ---------- App（对应 TS createApp） ----------

// Hooks /reset 重建会话后回调（重挂落盘监听）；配置页保存后回调（入口落盘 config/user.json 用）

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
