// agent-wire —— agent 之间互发消息（只读发现 + 只在对方空闲时投递 + 取回那一轮的最终正文）
// 用法:
//   agent-wire list                       列出各 agent：在线?/在 run?/最近活动
//   agent-wire send <名字> <消息> [--wait N] [--json]
//                                         对方在 run 就直接拒；否则投递并等它那一轮 run_end，
//                                         把那一轮的最终正文打到 stdout（agent 直接读输出即可）
// 解耦契约：不 import OkHuman 任何模块，只走它的 HTTP（/status /events /enqueue /history）。
package main

import (

	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Peer struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"` // 经 lan-bridge 时用 Bearer
}

type Cfg struct {
	Peers         []Peer `json:"peers"`
	Self          string `json:"self"`
	DefaultWaitS  int    `json:"default_wait_sec"`
}

var httpc = &http.Client{Timeout: 8 * time.Second}

func loadCfg() *Cfg {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		dir = "."
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "读不到 config.json（应与可执行文件同目录）:", err)
		os.Exit(1)
	}
	c := &Cfg{DefaultWaitS: 600}
	if err := json.Unmarshal(b, c); err != nil {
		fmt.Fprintln(os.Stderr, "config.json 解析失败:", err)
		os.Exit(1)
	}
	if c.DefaultWaitS <= 0 {
		c.DefaultWaitS = 600
	}
	return c
}

func (p *Peer) get(path string) (*http.Response, error) {
	req, err := http.NewRequest("GET", strings.TrimRight(p.URL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	return httpc.Do(req)
}

func (p *Peer) post(path string, body any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", strings.TrimRight(p.URL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	return httpc.Do(req)
}

// ---------- list ----------

type status struct {
	Running  bool   `json:"running"`
	UserSeq  int    `json:"user_seq"`
	LastEnd  int64  `json:"-"`
	OK       bool
	Err      string
}

func fetchStatus(p *Peer) status {
	r, err := p.get("/status")
	if err != nil {
		return status{Err: "不可达: " + err.Error()}
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return status{Err: fmt.Sprintf("HTTP %d", r.StatusCode)}
	}
	var m map[string]any
	if json.NewDecoder(r.Body).Decode(&m) != nil {
		return status{Err: "返回不是 JSON"}
	}
	s := status{OK: true}
	if v, ok := m["running"].(bool); ok {
		s.Running = v
	}
	if v, ok := m["user_seq"].(float64); ok {
		s.UserSeq = int(v)
	}
	if hb, ok := m["heartbeat"].(map[string]any); ok {
		if v, ok := hb["last_run_end_ms"].(float64); ok {
			s.LastEnd = int64(v)
		}
	}
	return s
}

type batch struct {
	Seq    int      `json:"seq"`
	Frames []string `json:"frames"`
}

// pollSince 取游标之后的事件批次；返回 (latest, batches, err)
func pollSince(p *Peer, since int) (int, []batch, error) {
	r, err := p.get(fmt.Sprintf("/events/poll?since=%d", since))
	if err != nil {
		return since, nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return since, nil, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	var o struct {
		Batches []batch `json:"batches"`
		Latest  int     `json:"latest"`
	}
	if json.NewDecoder(r.Body).Decode(&o) != nil {
		return since, nil, fmt.Errorf("返回不是 JSON")
	}
	latest := o.Latest
	if latest < since {
		latest = since
	}
	return latest, o.Batches, nil
}


// ---------- 取那一轮的正文 ----------

// collectTurn 从游标 base 之后观察对方：累加本轮 delta.text，run_end 时返回整段正文。
// 拿不到返回空串（超时/没产出）。
func collectTurn(p *Peer, base, waitS int) string {

	var roundText *bytes.Buffer
	started := false
	cursor := base
	deadline := time.Now().Add(time.Duration(waitS) * time.Second)
	for time.Now().Before(deadline) {
		latest, batches, err := pollSince(p, cursor)
		if err == nil {
			for _, b := range batches {
				if b.Seq > cursor {
					cursor = b.Seq
				}
				for _, fr := range b.Frames {
					for _, dl := range strings.Split(fr, "\n") {
						if !strings.HasPrefix(dl, "data:") {
							continue
						}
						var e ev
						if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(dl, "data:"))), &e) != nil {
							continue
						}
						switch e.Type {
						case "run_start":
							started = true
							roundText = &bytes.Buffer{}
						case "delta":
							if roundText != nil && e.Text != "" {
								roundText.WriteString(e.Text)
							}
						case "text":
							if roundText != nil && strings.TrimSpace(e.Text) != "" {
								roundText.Reset()
								roundText.WriteString(e.Text)
							}
						case "run_end":
							if started && roundText != nil {
								return clean(roundText.String())
							}
						}
					}
				}
			}
			if latest > cursor {
				cursor = latest
			}
		}
		time.Sleep(2 * time.Second)
	}
	return ""
}

