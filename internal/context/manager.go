package context

// ContextManager：会话状态 + 压缩编排（2026-08-24）
// - 每次 AddMessage 后检查总 token（system+summary+全部消息）：超 context.max_tokens
//   → 保留集 = 最近 keep_recent_chars 字符预算内的原文（至少最新 1 条），
//   保留集外（含旧 summary）交给 LLM 滚动压缩成 1 条 summary
// - 压缩批次原文先写存档文件（data.dir/session-records/）再压缩 → 细节零丢失，
//   压缩后总结文本另存 data.dir/session-summaries/（与存档同名基座、不同文件夹配对，
//   2026-09-25）；新 summary 附两行指针：总结文本文件 + 存档文件
//   （回忆轨迹先看总结，被压掉的细节按需 cat/grep 存档抓回）
// - 失败兜底：空闲超时重试 3 次仍失败 → 硬截断到 hard_trunc_chars；
//   其他错误（含 /stop 中断）→ 不重试、session 不变（下轮消息再试）
// - 硬截断只有一个触发点：上面那条 3 次空闲超时（2026-10-18 删 4xx 回退阶梯后，
//   原先经阶梯调用的 forceHardTruncate 入口已不存在）
//
// 热更新（2026-09-03）：SetCfg/SetLLM/SetSystemPrompt 支持 POST /config 即时生效。

import (
	"context"
	"fmt"
	"okhuman/internal/textutil"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"okhuman/internal/llm"
	"okhuman/internal/persist"
	"okhuman/internal/types"
)

// CompressEvent 压缩事件（compress 事件广播与 /status 日志共用形状；
// JSON 字段名与 TS 版一致）
type CompressEvent struct {
	Scope       string
	Detail      string
	BeforeChars int
	AfterChars  int
	Truncated   bool // 正文超 context.summary_max_chars 被硬截断
	Window      int  // 当时生效的 context.keep_summaries
	Text        string
	Thinking    *string
}

// Cfg 压缩参数（POST /config 热更新，对应 config.context）
type Cfg struct {
	MaxTokens        int
	KeepRecentChars  int
	HardTruncChars   int
	StreamIdleMS     int
	CharsPerToken    float64
	AttachmentTokens int // 一个附件按多少 token 计（0=不计）
	// KeepSummaries：上下文里保留几条总结（滑动窗口，超出从头部丢弃，不级联合并）。
	KeepSummaries int
	// SummaryMaxChars：单条总结正文的硬上限（utf16 计数；压缩轮的思考不计入正文）。
	// 超出直接截断，并在注入时提醒模型这条是硬截断的。
	SummaryMaxChars int
}

// NormalizeCfg 兜底归一：窗口条数或摘要上限取到非法值时退回安全默认。
// 为什么要在这里兜，而不只靠 /config 的校验：config/user.json 直接改文件会绕过
// numChecks，而 keep_summaries=0 会让"窗口"变成不限制、summary_max_chars=0
// 会让 textutil.UTF16Truncate 变成不截断 —— 两个都是静默失效。
func NormalizeCfg(c Cfg) Cfg {
	if c.KeepSummaries != 1 {
		c.KeepSummaries = 1 // 单条总结（旧语义）：窗口固定 1，多条并列已废弃
	}
	if c.SummaryMaxChars < 1000 {
		c.SummaryMaxChars = 30000
	}
	return c
}

// Manager 会话状态 + 压缩编排
type Manager struct {
	mu       sync.Mutex
	changeMu sync.Mutex // 审计 H2 修（2026-09-14）：串行化"变更全程"
	//（AddMessage 的 append+压缩+落盘、CompressNow）。/inject 与在飞 run
	// 并发 AddMessage 时，旧代码两个压缩并行、各自按压缩前下标切 batch → 陈旧
	// 下标切错位置可能丢错消息；持锁后压缩串行、下标始终有效。
	llm llm.LlmClient
	cfg Cfg
	est Est // cfg 的估算口径快照（SetCfg 时重算）
	// 最近一次压缩落盘的两个文件（session-records 全文存档 / session-summaries 总结）。
	// 手动压缩（CompressNow）把这两个路径报给调用方，UI 才能直接给出文件位置。
	lastRecordPath, lastSummaryPath string
	// 写存档/总结失败的原因（路径为空时用它说明为什么空；手动压缩会把这句报给调用方）
	lastRecordErr, lastSummaryErr error
	systemPrompt                  string
	session                       *types.SessionState
	seq                           int
	sessionNo                     int
	recordDir                     string
	runCtx                        *context.Context // 当前 run 的中止信号（压缩流用；/stop 时连压缩一起中止）
	compressL                     []listener[func(CompressEvent)]
	deltaL                        []listener[func(string, types.Delta)]
	persistL                      []listener[func(*types.SessionState, int)]
	listenSeq                     int      // 监听器编号（按 id 注销，不按函数指针）
	selfInfo                      SelfInfo // 自我生命感知输入（动态注入 2026-09-20）
}

