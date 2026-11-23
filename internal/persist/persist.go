package persist

// 落盘持久化：日志 / 会话统一落 data.dir（okhuman-<port>）。
// 布局：okhuman.log（追加，>5MB 轮转 .old）/ session.json（每次变更实时原子写）
//   session-records/（压缩触发完整上下文记录）/ tool-results/（截断工具结果全文）
//
// 实时落盘：无防抖、无内存缓存——任何变更同步原子写（tmp + rename），
// 调用返回时文件即最新；进程随时可杀（含 kill -9），零丢失。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"okhuman/internal/types"
)

const (
	LogMaxBytes = 5 * 1024 * 1024
	SessionFile = "session.json"
	LogFile     = "okhuman.log"
)

// Snapshot 会话快照（落盘格式 { no, seq, state }）
type Snapshot struct {
	No    int                 `json:"no"`
	Seq   int                 `json:"seq"`
	State *types.SessionState `json:"state"`
}

// WritePath 原子写 + 顺手保证父目录存在。此前 9 处调用点各自 open-code（现经此处写出的 8 处；
// SaveState 例外——它写的是 Init 时已建好的目录，直接用 AtomicWriteFile）
// "MkdirAll → AtomicWriteFile → 自己决定错误怎么办"，其中三处把 error 丢了
// （2026-11-12 四方审查后修）。归口到这里：调用方只决定怎么报，不建目录、也不吞错误。
func WritePath(path, data string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("建目录失败：%w", err)
	}
	return AtomicWriteFile(path, data)
}

// WriteJSONPath 先 MarshalIndent（缩进 2，与配置文件一致）再原子写。
// marshal 失败也如实返回——调用方原来普遍写 `out, _ := json.MarshalIndent(...)`。
func WriteJSONPath(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化失败：%w", err)
	}
	return WritePath(path, string(b))
}

