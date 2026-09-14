package context

// ContextManager：会话状态 + 压缩编排（2026-08-24）
// - 每次 AddMessage 后检查总 token（system+summary+全部消息）：超 context.max_tokens
//   → 保留集 = 最近 keep_recent_chars 字符预算内的原文（至少最新 1 条），
//   保留集外（含旧 summary）交给 LLM 滚动压缩成 1 条 summary
// - 压缩批次原文先写存档文件（data.dir/session-records/）再压缩 → 细节零丢失，
//   新 summary 附"完整原文已存档"指引（LLM 可 cat 抓回）
// - 失败兜底：空闲超时重试 3 次仍失败 → 硬截断到 hard_trunc_chars；
//   其他错误（含 /stop 中断）→ 不重试、session 不变（下轮消息再试）
// - 4xx 回退阶梯（agent 包调用）：forceHardTruncate 直接硬截断不经 LLM
//
// 热更新（2026-09-03）：SetCfg/SetLLM/SetSystemPrompt 支持 POST /config 即时生效。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"okhuman/internal/llm"
	"okhuman/internal/types"
)

// CompressEvent 压缩事件（compress 事件广播与 /status 日志共用形状；
// JSON 字段名与 TS 版一致）
type CompressEvent struct {
	Scope       string
	Detail      string
	BeforeChars int
	AfterChars  int
	Rounds      int
	Text        string
	Thinking    *string
}

// Cfg 压缩参数（POST /config 热更新，对应 config.context）
type Cfg struct {
	MaxTokens       int
	KeepRecentChars int
	HardTruncChars  int
	StreamIdleMS    int
	CharsPerToken   float64
}

// Manager 会话状态 + 压缩编排
type Manager struct {
	mu       sync.Mutex
	changeMu sync.Mutex // 审计 H2 修（2026-09-14）：串行化"变更全程"
	//（AddMessage 的 append+压缩+落盘、ForceHardTruncate）。/inject 与在飞 run
	// 并发 AddMessage 时，旧代码两个压缩并行、各自按压缩前下标切 batch → 陈旧
	// 下标切错位置可能丢错消息；持锁后压缩串行、下标始终有效。
	llm          llm.LlmClient
	cfg          Cfg
	systemPrompt string
	session      *types.SessionState
	seq          int
	sessionNo    int
	recordDir    string
	runCtx       *context.Context // 当前 run 的中止信号（压缩流用；/stop 时连压缩一起中止）
	compressL    []func(CompressEvent)
	deltaL       []func(string, types.Delta)
	persistL     []func(*types.SessionState, int)
}

// NewManager 新建（空会话）
func NewManager(client llm.LlmClient, cfg Cfg, systemPrompt string, recordDir string) *Manager {
	return &Manager{
		llm:          client,
		cfg:          cfg,
		systemPrompt: systemPrompt,
		session:      &types.SessionState{Messages: []types.RawEntry{}},
		recordDir:    recordDir,
	}
}

// SetCfg 热更新压缩参数
func (m *Manager) SetCfg(cfg Cfg) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// SetLLM 热更新 LLM 客户端
func (m *Manager) SetLLM(client llm.LlmClient) {
	m.mu.Lock()
	m.llm = client
	m.mu.Unlock()
}

// SetSystemPrompt 热更新系统提示词
func (m *Manager) SetSystemPrompt(p string) {
	m.mu.Lock()
	m.systemPrompt = p
	m.mu.Unlock()
}

// SystemPrompt 当前系统提示词
func (m *Manager) SystemPrompt() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.systemPrompt
}

// snapshotLocked 当前会话的深快照（持锁调用）：Messages 为新切片（元素拷贝），
// Summary 共享指针（TEntry 一经创建不再原地修改）。调用方可长期持有快照并发
// 读取——与 AddMessage 的 append / 压缩的切片重切互不干扰（无共享可变结构）。
func (m *Manager) snapshotLocked() *types.SessionState {
	snap := &types.SessionState{ID: m.session.ID, Summary: m.session.Summary}
	snap.Messages = make([]types.RawEntry, len(m.session.Messages))
	copy(snap.Messages, m.session.Messages)
	return snap
}

