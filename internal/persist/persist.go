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
	"strings"
	"sync"
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

// AtomicWriteFile 原子写（唯一 tmp + rename）：读端永远看到完整文件。
// tmp 名每次唯一（2026-09-14 审计修：旧固定 .tmp 名在并发 SaveSession 时
// ——/inject 与在飞 run 各自触发落盘——互相覆盖，rename 失败会把 session.json
// 留在陈旧内容上，进程恰在此窗口被杀则丢最近消息）
func AtomicWriteFile(file string, data string) error {
	f, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(data); err != nil {
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
	return nil
}

// Persistence 单实例落盘器
type Persistence struct {
	dir   string
	logMu sync.Mutex
}

func Init(dir string) (*Persistence, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Persistence{dir: dir}, nil
}

// Log 控制台 + 文件追加（>5MB 轮转 .old）；日志失败不影响主流程
func (p *Persistence) Log(msg string) {
	line := fmt.Sprintf("[%s] %s", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), msg)
	fmt.Println(line)
	go p.appendLogLine(line)
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

// LoadSessionFile 加载会话（无文件/损坏 → nil）。
// 新形状 { id, summary, messages }；旧三层形状 { id, raw, t1s, t2 } 加载时迁移
// （t2 + t1s 拼一条 summary，raw → messages，数据不丢）。
// 校验口径：no/seq 必须数字；summary 若存在必须 {content:string, created_at:number}；
// 每条消息 role 必须 string，content 必须 string 或数组（2026-09-13 修：/inject
// 附件消息的 content 是 parts 数组，旧校验只认 string → 带附件会话重启被拒载、
// 上下文全丢）。
func LoadSessionFile(dir string) (*Snapshot, error) {
	data, err := os.ReadFile(filepath.Join(dir, SessionFile))
	if err != nil {
		return nil, nil
	}
	var d struct {
		No  *int             `json:"no"`
		Seq *int             `json:"seq"`
		St  *json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.No == nil || d.Seq == nil || d.St == nil {
		return nil, nil
	}
	var st map[string]interface{}
	if err := json.Unmarshal(*d.St, &st); err != nil || st == nil {
		return nil, nil
	}
	if msgs, ok := st["messages"].([]interface{}); ok {
		// 新形状
		if err := checkSummary(st); err != nil {
			return nil, nil
		}
		for _, e := range msgs {
			if !validEntry(e) {
				return nil, nil
			}
		}
		snap := &Snapshot{No: *d.No, Seq: *d.Seq, State: &types.SessionState{ID: 1}}
		if v, ok := st["id"].(float64); ok {
			snap.State.ID = int(v)
		}
		for _, e := range msgs {
			snap.State.Messages = append(snap.State.Messages, entryFromJSON(e))
		}
		snap.State.Summary = summaryFromJSON(st)
		return snap, nil
	}
	// 旧形状（raw / t1s / t2）→ 迁移
	raw, ok1 := st["raw"].([]interface{})
	t1s, ok2 := st["t1s"].([]interface{})
	if !ok1 || !ok2 {
		return nil, nil
	}
	if err := checkT2(st); err != nil {
		return nil, nil
	}
	for _, e := range raw {
		if !validEntry(e) {
			return nil, nil
		}
	}
	var (
		parts     []string
		createdAt int64
		t2        *types.TEntry
	)
	if v, ok := st["t2"]; ok && v != nil {
		vo := v.(map[string]interface{})
		t2 = &types.TEntry{Content: vo["content"].(string), CreatedAt: int64(vo["created_at"].(float64))}
		parts = append(parts, t2.Content)
		createdAt = t2.CreatedAt
	}
	for _, t := range t1s {
		to, ok := t.(map[string]interface{})
		if !ok {
			return nil, nil // t1s 条目形状异常 → 整份拒载（与 validEntry 同口径，
		}
		if c, ok := to["content"].(string); ok {
			parts = append(parts, c)
			if ca, ok := to["created_at"].(float64); ok && createdAt == 0 {
				createdAt = int64(ca)
			}
		}
	}
	snap := &Snapshot{No: *d.No, Seq: *d.Seq, State: &types.SessionState{ID: 1}}
	if v, ok := st["id"].(float64); ok {
		snap.State.ID = int(v)
	}
	for _, e := range raw {
		snap.State.Messages = append(snap.State.Messages, entryFromJSON(e))
	}
	if len(parts) > 0 {
		snap.State.Summary = &types.TEntry{Content: strings.Join(parts, "\n\n"), CreatedAt: createdAt}
	}
	_ = t2
	return snap, nil
}

func checkSummary(st map[string]interface{}) error {
	v, ok := st["summary"]
	if !ok || v == nil {
		return nil
	}
	so, ok := v.(map[string]interface{})
	if !ok {
		return fmt.Errorf("summary 非对象")
	}
	if _, ok := so["content"].(string); !ok {
		return fmt.Errorf("summary.content 非字符串")
	}
	if _, ok := so["created_at"].(float64); !ok {
		return fmt.Errorf("summary.created_at 非数字")
	}
	return nil
}

func checkT2(st map[string]interface{}) error {
	v, ok := st["t2"]
	if !ok || v == nil {
		return nil
	}
	vo, ok := v.(map[string]interface{})
	if !ok {
		return fmt.Errorf("t2 非对象")
	}
	if _, ok := vo["content"].(string); !ok {
		return fmt.Errorf("t2.content 非字符串")
	}
	if _, ok := vo["created_at"].(float64); !ok {
		return fmt.Errorf("t2.created_at 非数字")
	}
	return nil
}

func summaryFromJSON(st map[string]interface{}) *types.TEntry {
	v, ok := st["summary"]
	if !ok || v == nil {
		return nil
	}
	so := v.(map[string]interface{})
	return &types.TEntry{Content: so["content"].(string), CreatedAt: int64(so["created_at"].(float64))}
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