// NewManager 新建（空会话）
func NewManager(client llm.LlmClient, cfg Cfg, systemPrompt string, recordDir string) *Manager {
	cfg = NormalizeCfg(cfg)
	return &Manager{
		llm:          client,
		cfg:          cfg,
		est:          NewEst(cfg.CharsPerToken, cfg.AttachmentTokens),
		systemPrompt: systemPrompt,
		session:      &types.SessionState{Messages: []types.RawEntry{}},
		recordDir:    recordDir,
	}
}

// SetCfg 热更新压缩参数
func (m *Manager) SetCfg(cfg Cfg) {
	cfg = NormalizeCfg(cfg)
	m.mu.Lock()
	m.cfg = cfg
	m.est = NewEst(cfg.CharsPerToken, cfg.AttachmentTokens)
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

// snapshotLocked 当前会话的深快照（持锁调用）：Messages 为新切片（元素拷贝），
// Summary 共享指针（TEntry 一经创建不再原地修改）。调用方可长期持有快照并发
// 读取——与 AddMessage 的 append / 压缩的切片重切互不干扰（无共享可变结构）。
func (m *Manager) snapshotLocked() *types.SessionState {
	snap := &types.SessionState{ID: m.session.ID}
	// Summaries 元素值拷贝（条目一经创建不再原地改，切片本身会被窗口重切）
	snap.Summaries = append([]types.SummaryEntry(nil), m.session.Summaries...)
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
	return subscribe(m, &m.compressL, fn)
}

// AddCompressDeltaListener 订阅压缩思考流（compress_delta 事件）；返回注销函数
func (m *Manager) AddCompressDeltaListener(fn func(string, types.Delta)) func() {
	return subscribe(m, &m.deltaL, fn)
}

// AddPersistListener 订阅会话变更（落盘监听）；返回注销函数
func (m *Manager) AddPersistListener(fn func(*types.SessionState, int)) func() {
	return subscribe(m, &m.persistL, fn)
}

// listener 一条订阅（带编号）。
type listener[T any] struct {
	id int
	fn T
}

// subscribe 追加一条订阅，返回只注销这一条的函数。
// 为什么按 id、不按旧实现的 reflect 函数代码指针比对：那是"函数能比较"的假象。
// 实测（本机 go1.24，2026-11-12）：同一个字面量产生的闭包会被编译器克隆、代码指针**不同**
// （所以本仓库现有注册点没被咬到），但**同一个 func 值注册两次**、以及**同一个方法的两次
// 方法值**（x.H）指针**相同** → 注销其一会把另一条一起删掉。按 id 注销没有这类歧义，
// 也不依赖编译器的克隆行为。
func subscribe[T any](m *Manager, l *[]listener[T], fn T) func() {
	m.mu.Lock()
	m.listenSeq++
	id := m.listenSeq
	*l = append(*l, listener[T]{id: id, fn: fn})
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		*l = unsubscribeByID(*l, id)
		m.mu.Unlock()
	}
}

func unsubscribeByID[T any](l []listener[T], id int) []listener[T] {
	out := make([]listener[T], 0, len(l))
	for _, e := range l {
		if e.id != id {
			out = append(out, e)
		}
	}
	return out
}

// Compose 组装发给 LLM 的消息（主循环与压缩请求共用 → 前缀逐 token 一致）
func (m *Manager) Compose() []types.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.composeLocked()
}