// Session 当前会话状态（深快照；2026-09-14 审计修：旧实现返回活引用，
// /status /history 等只读端点无锁遍历 Messages 时与 AddMessage 的 append
// 竞态——-race 必报，撕裂的 slice header 极端情况下越界 panic）
func (m *Manager) Session() *types.SessionState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// CfgOf 当前压缩参数
func (m *Manager) CfgOf() Cfg {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// GetSeq / SetSeq 落盘序列号
func (m *Manager) GetSeq() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

func (m *Manager) SetSeq(n int) {
	m.mu.Lock()
	m.seq = n
	m.mu.Unlock()
}

// SetSessionNo 会话号（/reset 后由 main 更新；会话记录文件名用）
func (m *Manager) SetSessionNo(n int) {
	m.mu.Lock()
	m.sessionNo = n
	m.mu.Unlock()
}

// Restore 从落盘快照恢复会话状态（启动时）
func (m *Manager) Restore(state *types.SessionState) {
	m.mu.Lock()
	m.session = state
	if state.Messages == nil {
		m.session.Messages = []types.RawEntry{}
	}
	m.mu.Unlock()
}

// SetRunSignal 设置当前 run 的中止信号（压缩流用它；run 结束置 nil）
func (m *Manager) SetRunSignal(ctx *context.Context) {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()
}

// AddCompressListener 订阅压缩事件；返回注销函数
func (m *Manager) AddCompressListener(fn func(CompressEvent)) func() {
	m.mu.Lock()
	m.compressL = append(m.compressL, fn)
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.compressL = removeFn(m.compressL, fn)
		m.mu.Unlock()
	}
}

// AddCompressDeltaListener 订阅压缩思考流（compress_delta 事件）；返回注销函数
func (m *Manager) AddCompressDeltaListener(fn func(string, types.Delta)) func() {
	m.mu.Lock()
	m.deltaL = append(m.deltaL, fn)
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.deltaL = removeFn(m.deltaL, fn)
		m.mu.Unlock()
	}
}

// AddPersistListener 订阅会话变更（落盘监听）；返回注销函数
func (m *Manager) AddPersistListener(fn func(*types.SessionState, int)) func() {
	m.mu.Lock()
	m.persistL = append(m.persistL, fn)
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.persistL = removeFn(m.persistL, fn)
		m.mu.Unlock()
	}
}

// removeFn 从监听器列表移除 fn（返回新列表；函数类型不可 == 比较，用 reflect 指针）
func removeFn[T any](l []T, fn T) []T {
	fnPtr := reflect.ValueOf(fn).Pointer()
	out := make([]T, 0, len(l))
	for _, f := range l {
		if reflect.ValueOf(f).Pointer() != fnPtr {
			out = append(out, f)
		}
	}
	return out
}

// Compose 组装发给 LLM 的消息（主循环与压缩请求共用 → 前缀逐 token 一致）
func (m *Manager) Compose(opts *ComposeOptions) []types.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ComposeMessages(m.session, m.systemPrompt, opts)
}

// EstimateTotalTokens 总 token 估算：system + summary + 全部消息
func (m *Manager) EstimateTotalTokens() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.estimateTotalLocked()
}

// AddMessage 追加一条消息（user/assistant/tool）；追加后检查总 token 超阈值
// → 压最旧一批为 summary。压缩内部错误已自行兜底（硬截断/下轮再试），不返回 error。
func (m *Manager) AddMessage(e *types.RawEntry) {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	m.mu.Lock()
	m.session.Messages = append(m.session.Messages, *e)
	m.seq++
	m.mu.Unlock()

	m.maybeCompress()
	m.emitPersist()
}