func clean(t string) string {
	if i := strings.LastIndex(t, "</think>"); i >= 0 {
		t = t[i+len("</think>"):]
	}
	return strings.TrimSpace(t)
}

// ---------- 异步：等完把正文以 /enqueue 投回发送方（靠主程序的轮内搭车进当前轮） ----------

type wait struct {
	ID     string `json:"id"`
	Peer   string `json:"peer"`
	Base   int    `json:"base"`
	Task   string `json:"task"`
	Self   string `json:"self"`
	Wait   int    `json:"wait"`
	Start  int64  `json:"start"`
}

func waitDir() string {
	exe, _ := os.Executable()
	d := filepath.Join(filepath.Dir(exe), "data", "waiting")
	os.MkdirAll(d, 0o755)
	return d
}

func (w *wait) path() string { return filepath.Join(waitDir(), w.ID+".json") }

func selfPeer(c *Cfg) *Peer {
	for _, p := range c.Peers {
		if p.Name == c.Self {
			pp := p
			return &pp
		}
	}
	return nil
}

func doPending(c *Cfg) {
	fs, _ := os.ReadDir(waitDir())
	if len(fs) == 0 {
		fmt.Println("没有在等的回信")
		return
	}
	fmt.Println("在等的回信（完成后会自动 /enqueue 投回，靠轮内搭车进我当前轮）:")
	for _, f := range fs {
		b, _ := os.ReadFile(filepath.Join(waitDir(), f.Name()))
		var w wait
		if json.Unmarshal(b, &w) == nil {
			fmt.Printf("  id=%s 等=%s 任务=%s 已等=%ds\n", w.ID, w.Peer, first(w.Task, 60), int(time.Now().Unix()-w.Start))
		}
	}
}

func first(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// doWaitInject：一次性 watcher 进程（跑完即退），等对方那一轮结束并把正文投回发送方
func doWaitInject(c *Cfg, id string) {
	b, err := os.ReadFile(filepath.Join(waitDir(), id+".json"))
	if err != nil {
		fmt.Println("没有这个等待任务:", id)
		return
	}
	var w wait
	if json.Unmarshal(b, &w) != nil {
		return
	}
	var peer *Peer
	for _, p := range c.Peers {
		if p.Name == w.Peer {
			pp := p
			peer = &pp
			break
		}
	}
	me := selfPeer(c)
	if peer == nil || me == nil {
		fmt.Println("配置缺 peer:", w.Peer, "或 self:", c.Self)
		return
	}
	text := collectTurn(peer, w.Base, w.Wait)
	if text == "" {
		text = clean(fallbackFromHistory(peer))
	}
	prefix := fmt.Sprintf("【agent-wire 回信 · 来自 %s】\n任务：%s\n", w.Peer, first(w.Task, 80))
	if text == "" {
		prefix = fmt.Sprintf("【agent-wire 回信超时 · 来自 %s】\n任务：%s\n（等了 %ds 没看到它那一轮结束；用 agent-wire list 看它是否还在 run）\n", w.Peer, first(w.Task, 80), w.Wait)
	}
	r, err := me.post("/enqueue", map[string]any{"message": prefix + text})
	if err == nil {
		r.Body.Close()
	}
	os.Remove(w.path())
	fmt.Printf("已投回 %s：%v\n", c.Self, err)
}

func doList(c *Cfg) {
	out := make([]string, len(c.Peers))
	var wg sync.WaitGroup
	for i, p := range c.Peers {
		wg.Add(1)
		go func(i int, p Peer) {
			defer wg.Done()
			s := fetchStatus(&p)
			who := p.Name
			if p.Name == c.Self {
				who += " (我自己)"
			}
			switch {
			case s.Err != "":
				out[i] = fmt.Sprintf("%-10s 离线   %s  [%s]", who, p.URL, s.Err)
			case s.Running:
				out[i] = fmt.Sprintf("%-10s 在 run  %s  user_seq=%d", who, p.URL, s.UserSeq)
			default:
				last := "无记录"
				if s.LastEnd > 0 {
					last = time.UnixMilli(s.LastEnd).Format("01-02 15:04")
				}
				out[i] = fmt.Sprintf("%-10s 空闲   %s  上次结束=%s", who, p.URL, last)
			}
		}(i, p)
	}
	wg.Wait()
	fmt.Println("可对话的 agent（在 run 的不接受消息）:")
	sort.Strings(out)
	for _, l := range out {
		fmt.Println("  " + l)
	}
}

// ---------- send ----------

type ev struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	Role string `json:"role"`
	Name string `json:"name"`
}