// composeLocked = 基础组装 + 动态注入（2026-09-20，持锁调用）：系统提示词首
// prepend 自我生命感知块（本实例端口/pid + LLM 端口/pid，每次调用现算 →
// 端口/pid 变化实时更新）。消息本体不再注入任何东西（时间戳功能同日砍掉），
// 会话前缀跨 run 逐 token 稳定 → KV 前缀可复用。
func (m *Manager) composeLocked() []types.Message {
	msgs := ComposeMessages(m.session, m.systemPrompt)
	if sb := m.selfBlockLocked(); sb != "" {
		if sp, ok := msgs[0].Content.(string); ok {
			msgs[0].Content = sb + "\n\n" + sp
		}
	}
	return msgs
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
		// 只剩最新一条消息：最新一条永不压。没有原文可压就什么都不做 ——
		// 真超了模型窗口，请求按既有规则原样报错，处置交给人。（2026-11-05：
		// 旧的"再压一遍 summary"杠杆随迭代压缩一起删掉，不用"丢摘要"去替代它。）
		m.mu.Unlock()
		return
	}
	// 字符预算：从最新往前累积 ≤ keep_recent_chars 的最大条数（至少 1）
	keep, _ := m.keepWindowLocked(m.cfg.KeepRecentChars)
	batch := m.session.Messages[:n-keep]
	m.mu.Unlock()

	if len(batch) == 0 {
		// 全部消息都在保留预算内 → 没有原文可压，什么都不做：真超了模型窗口就让请求按既有
		// 规则原样报错，处置交给人。除 setSummaryLocked 的替换之外没有第二条丢弃路径。
		return
	}
	m.doCompress(batch)
}

// DumpSessionRecord 把当前会话全文落盘到 session-records/（与压缩存档同一命名口径
// ctx-<会话号>-<时间戳>-seq<N>.txt），返回 (路径, 消息条数)；空会话不落盘返回 ("", 0)。
// 用途：重置会话前保住旧会话原文——重置整体换掉 Manager，除压缩存档外旧会话无处可寻。
// 只读会话不改结构，故与任何路径都安全（持 mu 快照写文件）。
// 返回第三个值 = 写失败原因：那是旧会话原文除压缩存档外**唯一**的全文存档，
// 静默失败 = 事后无处可寻（2026-11-13 交叉审查指出，旧版至少响应里还看得见占位串）。
func (m *Manager) DumpSessionRecord(reason string) (string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.session.Messages)
	if n == 0 && len(m.session.Summaries) == 0 {
		return "", 0, nil
	}
	p, err := m.writeSessionRecordLocked(m.recordTsLocked(), reason)
	if err != nil {
		return "", n, err
	}
	return p, n, nil
}

// estimateTotalLocked 估算总 token（持锁调用）
func (m *Manager) estimateTotalLocked() int {
	t := EstimateTokens(m.systemPrompt, m.cfg.CharsPerToken)
	for _, sm := range m.session.Summaries {
		t += EstimateTokens(sm.Content, m.cfg.CharsPerToken)
	}
	for _, e := range m.session.Messages {
		t += EstimateMessageTokens(e, m.est)
	}
	return t
}

// doCompress 压缩 = 当前主循环完整前缀（含全部待压缩内容）+ 末尾一条压缩轮指令。
// 前缀不变时重发同一请求，KV 前缀仍能命中。
// 2026-11-05 语义：一次压缩只覆盖 batch（已有摘要与保留的最近原文不进新摘要，
// 指令里明确划定范围），产出正文按 cfg.SummaryMaxChars 硬截断，然后 append 进
// 摘要窗口；窗口超过 cfg.KeepSummaries 就从头部丢弃（不级联合并）。
// 失败兜底：空闲超时重试共 3 次 → 仍失败 hardTruncate；空输出/其他错误 →
// session 不变（下一轮消息再试，原文已落盘不丢）。
// compressOutcome 一次压缩的真实结果。CompressNow 据此决定返回给用户的 ok 与文案：
// 旧实现无条件回 true，模型报错/空输出时用户看到"已提交压缩…新增 1 条总结"，
// 而会话其实一点没变（2026-11-13 交叉审查）。
type compressOutcome struct {
	Changed    bool   // 会话是否真的被改动（含硬截断这种降级）
	Summarized bool   // 是否由模型生成了总结（硬截断只放占位 stub，不算）
	Note       string // 失败或降级的原因（可为空）
}

// batchSeqRange 最旧 nBatch 条消息的真实 seq 区间。totalSeq 是累计计数（只增不减）、
// nMsg 是压缩前的会话条数，因此当前会话覆盖 totalSeq-nMsg+1..totalSeq，最旧那一批的
// 起点与它相同、终点再往前推 nBatch-1（2026-11-13 第四轮审计 N6）。
func batchSeqRange(totalSeq, nMsg, nBatch int) (int, int) {
	from := totalSeq - nMsg + 1
	return from, from + nBatch - 1
}