// maybeCompress 压缩触发：总 token（含 system + summary）> max_tokens → 压最旧一批。
// 保留集 = 最近 keep_recent_chars 字符预算内的原文（至少保留最新 1 条）；
// 保留集外全在预算内且无 summary → 无可压（return）。
func (m *Manager) maybeCompress() {
	m.mu.Lock()
	if m.estimateTotalLocked() <= m.cfg.MaxTokens {
		m.mu.Unlock()
		return
	}
	n := len(m.session.Messages)
	if n == 0 {
		m.mu.Unlock()
		return // 只有 system → 超阈值不可能（system 固定）
	}
	if n == 1 {
		// 一条消息：最新一条不压。超阈值靠 summary → 压 summary（无 summary 则无可压）。
		if m.session.Summary != nil {
			m.mu.Unlock()
			m.doCompress(nil)
		} else {
			m.mu.Unlock()
		}
		return
	}
	// 字符预算：从最新往前累积 ≤ keep_recent_chars 的最大条数（至少 1）
	keep := 1
	acc := messageChars(m.session.Messages[n-1])
	for i := n - 2; i >= 0; i-- {
		c := messageChars(m.session.Messages[i])
		if acc+c > m.cfg.KeepRecentChars {
			break
		}
		acc += c
		keep++
	}
	batch := m.session.Messages[:n-keep]
	hasSummary := m.session.Summary != nil
	m.mu.Unlock()

	if len(batch) == 0 && !hasSummary {
		return // 全保留且无 summary → 无可压
	}
	m.doCompress(batch)
}

// estimateTotalLocked 估算总 token（持锁调用）
func (m *Manager) estimateTotalLocked() int {
	t := EstimateTokens(m.systemPrompt, m.cfg.CharsPerToken)
	if m.session.Summary != nil {
		t += EstimateTokens(m.session.Summary.Content, m.cfg.CharsPerToken)
	}
	for _, e := range m.session.Messages {
		t += EstimateMessageTokens(e, m.cfg.CharsPerToken)
	}
	return t
}

// doCompress 压缩 = 当前主循环完整前缀（含全部待压缩内容）+ 末尾一句压缩轮指令。
// 重试时 prefix 不变（session 未变）→ 重发同一压缩请求，前缀 KV 仍命中。
// 失败兜底：空闲超时重试共 3 次 → 仍失败 hardTruncate；空输出/其他错误 →
// session 不变（下一轮消息再试，原文已落盘不丢）。
func (m *Manager) doCompress(batch []types.RawEntry) {
	m.mu.Lock()
	recordPath := m.writeSessionRecordLocked()
	oldSummary := ""
	if m.session.Summary != nil {
		oldSummary = m.session.Summary.Content
	}
	beforeChars := sessionChars(m.session)
	prefix := ComposeMessages(m.session, m.systemPrompt, nil)
	streamIdleMS := m.cfg.StreamIdleMS
	m.mu.Unlock()

	const scope = "history"
	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		res, err := LoopCompress(m.runContext(), m.llmLocked(), prefix, scope, LoopCompressOpts{
			OnDelta: func(d types.Delta) { m.emitCompressDelta(scope, d) },
			IdleMS:  streamIdleMS,
		})
		if err != nil {
			if _, ok := err.(*llm.IdleTimeoutError); !ok {
				// 非空闲超时（/stop 中断 → ctx 取消；或其他错误）→ 不重试，session 不变
				m.emit(scope, "压缩失败："+err.Error(), oldSummary, 0, beforeChars, nil)
				return
			}
			// 空闲超时：第 3 次仍失败 → 硬截断兜底；否则重试
			if attempt >= maxAttempts {
				m.mu.Lock()
				m.hardTruncateLocked(recordPath)
				m.mu.Unlock()
				return
			}
			m.emit(scope, fmt.Sprintf("压缩空闲超时（%dms 无响应），重试 %d/%d", streamIdleMS, attempt, maxAttempts), oldSummary, 0, beforeChars, nil)
			continue
		}
		// 模型空输出 → 按失败处理（原文已落盘 session-records/，不丢）
		if res.Text == "" {
			m.emit(scope, "压缩失败：模型空输出", oldSummary, 0, beforeChars, nil)
			return
		}
		// 压缩成功：batch 出列，新 summary 替换旧 summary（尾部附记录文件路径）
		m.mu.Lock()
		if len(batch) > 0 && len(batch) <= len(m.session.Messages) {
			m.session.Messages = m.session.Messages[len(batch):]
		}
		ref := res.Text
		if recordPath != "[记录写入失败]" {
			ref += "\n\n[完整原文已存档：" + recordPath + " —— 需要被压掉的细节时用 bash cat/grep 抓回]"
		}
		m.session.Summary = &types.TEntry{Content: ref, CreatedAt: time.Now().UnixMilli()}
		m.mu.Unlock()
		m.emit(scope, fmt.Sprintf("%d 条最早消息%s → 1 条 summary（完整记录 %s）", len(batch), oldSummaryMark(oldSummary), recordPath), ref, res.Rounds, beforeChars, res.Thinking)
		return
	}
}