// AtomicWriteFile 原子写（唯一 tmp + rename）：读端永远看到完整文件。
// tmp 名每次唯一（2026-09-14 审计修：旧固定 .tmp 名在并发 SaveSession 时
// ——/inject 与在飞 run 各自触发落盘——互相覆盖，rename 失败会把 session.json
// 留在陈旧内容上，进程恰在此窗口被杀则丢最近消息）
func AtomicWriteFile(file string, data string) error {
	dir := filepath.Dir(file)
	// 权限沿用目标文件原有 mode（不存在则 0644）：CreateTemp 建的是 0600，
	// 不显式 Chmod 的话 rename 会把原文件权限悄悄收紧（session.json 曾因此变 0600）。
	mode := os.FileMode(0o644)
	if st, err := os.Stat(file); err == nil {
		mode = st.Mode().Perm()
	}
	f, err := os.CreateTemp(dir, filepath.Base(file)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	// 数据先落盘再 rename：只 rename 不 fsync，进程崩溃安全但掉电时可能
	// "rename 成功、内容不全"（元数据与数据页顺序不保证）。
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	// rename 本身也尽力落盘（失败不影响本次写入的正确性，只影响掉电后的可见性）
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Persistence 单实例落盘器
type Persistence struct {
	dir   string
	logMu sync.Mutex // 只保护文件写
	// enqMu 保证入队顺序 = 调用顺序；logCh 由唯一一个 writer 消费，
	// 于是落盘顺序 = 入队顺序 = 调用顺序。
	enqMu   sync.Mutex
	logCh   chan logEntry
	dropped atomic.Int64
}

// logQueueSize 日志队列深度。有界是故意的：日志绝不能把主流程拖住，
// 也绝不能无界吃内存（磁盘慢/满时队列会堆）。
const logQueueSize = 4096

// logFlushWait FlushLog 等冲刷请求被处理到的上限；超时就不再等（退出路径不能卡死）。
const logFlushWait = 2 * time.Second

// logEntry 队列里的一条：要么是一行日志，要么是一次冲刷请求（flush 非 nil）。
// 冲刷请求本身不写文件；单写手按序消费，所以它被处理到 = 它前面的都已落盘。
type logEntry struct {
	line  string
	flush chan struct{}
}

func Init(dir string) (*Persistence, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := &Persistence{dir: dir, logCh: make(chan logEntry, logQueueSize)}
	go p.logWriter()
	return p, nil
}

// Log 控制台 + 文件追加（>5MB 轮转 .old）；日志失败不影响主流程。
//
// 2026-10-19 修：以前每条日志 go appendLogLine —— 取锁顺序随机，实测 400 条
// 并发日志里 182 行乱序（45%）。okhuman.log 是审计时间线（谁什么时候 reset、
// 后端什么时候消失），全靠行的先后推断；行内时间戳只到毫秒，同毫秒内无法定序，
// 因果可能被写反。改成单写手 + 有界队列：顺序 = 调用顺序，写文件不挡主流程，
// 队列满则丢日志并留一行痕迹（丢日志好过丢请求）。
func (p *Persistence) Log(msg string) {
	line := fmt.Sprintf("[%s] %s", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), msg)
	fmt.Println(line)
	p.enqMu.Lock()
	select {
	case p.logCh <- logEntry{line: line}:
	default:
		p.dropped.Add(1)
	}
	p.enqMu.Unlock()
}

// logWriter 唯一的日志写手：串行消费队列，保证落盘顺序。
func (p *Persistence) logWriter() {
	for e := range p.logCh {
		if d := p.dropped.Swap(0); d > 0 {
			p.appendLogLine(fmt.Sprintf("[log] 日志队列已满，丢弃 %d 行（保主流程，不阻塞）", d))
		}
		if e.flush != nil {
			close(e.flush)
			continue
		}
		p.appendLogLine(e.line)
	}
}

// FlushLog 等已排队的日志真正落盘（进程退出/被重启前调用，避免丢最后几行）。
// 只等不关：关了之后的 Log 会 panic。
//
// 做法是往队列里投一个冲刷请求：写手按序消费，它被处理到时说明前面的都已写完。
// 旧的写法是轮询 len(logCh) —— 那只看队列长度，写手已经把最后一条取出去、还没写完时
// 长度就是 0 了，于是 FlushLog 会提前返回、最后一行没落盘（2026-11-13 复审指出）。
// 队列满（此刻 Log 本身也在丢行）时投不进去，退化为原来的有界轮询。
func (p *Persistence) FlushLog() {
	ack := make(chan struct{})
	select {
	case p.logCh <- logEntry{flush: ack}:
		select {
		case <-ack:
		case <-time.After(logFlushWait):
		}
		return
	default:
	}
	for i := 0; len(p.logCh) > 0 && i < 500; i++ {
		time.Sleep(2 * time.Millisecond)
	}
}

func (p *Persistence) appendLogLine(line string) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	path := filepath.Join(p.dir, LogFile)
	fi, err := os.Stat(path)
	if err == nil && fi.Size() > LogMaxBytes {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

// SaveSession 会话实时落盘（同步原子写：返回时 session.json 已是最新）
func (p *Persistence) SaveSession(snap *Snapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return AtomicWriteFile(filepath.Join(p.dir, SessionFile), string(data))
}

// LoadSessionFile 加载会话。返回值三种情况必须分得开（2026-11-13 四方审计 R4/#15）：
//
//	无文件 → (nil, nil)：正常的首次启动；
//	文件存在但形状不被接受 → (nil, error)：调用方要留痕，旧文件仍留在盘上；
//	正常 → (snapshot, nil)。
//
// 以前损坏也回 (nil, nil) → 半截文件被当"无会话"，整个会话静默归零、日志一行没有。
// 唯一形状：{ no, seq, state: { id, summaries:[…], messages:[…] } }。
// 校验口径：no/seq 必须数字；summaries 若存在必须是数组、每项 content 必须是字符串
// （任一项形状异常 → 整份拒载，与 validEntry 同口径）；每条消息 role 必须 string，
// content 必须 string 或数组（/inject 附件消息的 content 是 parts 数组）。
// 2026-11-05：删掉 TS 版三层形状（raw/t1s/t2）的迁移与单条 summary 的旧字段——
// 盘上已不存在那种文件，不留不会再被走的代码。旧文件里的 "summary" 字段按未知字段忽略，
// 结果是"零条摘要"，不报错（下次压缩按新规则从头建）。
func LoadSessionFile(dir string) (*Snapshot, error) {
	data, err := os.ReadFile(filepath.Join(dir, SessionFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读会话文件失败：%w", err)
	}
	var d struct {
		No  *int             `json:"no"`
		Seq *int             `json:"seq"`
		St  *json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("顶层 JSON 不合法：%w", err)
	}
	if d.No == nil || d.Seq == nil || d.St == nil {
		return nil, fmt.Errorf("顶层缺少 no/seq/state 字段")
	}
	var st map[string]interface{}
	if err := json.Unmarshal(*d.St, &st); err != nil || st == nil {
		return nil, fmt.Errorf("state 不是对象")
	}
	msgs, ok := st["messages"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("state.messages 不是数组")
	}
	sums, err := summariesFromJSON(st)
	if err != nil {
		return nil, fmt.Errorf("summaries：%w", err)
	}
	for i, e := range msgs {
		if !validEntry(e) {
			return nil, fmt.Errorf("第 %d 条消息形状不被接受（role 须为字符串，content 须为字符串或数组）", i)
		}
	}
	snap := &Snapshot{No: *d.No, Seq: *d.Seq, State: &types.SessionState{ID: 1, Summaries: sums}}
	if v, ok := st["id"].(float64); ok {
		snap.State.ID = int(v)
	}
	for _, e := range msgs {
		snap.State.Messages = append(snap.State.Messages, entryFromJSON(e))
	}
	return snap, nil
}

// summariesFromJSON 解析摘要窗口（旧→新顺序原样保留）。
func summariesFromJSON(st map[string]interface{}) ([]types.SummaryEntry, error) {
	v, ok := st["summaries"]
	if !ok || v == nil {
		return nil, nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("summaries 非数组")
	}
	out := make([]types.SummaryEntry, 0, len(arr))
	for _, it := range arr {
		o, ok := it.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("summaries 项非对象")
		}
		c, ok := o["content"].(string)
		if !ok {
			return nil, fmt.Errorf("summaries.content 非字符串")
		}
		sm := types.SummaryEntry{Content: c}
		if f, ok := o["created_at"].(float64); ok {
			sm.CreatedAt = int64(f)
		}
		if f, ok := o["seq_from"].(float64); ok {
			sm.SeqFrom = int(f)
		}
		if f, ok := o["seq_to"].(float64); ok {
			sm.SeqTo = int(f)
		}
		if f, ok := o["orig_chars"].(float64); ok {
			sm.OrigChars = int(f)
		}
		if b, ok := o["truncated"].(bool); ok {
			sm.Truncated = b
		}
		if str, ok := o["summary_file"].(string); ok {
			sm.SummaryFile = str
		}
		if str, ok := o["record_file"].(string); ok {
			sm.RecordFile = str
		}
		out = append(out, sm)
	}
	return out, nil
}

func validEntry(e interface{}) bool {
	eo, ok := e.(map[string]interface{})
	if !ok {
		return false
	}
	role, ok := eo["role"].(string)
	if !ok || role == "" {
		return false
	}
	c, ok := eo["content"]
	if !ok {
		return false
	}
	if _, isStr := c.(string); isStr {
		return true
	}
	if _, isArr := c.([]interface{}); isArr {
		return true
	}
	return false
}

func entryFromJSON(e interface{}) types.RawEntry {
	eo := e.(map[string]interface{})
	out := types.RawEntry{Role: eo["role"].(string)}
	if s, ok := eo["content"].(string); ok {
		out.Content = s
	} else if arr, ok := eo["content"].([]interface{}); ok {
		if parts, ok := partsFromJSON(arr); ok {
			out.Content = parts
		} else {
			// 形状无法识别 → 整体序列化为文本存根（保数据不丢，请求侧可解析）
			b, _ := json.Marshal(arr)
			out.Content = fmt.Sprintf("（附件内容载入形状异常：%s）", string(b))
		}
	}
	if tcid, ok := eo["tool_call_id"].(string); ok && tcid != "" {
		out.ToolCallID = &tcid
	}
	if rc, ok := eo["reasoning_content"].(string); ok && rc != "" {
		out.ReasoningContent = &rc
	}
	if tcs, ok := eo["tool_calls"].([]interface{}); ok {
		for _, tc := range tcs {
			tco, ok := tc.(map[string]interface{})
			if !ok {
				continue
			}
			f, _ := tco["function"].(map[string]interface{})
			name, _ := f["name"].(string)
			args, _ := f["arguments"].(string)
			id, _ := tco["id"].(string)
			out.ToolCalls = append(out.ToolCalls, types.ToolCall{ID: id, Type: "function", Function: types.Function{Name: name, Arguments: args}})
		}
	}
	return out
}

func partsFromJSON(arr []interface{}) ([]types.ContentPart, bool) {
	parts := make([]types.ContentPart, 0, len(arr))
	for _, p := range arr {
		m, ok := p.(map[string]interface{})
		if !ok {
			return nil, false
		}
		part, err := types.FromPartMap(m)
		if err != nil {
			return nil, false
		}
		parts = append(parts, part)
	}
	return parts, true
}