func (m *Manager) doCompress(batch []types.RawEntry) compressOutcome {
	m.mu.Lock()
	ts := m.recordTsLocked()
	recordPath, recErr := m.writeSessionRecordLocked(ts, "压缩")
	m.lastRecordPath, m.lastRecordErr = recordPath, recErr
	beforeChars := sessionChars(m.session, m.est)
	prefix := m.composeLocked()
	streamIdleMS := m.cfg.StreamIdleMS
	// 出锁后要用的配置只能在锁内取走：SetCfg 写 m.cfg，出锁再读就是 data race
	// （2026-11-13 第四轮 A1，-race 实抓）。摘要条数不在其中——它不再进任何文案。
	maxChars := m.cfg.SummaryMaxChars
	nMsg := len(m.session.Messages)
	// 批次的真实 seq 区间：m.seq 是累计条数（只增不减），当前会话 N 条对应真实 seq
	// 是 seq-N+1..seq，最旧 B 条是 seq-N+1..seq-N+B。旧公式写成 seq-B+1..seq，
	// 系统性偏出 keep 窗条数 → 每条总结的注入头"覆盖 seq a-b"、压缩指令、/history、
	// /status 全是错值（2026-11-13 第四轮 N6，错自 cfc5315）。
	seqFrom, seqTo := batchSeqRange(m.seq, nMsg, len(batch))
	// 覆盖区间只由代码记账（模型对自己上下文里的绝对 seq 没有可靠概念，2026-11-13 第七轮
	// P6 就是它自己写错）：prevFrom 把新总结的起点扩到被它取代那条的起点。压缩指令不带这些
	// 数字——单条语义下整个上下文都是它的范围，没什么要声明的（2026-11-16 删）。
	prevFrom := seqFrom
	if len(m.session.Summaries) > 0 && m.session.Summaries[0].SeqFrom < prevFrom {
		prevFrom = m.session.Summaries[0].SeqFrom
	}
	m.mu.Unlock()

	instruction := compressPromptBody

	const scope = "history"
	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		res, err := CompressOnce(m.runContext(), m.llmLocked(), prefix, scope, instruction, CompressOpts{
			OnDelta: func(d types.Delta) { m.emitCompressDelta(scope, d) },
			IdleMS:  streamIdleMS,
		})
		if err != nil {
			if _, ok := err.(*llm.IdleTimeoutError); !ok {
				m.emit(scope, "压缩失败："+err.Error(), "", false, beforeChars, nil)
				return compressOutcome{Note: "压缩请求失败：" + err.Error()}
			}
			if attempt >= maxAttempts {
				m.mu.Lock()
				m.hardTruncateLocked(recordPath)
				m.mu.Unlock()
				return compressOutcome{Changed: true, Note: fmt.Sprintf(
					"连续 %d 次空闲超时：已放弃 LLM 压缩，改为硬截断（没有生成总结）", maxAttempts)}
			}
			m.emit(scope, fmt.Sprintf("压缩空闲超时（%dms 无响应），重试 %d/%d", streamIdleMS, attempt, maxAttempts), "", false, beforeChars, nil)
			continue
		}
		if res.Text == "" {
			m.emit(scope, "压缩失败：模型空输出", "", false, beforeChars, nil)
			return compressOutcome{Note: "模型空输出，没有生成总结"}
		}
		m.mu.Lock()
		if len(batch) == 0 || len(batch) > len(m.session.Messages) {
			// 内部不一致：没有原文可删（或批次大于会话）。此时若继续放 summary，
			// 上下文只会**变长**却报"压缩成功"，所以直接退出（2026-11-13 四方审计）。
			m.mu.Unlock()
			return compressOutcome{Note: "内部不一致：待压批次与实际会话不匹配，会话未改动"}
		}
		m.session.Messages = m.session.Messages[len(batch):]
		origChars := textutil.UTF16Len(res.Text)
		// 提醒行本身也计入上限：先算出提醒，再按 剩余空间 截正文 ——
		// 保证"注入的这条正文"始终 ≤ summary_max_chars，不靠事后超一点。
		note := ""
		truncated := origChars > maxChars
		if truncated {
			note = fmt.Sprintf("\n（正文已按 %d 字符上限硬截断，截断前 %d 字符）", maxChars, origChars)
		}
		room := maxChars
		if n2 := maxChars - textutil.UTF16Len(note); truncated && n2 > 0 {
			room = n2
		}
		body, _ := textutil.UTF16Truncate(res.Text, room)
		if truncated {
			body = strings.TrimSpace(body) + note
		}
		// 总结正文落盘 session-summaries/（与 records 存档同名基座配对）；写失败不阻断压缩
		var summaryPath string
		// 每轮先复位再尝试：只在出错时赋值的话，任何一次历史写失败会一直挂在
		// CompressNow 的 detail 上（对照 lastRecordErr 每轮必赋值）。2026-11-13 交叉审查指出。
		m.lastSummaryErr = nil
		if recErr == nil {
			var sumErr error
			summaryPath, sumErr = m.writeSummaryFileLocked(ts, body, origChars, truncated, beforeChars, recordPath)
			if sumErr != nil {
				summaryPath = ""
				m.lastSummaryErr = sumErr
			}
		}
		m.lastSummaryPath = summaryPath
		entry := types.SummaryEntry{Content: body, CreatedAt: time.Now().UnixMilli(),
			SeqFrom: seqFrom, SeqTo: seqTo, OrigChars: origChars, Truncated: truncated,
			SummaryFile: summaryPath, RecordFile: recordPath}
		dropped := m.setSummaryLocked(entry, prevFrom)
		m.mu.Unlock()

		truncNote := ""
		if truncated {
			truncNote = fmt.Sprintf("（超 %d 上限已硬截断，原 %d 字符）", maxChars, origChars)
		}
		detail := fmt.Sprintf("%d 条最早消息（seq %d-%d）已重写进 1 条总结，正文 %d 字符%s",
			len(batch), seqFrom, seqTo, textutil.UTF16Len(body), truncNote)
		if summaryPath != "" {
			detail += "，总结文本已落盘"
		}
		for _, d := range dropped {
			detail += fmt.Sprintf("\n取代了上一条总结：seq %d-%d，%d 字符，文本 %s，原文 %s",
				d.SeqFrom, d.SeqTo, d.OrigChars, d.SummaryFile, d.RecordFile)
		}
		m.emit(scope, detail, body, truncated, beforeChars, res.Thinking)
		return compressOutcome{Changed: true, Summarized: true}
	}
}