// oldSummaryMark 成功事件 detail 的"旧 summary"后缀
func oldSummaryMark(oldSummary string) string {
	if oldSummary != "" {
		return " + 旧 summary"
	}
	return ""
}

// emitCompressDelta 广播压缩流增量（WebUI "压缩中" 实时显示）
func (m *Manager) emitCompressDelta(scope string, d types.Delta) {
	m.mu.Lock()
	l := m.deltaL
	m.mu.Unlock()
	for _, fn := range l {
		fn(scope, d)
	}
}

// runContext 当前 run 的中止信号（无活跃 run 时用 Background）
func (m *Manager) runContext() context.Context {
	m.mu.Lock()
	ctx := m.runCtx
	m.mu.Unlock()
	if ctx != nil {
		return *ctx
	}
	return context.Background()
}

func (m *Manager) llmLocked() llm.LlmClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.llm
}

// ForceHardTruncate 兜底硬截断（LLM 4xx 三级阶梯第三级）：不经 LLM 压缩重试，
// 直接保留最近 hard_trunc_chars 字符，完整原文落 session-records/
func (m *Manager) ForceHardTruncate() {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	m.mu.Lock()
	recordPath := m.writeSessionRecordLocked()
	m.hardTruncateLocked(recordPath)
	m.mu.Unlock()
}

// hardTruncateLocked 硬截断（持锁）：压缩 3 次重试均空闲超时（或 4xx 阶梯）→
// 放弃 LLM 压缩，直接保留最近 hard_trunc_chars 字符预算内的原文，其余丢弃
// （完整内容已在 session-records/ 落盘，数据不丢）。summary 置为记录文件路径引用。
func (m *Manager) hardTruncateLocked(recordPath string) {
	before := sessionChars(m.session)
	n := len(m.session.Messages)
	keep := 1
	acc := 0
	if n > 0 {
		acc = messageChars(m.session.Messages[n-1])
	}
	for i := n - 2; i >= 0; i-- {
		c := messageChars(m.session.Messages[i])
		if acc+c > m.cfg.HardTruncChars {
			break
		}
		acc += c
		keep++
	}
	if n > 0 {
		m.session.Messages = m.session.Messages[n-keep:]
	}
	ref := "[完整会话原文已存档：" + recordPath + " —— 上下文压缩失败已硬截断，需要被截掉的细节时用 bash cat/grep 抓回]"
	if recordPath == "[记录写入失败]" {
		ref = "[完整会话原文已存档 —— 上下文压缩失败已硬截断，需要被截掉的细节时用 bash 抓回]"
	}
	m.session.Summary = &types.TEntry{Content: ref, CreatedAt: time.Now().UnixMilli()}
	m.emitLocked("history", fmt.Sprintf("压缩 3 次均无响应 → 硬截断保留最近 %d 字符（丢弃 %d 条，完整记录 %s）", acc, n-keep, recordPath), ref, 0, before, nil)
}