func doSend(c *Cfg, name, msg string, waitS int) {
	var peer *Peer
	for _, p := range c.Peers {
		if p.Name == name {
			pp := p
			peer = &pp
			break
		}
	}
	if peer == nil {
		names := []string{}
		for _, p := range c.Peers {
			names = append(names, p.Name)
		}
		fmt.Println("没有这个 agent：", name, "  可选:", strings.Join(names, ", "))
		os.Exit(1)
	}
	if name == c.Self {
		fmt.Println("不能给自己发：自己正在 run（这正是规则要防的）")
		os.Exit(2)
	}
	if st := fetchStatus(peer); st.Err != "" {
		fmt.Println("对方不可达：", name, st.Err)
		os.Exit(1)
	} else if st.Running {
		fmt.Printf("拒：%s 正在 run（user_seq=%d）——规则只允许给空闲的 agent 发\n", name, st.UserSeq)
		os.Exit(2)
	}

	// 先记下当前事件游标：之后只看新批次，不会把上一轮误当成本轮
	base, _, _ := pollSince(peer, -1)

	er, err := peer.post("/enqueue", map[string]any{"message": msg})
	if err != nil {
		fmt.Println("投递失败：", err)
		os.Exit(1)
	}
	eb, _ := io.ReadAll(er.Body)
	er.Body.Close()
	if er.StatusCode != 200 {
		fmt.Printf("投递 HTTP %d: %s\n", er.StatusCode, strings.TrimSpace(string(eb)))
		os.Exit(1)
	}

	// 一律异步：派完就返回，对方跑完由 watcher 把正文投回我的会话（靠轮内搭车进当前轮）
	w := &wait{ID: fmt.Sprintf("%d", time.Now().UnixNano()%100000000), Peer: name, Base: base,
		Task: msg, Self: c.Self, Wait: waitS, Start: time.Now().Unix()}
	bb, _ := json.Marshal(w)
	os.WriteFile(w.path(), bb, 0o644)
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "wait-inject", "--id", w.ID)
	lf, _ := os.OpenFile(filepath.Join(waitDir(), w.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = lf, lf, os.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Println("起 watcher 失败：", err)
		os.Exit(1)
	}
	fmt.Printf("已派活 id=%s 目标=%s（我不等，继续干我的）。它跑完后回信会带前缀【agent-wire 回信 · 来自 %s】投回我的会话，靠轮内搭车进我当前轮。在等谁：agent-wire pending\n", w.ID, name, name)
}


// /events 没拿到正文时兜底：从 /history 里取最后一条 assistant 文本
func fallbackFromHistory(p *Peer) string {
	r, err := p.get("/history")
	if err != nil {
		return ""
	}
	defer r.Body.Close()
	var h struct {
		Entries []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			Type    string `json:"type"`
		} `json:"entries"`
	}
	if json.NewDecoder(r.Body).Decode(&h) != nil {
		return ""
	}
	for i := len(h.Entries) - 1; i >= 0; i-- {
		e := h.Entries[i]
		if (e.Role == "assistant" || e.Role == "text") && strings.TrimSpace(e.Content) != "" {
			return e.Content
		}
	}
	return ""
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: agent-wire list | pending | send <名字> <消息> [--wait 秒]   （send 一律异步：派完即返回，回信靠轮内搭车进我当前轮）")
		os.Exit(1)
	}
	c := loadCfg()
	switch os.Args[1] {
	case "list":
		doList(c)
	case "pending":
		doPending(c)
	case "wait-inject":
		fs := flag.NewFlagSet("wait-inject", flag.ExitOnError)
		id := fs.String("id", "", "等待任务 id")
		_ = fs.Parse(os.Args[2:])
		doWaitInject(c, *id)
	case "send":
		rest := os.Args[2:]
		var pos []string
		valFlags := map[string]bool{"--wait": true}
		var flagArgs []string
		for i := 0; i < len(rest); i++ {
			tok := rest[i]
			if strings.HasPrefix(tok, "-") {
				if valFlags[tok] && i+1 < len(rest) {
					flagArgs = append(flagArgs, tok, rest[i+1])
					i++
				} else {
					flagArgs = append(flagArgs, tok)
				}
				continue
			}
			if i > 0 && len(flagArgs) > 0 {
				prev := flagArgs[len(flagArgs)-1]
				if valFlags[prev] {
					continue // 上一个 flag 的值，不是消息
				}
			}
			pos = append(pos, tok)
		}
		_ = pos
		fs := flag.NewFlagSet("send", flag.ExitOnError)
		wait := fs.Int("wait", c.DefaultWaitS, "等待对方那一轮结束的秒数上限")


		_ = fs.Parse(flagArgs)
		if len(pos) < 2 {
			fmt.Println("用法: agent-wire send <名字> <消息> [--wait 秒]")
			os.Exit(1)
		}
		doSend(c, pos[0], strings.Join(pos[1:], " "), *wait)
	default:
		fmt.Println("未知子命令:", os.Args[1])
		os.Exit(1)
	}
}