// emitCompressDelta 广播压缩流增量（WebUI "压缩中" 实时显示）
func (m *Manager) emitCompressDelta(scope string, d types.Delta) {
	m.mu.Lock()
	l := m.deltaL
	m.mu.Unlock()
	for _, e := range l {
		e.fn(scope, d)
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

// CompressNow 手动压缩（WebUI「手动压缩上下文」按钮）：不等 token 阈值，按
// keep_recent_chars 预算把最旧一批交给 LLM 压成 summary——与 maybeCompress 同
// 一套分批口径与同一条压缩路径，只是触发由人做。返回 (说明, 是否真的压了)。
// keepWindowLocked 从最新往前累积、字符数不超过 budget 的最大条数（至少 1 条），
// 同时返回保留下来的字符数（硬截断要把这个数写进日志）。调用方需持 m.mu。
// 同一算法原本在三处各写一遍（自动压缩 / 手动 /compress / 硬截断）——2026-11-12 先并了
// 前两处，2026-11-16 把硬截断那份也并进来，边界以后只有一处可改。
func (m *Manager) keepWindowLocked(budget int) (keep int, keptChars int) {
	n := len(m.session.Messages)
	if n == 0 {
		return 0, 0
	}
	keep, keptChars = 1, messageChars(m.session.Messages[n-1], m.est)
	for i := n - 2; i >= 0; i-- {
		c := messageChars(m.session.Messages[i], m.est)
		if keptChars+c > budget {
			break
		}
		keptChars += c
		keep++
	}
	return keep, keptChars
}

// setSummaryLocked 落一条总结，返回被它取代的旧总结（持锁调用）。这是摘要窗口的唯一
// 写法：窗口固定 1（NormalizeCfg），所以落一条就是替换一条；模型生成的总结与硬截断的
// 占位条目走同一条路，不再各写一份裁剪。fromSeq>0 时把覆盖起点扩到被取代那条的起点
// （区间只由代码记账）。
func (m *Manager) setSummaryLocked(entry types.SummaryEntry, fromSeq int) []types.SummaryEntry {
	if fromSeq > 0 {
		entry.SeqFrom = fromSeq
	}
	old := m.session.Summaries
	m.session.Summaries = []types.SummaryEntry{entry}
	return old
}

// 持 changeMu 与在飞 run 的 AddMessage 串行；server 侧另有一道"run 进行中拒绝"
// 的闸（避免在 run 中途换掉会话结构）。
func (m *Manager) CompressNow() (string, bool) {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	m.mu.Lock()
	n := len(m.session.Messages)
	if n == 0 {
		m.mu.Unlock()
		return "会话为空，没有可压缩的内容", false
	}
	keep, _ := m.keepWindowLocked(m.cfg.KeepRecentChars)
	batch := m.session.Messages[:n-keep]
	before := sessionChars(m.session, m.est)
	m.mu.Unlock()
	if len(batch) == 0 {
		return fmt.Sprintf("无可压内容（全部 %d 条都在 context.keep_recent_chars 的保留预算内）；要腾出空间只能 /reset，或调小 keep_recent_chars", n), false
	}
	out := m.doCompress(batch)
	if !out.Changed {
		return "压缩未完成：" + out.Note + "（会话未改变，没有新增总结）", false
	}
	m.mu.Lock()
	rec, sum := m.lastRecordPath, m.lastSummaryPath
	recErr, sumErr := m.lastRecordErr, m.lastSummaryErr
	m.mu.Unlock()
	if !out.Summarized {
		// 降级路径（硬截断）：改了会话，但没有总结，不能按"已压缩"说话
		s := out.Note
		if rec != "" {
			s += "\n全文存档：" + rec
		} else if recErr != nil {
			s += "\n全文存档写入失败：" + recErr.Error()
		}
		return s, true
	}
	detail := fmt.Sprintf("已提交压缩：%d 条最旧消息重写为 1 条总结（压缩前 %d 字符）", len(batch), before)
	if rec != "" {
		detail += "\n全文存档：" + rec
	} else if recErr != nil {
		detail += "\n全文存档写入失败：" + recErr.Error()
	}
	if sum != "" {
		detail += "\n总结文本：" + sum
	} else if sumErr != nil {
		detail += "\n总结文本写入失败：" + sumErr.Error()
	}
	return detail, true
}

// hardTruncateLocked 硬截断（持锁）：压缩 3 次重试均空闲超时 →
// 放弃 LLM 压缩，直接保留最近 hard_trunc_chars 字符预算内的原文，其余丢弃
// （完整内容已在 session-records/ 落盘，数据不丢）。summary 置为记录文件路径引用。
func (m *Manager) hardTruncateLocked(recordPath string) {
	before := sessionChars(m.session, m.est)
	n := len(m.session.Messages)
	keep, keptChars := m.keepWindowLocked(m.cfg.HardTruncChars)
	if n > 0 {
		m.session.Messages = m.session.Messages[n-keep:]
	}
	ref := "[完整会话原文已存档：" + recordPath + " —— 上下文压缩失败已硬截断，需要被截掉的细节时用 bash cat/grep 抓回]"
	if recordPath == "" {
		ref = "[完整会话原文已存档 —— 上下文压缩失败已硬截断，需要被截掉的细节时用 bash 抓回]"
	}
	// 兜底不产生正文（没有 LLM 总结），只留一条带存档指针的条目；它走的也是 setSummaryLocked。
	m.setSummaryLocked(types.SummaryEntry{
		Content: ref, CreatedAt: time.Now().UnixMilli(), SeqTo: m.seq,
		Truncated: false, RecordFile: recordPath}, 0)
	m.emitLocked("history", fmt.Sprintf("压缩 3 次均无响应 → 硬截断保留最近 %d 字符（丢弃 %d 条，完整记录 %s）", keptChars, n-keep, recordPath), ref, false, before, nil)
}

// recordTsLocked 当前 UTC 毫秒精度时间戳（总结/存档文件名同名基座配对用，持锁调用）
func (m *Manager) recordTsLocked() string {
	return time.Now().UTC().Format("2006-01-02_15-04-05-000") + "Z"
}

// writeSummaryFileLocked 压缩后总结文本落盘（2026-09-25）：
// recordDir/session-summaries/<同名基座>.summary.txt（持锁调用）。与 session-records/
// 存档不同文件夹、同名基座成对：总结供快速回忆任务轨迹（时间线/关键实体/完成状态），
// 存档供按需抓回被压掉的细节。文件头带元信息（会话号/生成时间/轮数/前后字数/配对存档）。
func (m *Manager) writeSummaryFileLocked(ts, text string, origChars int, truncated bool, beforeChars int, recordPath string) (string, error) {
	iso := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	truncNote := "否"
	if truncated {
		truncNote = fmt.Sprintf("是（原 %d 字符 → 保留 %d 字符，上限 %d）", origChars, textutil.UTF16Len(text), m.cfg.SummaryMaxChars)
	}
	lines := []string{
		fmt.Sprintf("# 压缩后总结 会话号=%d 生成=%s 硬截断=%s", m.sessionNo, iso, truncNote),
		fmt.Sprintf("压缩前会话字符数=%d 本条正文字符数=%d（单实例：agent 由落盘目录 okhuman-<port>/ 标识）", beforeChars, textutil.UTF16Len(text)),
		"配对完整存档：" + recordPath,
		"",
		text,
	}
	dir := filepath.Join(m.recordDir, "session-summaries")
	path := filepath.Join(dir, fmt.Sprintf("ctx-%d-%s-seq%d.summary.txt", m.sessionNo, ts, m.seq))
	if err := persist.WritePath(path, strings.Join(lines, "\n")); err != nil {
		return "", err
	}
	return path, nil
}

// writeSessionRecordLocked 生成会话完整记录（recordDir/session-records/
// ctx-<会话号>-<时间戳>-seq<N>.txt，持锁调用）。内容 = 落盘时刻的完整会话
// （system 提示词 + summary + 全部消息原文）。
// reason 只进文件头标明触发场景："压缩"（自动/手动压缩前，防细节丢失）与
// "重置会话"（2026-10-18：重置会整体换掉 Manager，旧会话原文除压缩存档外无处可寻
// → 重置前无条件先存一份全文，主人要的就是这个）。
// 人类可读的纯文本；结构固定：压缩后总结置顶 → system 提示词 → 消息按时间序。
// 文件名毫秒精度 + seq 后缀（同毫秒不撞名）。ts 由调用方生成（doCompress 把同一 ts
// 传给本函数与 writeSummaryFileLocked → 存档/总结同名基座配对）。
func (m *Manager) writeSessionRecordLocked(ts, reason string) (string, error) {
	iso := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	var lines []string
	lines = append(lines, fmt.Sprintf("# 会话完整记录 会话号=%d 生成=%s 触发=%s（单实例：agent 由落盘目录 okhuman-<port>/ 标识）", m.sessionNo, iso, reason))
	for i, sm := range m.session.Summaries {
		lines = append(lines, "", fmt.Sprintf("## 历史总结 %d/%d（seq %d-%d，生成于 %s，硬截断=%v）",
			i+1, len(m.session.Summaries), sm.SeqFrom, sm.SeqTo,
			time.UnixMilli(sm.CreatedAt).UTC().Format("2006-01-02 15:04 UTC"), sm.Truncated), "", sm.Content, "")
	}
	lines = append(lines, "## 系统提示词", "", m.systemPrompt, "")
	lines = append(lines, "## 消息（按时间序，落盘时刻的完整会话）")
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
	if err := persist.WritePath(path, strings.Join(lines, "\n")); err != nil {
		return "", err
	}
	return path, nil
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
func (m *Manager) emit(scope, detail, text string, truncated bool, beforeChars int, thinking *string) {
	m.mu.Lock()
	m.emitLocked(scope, detail, text, truncated, beforeChars, thinking)
	m.mu.Unlock()
}

// emitLocked 广播压缩事件（持锁路径调用——sync.Mutex 非重入，持锁时禁用 emit）
func (m *Manager) emitLocked(scope, detail, text string, truncated bool, beforeChars int, thinking *string) {
	e := CompressEvent{
		Scope:       scope,
		Detail:      detail,
		BeforeChars: beforeChars,
		AfterChars:  textutil.UTF16Len(text),
		Truncated:   truncated,
		Window:      m.cfg.KeepSummaries,
		Text:        text,
		Thinking:    thinking,
	}
	l := m.compressL
	for _, sub := range l {
		sub.fn(e)
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
	for _, e := range l {
		e.fn(state, seq)
	}
}