// writeSessionRecordLocked 压缩触发时生成会话完整记录
// （recordDir/session-records/ctx-<会话号>-<时间戳>-seq<N>.txt，持锁调用）。
// 内容 = 压缩前的完整会话（system 提示词 + summary + 全部消息原文）。
// 人类可读的纯文本；结构固定：压缩后总结置顶 → system 提示词 → 消息按时间序。
// 文件名毫秒精度 + seq 后缀（同毫秒不撞名）。
func (m *Manager) writeSessionRecordLocked() string {
	now := time.Now().UTC()
	ts := now.Format("2006-01-02_15-04-05-000") + "Z"
	iso := now.UTC().Format("2006-01-02T15:04:05.000Z")
	var lines []string
	lines = append(lines, fmt.Sprintf("# 会话完整记录 会话号=%d 生成=%s（单实例：agent 由落盘目录 okhuman-<port>/ 标识）", m.sessionNo, iso))
	if m.session.Summary != nil {
		lines = append(lines, "", "## 压缩后总结（LLM 滚动总结，压缩本批之前生成）", "", m.session.Summary.Content, "")
	}
	lines = append(lines, "## 系统提示词", "", m.systemPrompt, "")
	lines = append(lines, "## 消息（按时间序，压缩触发时的完整会话）")
	for _, e := range m.session.Messages {
		role := e.Role
		if e.Role == "tool" {
			role = "工具结果(" + e.ToolCallIDOrUnknown() + ")"
		}
		lines = append(lines, "", "### ["+role+"]")
		if e.ReasoningContent != nil {
			lines = append(lines, "（思考）", *e.ReasoningContent)
		}
		if s, ok := e.Content.(string); ok {
			lines = append(lines, s)
		} else if parts := types.AsParts(e.Content); parts != nil {
			nImg := 0
			refs := []string{}
			texts := []string{}
			for _, p := range parts {
				switch p.Type {
				case "image_url":
					nImg++
				case "inject_ref":
					refs = append(refs, p.Ref)
				case "text":
					texts = append(texts, p.Text)
				}
			}
			line := fmt.Sprintf("（附件 %s%s）", joinNonEmpty(refs), imgSuffix(nImg))
			if len(texts) > 0 {
				line += "\n" + strings.Join(texts, "\n")
			}
			lines = append(lines, line)
		}
		for _, tc := range e.ToolCalls {
			lines = append(lines, "（调用 "+tc.Function.Name+"）", tc.Function.Arguments)
		}
	}
	dir := filepath.Join(m.recordDir, "session-records")
	path := filepath.Join(dir, fmt.Sprintf("ctx-%d-%s-seq%d.txt", m.sessionNo, ts, m.seq))
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err == nil {
			return path
		}
	}
	return "[记录写入失败]"
}

func joinNonEmpty(refs []string) string {
	if len(refs) == 0 {
		return "×0"
	}
	return strings.Join(refs, ",")
}

func imgSuffix(nImg int) string {
	if nImg > 0 {
		return fmt.Sprintf("，图片 ×%d", nImg)
	}
	return ""
}

// emit 广播压缩事件（after_chars = text 长度；thinking 可空）
// emit 广播压缩事件（非持锁路径调用）
func (m *Manager) emit(scope, detail, text string, rounds, beforeChars int, thinking *string) {
	m.mu.Lock()
	m.emitLocked(scope, detail, text, rounds, beforeChars, thinking)
	m.mu.Unlock()
}

// emitLocked 广播压缩事件（持锁路径调用——sync.Mutex 非重入，持锁时禁用 emit）
func (m *Manager) emitLocked(scope, detail, text string, rounds, beforeChars int, thinking *string) {
	e := CompressEvent{
		Scope:       scope,
		Detail:      detail,
		BeforeChars: beforeChars,
		AfterChars:  utf16Len(text),
		Rounds:      rounds,
		Text:        text,
		Thinking:    thinking,
	}
	l := m.compressL
	for _, fn := range l {
		fn(e)
	}
}

// emitPersist 广播会话变更（落盘）。传深快照（2026-09-14 审计修：旧实现传
// 活指针，落盘监听器 json.Marshal 时与并发 AddMessage 的 append 竞态）
func (m *Manager) emitPersist() {
	m.mu.Lock()
	l := m.persistL
	state := m.snapshotLocked()
	seq := m.seq
	m.mu.Unlock()
	for _, fn := range l {
		fn(state, seq)
	}
}
