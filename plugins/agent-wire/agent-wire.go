// agent-wire —— agent 之间互发消息（只读发现 + 只在对方空闲时投递 + 取回那一轮的最终正文）
// 用法:
//
//	agent-wire list                       列出各 agent：在线?/在 run?/最近活动
//	agent-wire send <名字> <消息> [--wait N] [--json]
//	                                      对方在 run 就直接拒；否则投递并等它那一轮 run_end，
//	                                      把那一轮的最终正文打到 stdout（agent 直接读输出即可）
//
// 解耦契约：不 import OkHuman 任何模块，只走它的 HTTP（/status /events /enqueue /history）。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Peer struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"` // 经 lan-bridge 时用 Bearer
	Inst  string `json:"inst,omitempty"`  // 桥挂多实例时要哪个上游（8451/8452…）；桥只认 okh_inst cookie
}

// auth 带上桥的两把钥匙：Bearer 决定能不能进，okh_inst 决定打到哪个上游。
// 桥挂着多个上游时 inst 必填：缺了或写错，桥直接 400，不再静默落到 default_instance
// （2026-11-11 起；静默落错的表现为"回信来自另一个 agent"，看不见，所以必须让它响）。
// 桥只挂一个上游时可以不写。
func (p *Peer) auth(r *http.Request) {
	if p.Token != "" {
		r.Header.Set("Authorization", "Bearer "+p.Token)
	}
	if p.Inst != "" {
		r.Header.Set("Cookie", "okh_inst="+p.Inst)
	}
}

type Cfg struct {
	Peers []Peer `json:"peers"`
}

var httpc = &http.Client{Timeout: 8 * time.Second}

// 大响应单独放宽：事件 poll 可能积压到窗口上限（3MB）；/history 一律返回整个会话
// （含 reasoning，几 MB 很正常），8 秒会被当成本机故障。
var httpcBig = &http.Client{Timeout: 30 * time.Second}

func loadCfg() *Cfg {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	cfgPath := os.Getenv("AGENT_WIRE_CONFIG") // 要换配置文件时用；"我是谁"仍只由 OKHUMAN_SELF_URL 声明
	if cfgPath == "" {
		cfgPath = filepath.Join(dir, "config.json")
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读不到配置", cfgPath, "—— 放在可执行文件同目录，或用 AGENT_WIRE_CONFIG 指定；不退回到当前目录（会拿错 self/peers，回信投错实例）")
		os.Exit(1)
	}
	c := &Cfg{}
	if err := json.Unmarshal(b, c); err != nil {
		fmt.Fprintln(os.Stderr, "config.json 解析失败:", err)
		os.Exit(1)
	}
	return c
}

func (p *Peer) get(path string) (*http.Response, error) { return p.getWith(httpc, path) }

func (p *Peer) getWith(hc *http.Client, path string) (*http.Response, error) {
	req, err := http.NewRequest("GET", strings.TrimRight(p.URL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	p.auth(req)
	return hc.Do(req)
}

func (p *Peer) post(path string, body any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", strings.TrimRight(p.URL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	p.auth(req)
	return httpc.Do(req)
}

// ---------- 投递兜底 ----------

func undir() string {
	d := filepath.Join(waitDir(), "..", "undelivered")
	os.MkdirAll(d, 0o755)
	return d
}

// enqueueSelf 投回我自己的会话。失败必须重试并落盘，不许静默丢（真实丢过三次回信：
// 一次收口在中间状态、一次 /history 解码失败、一次 /enqueue 的返回值根本没看）。
// tryEnqueue 投一次，不重试。
func tryEnqueue(me *Peer, msg string) bool {
	r, err := me.post("/enqueue", map[string]any{"message": msg})
	if err == nil && r != nil && r.StatusCode < 300 {
		if r.Body != nil {
			r.Body.Close()
		}
		return true
	}
	if r != nil && r.Body != nil {
		r.Body.Close()
	}
	return false
}

func enqueueSelf(me *Peer, msg string) bool {
	for i := 0; i < 12; i++ {
		if tryEnqueue(me, msg) {
			return true
		}
		time.Sleep(20 * time.Second)
	}
	fn := filepath.Join(undir(), fmt.Sprintf("%d-%d.txt", time.Now().Unix(), time.Now().Nanosecond()))
	os.WriteFile(fn, []byte(msg), 0o644)
	fmt.Println("投递失败，已落盘等下次补投:", fn)
	return false
}

// flushUndelivered 下次任何 agent-wire 调用时把落盘的消息补投出去。
func flushUndelivered(me *Peer) {
	fs, _ := os.ReadDir(undir())
	for _, f := range fs {
		b, err := os.ReadFile(filepath.Join(undir(), f.Name()))
		if err != nil {
			continue
		}
		if tryEnqueue(me, string(b)) { // 单次：投不动就留在原地等下次，别在这儿睡 240 秒、也别复制一份
			os.Remove(filepath.Join(undir(), f.Name()))
			fmt.Println("补投了一条之前没投出去的回信:", f.Name())
		}
	}
}

// ---------- list ----------

type status struct {
	Running bool  `json:"running"`
	UserSeq int   `json:"user_seq"`
	LastEnd int64 `json:"-"`
	OK      bool
	Err     string
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
// pollSince 返回 (对端窗口最新批号 latest, 本次给的批次, gap, err)。
// latest 只能用来取基线，**不能拿来推游标**：单次 poll 最多给 maxPollBatches=50 批，
// 按 latest 推就跳过了没读到的批（2026-11-10 实测：63 批的一轮只收到前 48 段，run_end 也丢）。
// gap 是对端明确告诉我们的"被窗口挤掉的批数"，必须计下来，不能当没看见。
func pollSince(p *Peer, since int) (int, []batch, int, bool, error) {
	r, err := p.getWith(httpcBig, fmt.Sprintf("/events/poll?since=%d", since))
	if err != nil {
		return since, nil, 0, false, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return since, nil, 0, false, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	var o struct {
		Batches []batch `json:"batches"`
		Latest  int     `json:"latest"`
		Gap     int     `json:"gap"`
		Running bool    `json:"running"`
	}
	if json.NewDecoder(r.Body).Decode(&o) != nil {
		return since, nil, 0, false, fmt.Errorf("返回不是 JSON")
	}
	latest := o.Latest
	if latest < since {
		latest = since
	}
	return latest, o.Batches, o.Gap, o.Running, nil
}

// ---------- 取那一轮的正文 ----------

// collectTurn 从游标 base 之后观察对方：累加本轮 delta.text，run_end 时返回整段正文。
// 拿不到返回空串（超时/没产出）。
// collectTurn 的终止判据只有两个：连续不可达 10 次（对端重启/被杀）、已收口（事件流 run_end，
// 或 /status 不在 run 且游标确实前进）。CHECK_WIN 是"取一次并续期"的窗口，不是放弃时限——
// 对端长期在 run 就每窗口续期一次（没有"进程泄漏上限"那种东西，从前写在注释里是错的）。
const CHECK_WIN_S = 10800 // 三小时＝取一次并续期的窗口，不是放弃的时限

// collectTurn 观察对方那一轮。窗口（CHECK_WIN）到点不判失败：还在 run 就取末条消息当
// 中途状态返回（reason="stalled"），调用方据此续期再等；真 run 完（run_end 或 /status
// 显示 running=false）才收口。收口时若无事件流正文，取 /history 最后一条消息并标明类型。
func collectTurn(p *Peer, base int, task string) (string, string, string) {
	var roundText *bytes.Buffer
	started, cursor, missed, mine, ok := false, base, 0, false, false
	var sinceFail time.Time // 连续不可达的起点；一次成功就清零
	const deadAfter = 5 * time.Minute
	deadline := time.Now().Add(time.Duration(CHECK_WIN_S) * time.Second)
	for time.Now().Before(deadline) {
		ok = false // 每轮重新判定：不清零的话，一次失败会沿用上一轮的"成功"，守卫失效
		latest, batches, gap, running, err := pollSince(p, cursor)
		_ = latest // 见 pollSince：latest 不参与推游标
		if gap > 0 {
			missed += gap
		}
		if err != nil {
			// 按时间判死：跨机抖动、对端重启都可能连错几十秒；数次数会误判。
			if sinceFail.IsZero() {
				sinceFail = time.Now()
			}
			if time.Since(sinceFail) >= deadAfter {
				return "", "", "dead"
			}
		} else {
			ok = true
			sinceFail = time.Time{}
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
						case "run_msg", "user_piggyback":
							// 这一轮的输入里含我这条（新轮 = run_msg；被轮内搭车吃进当前轮 =
							// user_piggyback）→ 这轮的结束才是给我的。合并批次里我的原文也在，认它没错。
							if task != "" && strings.Contains(e.Message, task) {
								mine = true
							}
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
							if !mine {
								// 不是我的那一轮（别人的活先跑完）→ 不收口，继续等处理我这条的那一轮。
								break
							}
							// 有缺口时优先 /history：run 结束后本轮已持久化，那才是完整正文——
							// 事件流只留最近 200 批/3MB，被挤掉的段拿不回来，交一段截过的不如交完整的。
							if missed > 0 {
								if t, k := entryForTask(p, task); t != "" {
									return t, k, "run_end"
								}
								if t, _ := lastEntry(p); clean(t) != "" {
									return clean(t), fmt.Sprintf("正文（/history 末条；事件流有 %d 批被窗口挤掉，未走流式累加）", missed), "run_end"
								}
							}
							if started && roundText != nil && clean(roundText.String()) != "" {
								return clean(roundText.String()), "正文（本轮事件流 run_end 收口）" + missNote(missed), "run_end"
							}
							if started {
								t, k := lastEntry(p)
								return t, k, "ended"
							}
						}
					}
				}
			}
		}
		// 收口不要求见过 run_start：watcher 中途接上就永远收不了口（实测卡过一次）。
		// 只要对方不再在 run、且它这边确实有新消息，就取末条并标明类型。
		// 只用"同一次成功 poll 里的 running"判断收口：poll 失败时该字段是零值，不可信
		// （曾写成 running==false 就直接收口，会把跑到一半的正文当成已收口）。
		if ok && !running && cursor > base {
			// 没认出自己的输入时：它还在队列里就说明没轮到我，继续等；
			// 已不在队列（我接上太晚、错过了输入帧）才收口，并标明这条可能对不上。
			if !mine {
				if has, err := queuedHas(p, task); err == nil && has {
					time.Sleep(2 * time.Second)
					continue
				}
			}
			note := missNote(missed)
			if !mine {
				note += "｜没在输入里认出我的原文，这条可能不是给我的"
			}
			if roundText != nil && clean(roundText.String()) != "" {
				return clean(roundText.String()), "正文（对端已空闲，本轮只取到这些）" + note, "ended"
			}
			if t, k := entryForTask(p, task); t != "" {
				return t, k, "ended"
			}
			t, k := lastEntry(p)
			return t, k + note, "ended"
		}
		time.Sleep(2 * time.Second)
	}
	// 窗口到点：还在 run 就只当中途状态，不算回信，调用方续期
	t, k := lastEntry(p)
	return t, k, "stalled"
}

// missNote：有批次没取到时必须在回信里写明，不能交出一段看起来完整、其实被截过的正文。
func missNote(missed int) string {
	if missed <= 0 {
		return ""
	}
	return fmt.Sprintf("｜注意：有 %d 批事件没取到，正文可能不完整", missed)
}

var toolRE = regexp.MustCompile(`(?s)<tool_call.*?(?:</tool_call>|$)|<tool_result.*?(?:</tool_result>|$)|<invoke.*?(?:</invoke>|$)|<parameter.*?(?:</parameter>|$)|<arguments.*?(?:</arguments>|$)`)

// clean 去掉思考与工具调用块。真实丢过一次回信：对方 run 停在工具调用上，
// 整段"正文"其实只剩一个 <tool_call> 标记，照原样投回来等于投了个空。
func clean(t string) string {
	if i := strings.LastIndex(t, "</think>"); i >= 0 {
		t = t[i+len("</think>"):]
	}
	t = toolRE.ReplaceAllString(t, " ")
	return strings.TrimSpace(t)
}

// ---------- 异步：等完把正文以 /enqueue 投回发送方（靠主程序的轮内搭车进当前轮） ----------

type wait struct {
	Watcher int    `json:"watcher"` // watcher 进程 pid：ID 里的 pid 是 send 那一刻的短命进程，判不了活死
	Revives int    `json:"revives"` // 被重新接上次数的上限（防止 watcher 反复死掉形成循环）
	ID      string `json:"id"`
	Peer    string `json:"peer"`
	Base    int    `json:"base"`
	Task    string `json:"task"`
	Start   int64  `json:"start"`
}

// waitDir：优先实例自己声明的数据目录（$OKHUMAN_DATA_DIR/agent-wire/waiting，跟着实例走，
// 共用这份插件的多个实例互不搅）；没有声明就退回可执行文件同目录（终端手跑的老行为）。
func waitDir() string {
	if dd := strings.TrimSpace(os.Getenv("OKHUMAN_DATA_DIR")); dd != "" {
		d := filepath.Join(dd, "agent-wire", "waiting")
		os.MkdirAll(d, 0o700)
		return d
	}
	exe, _ := os.Executable()
	d := filepath.Join(filepath.Dir(exe), "data", "waiting")
	os.MkdirAll(d, 0o755)
	return d
}

func (w *wait) path() string { return filepath.Join(waitDir(), w.ID+".json") }

// atomicWrite 写等待记录必须原子：先清空再写的话，刚被拉起来的 watcher 可能读到空文件，
// Unmarshal 失败就一声不响地退出（2026-11-10 排查"接上的 watcher 一秒就没了"就是这个）。
func atomicWrite(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// logPath 是 watcher 自己的日志：doSend 建它，watcher 退出时删（此前从不删）。
func logPath(id string) string { return filepath.Join(waitDir(), id+".log") }

func normURL(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }

// selfURL 是"我自己是谁 / 回信投给谁"，只认实例的声明：OKHUMAN_SELF_URL ——
// 主程序起工具子进程时注入（internal/tools.SetSelfID），谁跑我谁说清我是谁，不靠我猜。
// 为什么不在 config 里留 self：同机多实例共用同一份插件时它会认错人——2026-11-11 实测
// 8452 读到了 8451 的 config（self: me），回信投进 8451、等待记录也落在同一个目录。
// 终端里手跑就自己声明：OKHUMAN_SELF_URL=http://127.0.0.1:8451 agent-wire send …
func selfURL(c *Cfg) string {
	return normURL(os.Getenv("OKHUMAN_SELF_URL"))
}

// selfPeer 取"我自己"那一条：URL 相同即同一个人（名字只是显示用），带上它的 token。
func selfPeer(c *Cfg) *Peer {
	u := selfURL(c)
	if u == "" {
		return nil
	}
	for _, p := range c.Peers {
		if normURL(p.URL) == u {
			pp := p
			return &pp
		}
	}
	return &Peer{Name: "本实例", URL: u}
}

// spawnWatcher 起（或重新接上）一个等待进程，返回它的 pid（必须记进记录，否则下次调用会把它当孤儿再接一个）。
func spawnWatcher(id string) (int, error) {
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "wait-inject", "--id", id)
	lf, _ := os.OpenFile(logPath(id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = lf, lf, os.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// putWatcherPid 把真 pid 记回等待记录。记录可能已经被跑完的 watcher 删掉了——那时**不写回**，
// 否则会留一条等不到的孤儿，下次调用接上它就多投一遍。
func putWatcherPid(path, id string, pid int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cur wait
	if json.Unmarshal(b, &cur) != nil || cur.ID != id {
		return
	}
	cur.Watcher = pid
	if bb, err := json.Marshal(cur); err == nil {
		_ = atomicWrite(path, bb)
	}
}

// alive 只看 /proc/<pid>。pid 复用时可能误判成"还活着"，那只是把下一次接上推到下回调用，不会丢消息。
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// reviveWatches 救孤儿：watcher 被我重启/机器重启/OOM 杀掉后，等待记录会一直挂在 waiting 里没人接。
// 每次调用插件（list/pending/send 都会走这里）顺手接上；接不上 5 次就明说等不到，别永远挂着。
// skipID：本进程正在服务的那条（wait-inject 自己也会走到这里）。不跳过的话，父进程还没把 pid
// 写回记录，我就看见"pid 不在"，给自己再接一个 watcher —— 同一个任务投两次。
func reviveWatches(c *Cfg, me *Peer, skipID string) {
	fs, _ := os.ReadDir(waitDir())
	for _, f := range fs {
		name := f.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(waitDir(), name)
		b, err := os.ReadFile(path)
		var w wait
		if err != nil || json.Unmarshal(b, &w) != nil || w.ID == "" {
			continue
		}
		if alive(w.Watcher) || w.ID == skipID {
			continue
		}
		if w.Revives >= 5 {
			enqueueSelf(me, fmt.Sprintf("【agent-wire 等不到 · 来自 %s】\n任务：%s\n（watcher 连续 5 次起不来，这条我不再等了；要重发我说一声）",
				w.Peer, first(w.Task, 80)))
			os.Remove(path)
			continue
		}
		w.Revives++
		if bb, err := json.Marshal(w); err == nil {
			_ = atomicWrite(path, bb)
		}
		pid, err := spawnWatcher(w.ID)
		if err != nil {
			fmt.Println("接上等待失败:", w.ID, err)
			continue
		}
		putWatcherPid(path, w.ID, pid)
		fmt.Printf("接上丢失的等待 id=%s pid=%d（第 %d 次）\n", w.ID, pid, w.Revives)
	}
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
			state := "watcher 已死（下次调用会自动接上）"
			if alive(w.Watcher) {
				state = fmt.Sprintf("watcher=%d 在等", w.Watcher)
			}
			fmt.Printf("  id=%s 等=%s 任务=%s 已等=%ds %s\n", w.ID, w.Peer, first(w.Task, 60), int(time.Now().Unix()-w.Start), state)
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
	defer os.Remove(logPath(id)) // 不论走哪条出口，日志都不留在盘上
	if err != nil {
		fmt.Println("没有这个等待任务:", id)
		return
	}
	var w wait
	if json.Unmarshal(b, &w) != nil {
		fmt.Println("等待记录读坏了，不再等:", id) // 以前这里静默退出，现场什么都看不出来
		return
	}
	if w.Watcher != os.Getpid() { // 报个活：不写回的话，下一次别的调用会把我当孤儿再接一个
		w.Watcher = os.Getpid()
		if bb, err := json.Marshal(w); err == nil {
			_ = atomicWrite(w.path(), bb)
		}
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
		fmt.Println("配置缺 peer:", w.Peer, "，或没有 OKHUMAN_SELF_URL —— 回信不知道该投给谁")
		return
	}
	for {
		text, kind, why := collectTurn(peer, w.Base, w.Task)
		prefix := fmt.Sprintf("【agent-wire 回信 · 来自 %s】\n任务：%s\n", w.Peer, first(w.Task, 80))
		if why == "stalled" {
			// 还在 run：只报中途状态并续期，不删等待记录
			enqueueSelf(me, fmt.Sprintf(
				"【agent-wire 仍在等 · 来自 %s】\n任务：%s\n（三小时窗口到点，它还在 run；末条是 %s，只当中途状态不算回信，续期再等三小时）\n%s",
				w.Peer, first(w.Task, 80), orX(kind, "无内容"), first(text, 1200)))
			continue
		}
		if why == "dead" {
			enqueueSelf(me, fmt.Sprintf(
				"【agent-wire 无回信 · 来自 %s】\n任务：%s\n（连续不可达：对端可能已重启或被杀）\n要不要我重问一次，等用户一句话。",
				w.Peer, first(w.Task, 80)))
		} else {
			enqueueSelf(me, prefix+fmt.Sprintf("（末条类型：%s）\n%s", orX(kind, "无内容"), text))
		}
		break
	}
	os.Remove(w.path())
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
	Type    string `json:"type"`
	Text    string `json:"text"`
	Message string `json:"message"` // run_msg / user_piggyback：本轮（或搭车）实际输入的原文
	Role    string `json:"role"`
	Name    string `json:"name"`
}

func doSend(c *Cfg, name, msg string) {
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
	if selfURL(c) == "" { // 认不出自己就不知道回信投给谁：先出声，别派一个永远收不到回的活
		// （2026-11-11 各实例 config 已删掉 self，身份只由实例声明给出，见 SetSelfID）
		fmt.Println("认不出我自己：没有 OKHUMAN_SELF_URL。在实例里跑由主程序注入；终端里手跑自己声明：OKHUMAN_SELF_URL=http://127.0.0.1:<端口> agent-wire send …")
		os.Exit(2)
	}
	if normURL(peer.URL) == selfURL(c) {
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

	// base 只是省流量的起点；**不把别人的轮当成我的，靠的是输入匹配（run_msg / user_piggyback），
	// 不是这个游标**——中途接上、或我的消息被排到别人后面时，光靠游标区分不了。
	base, _, _, _, _ := pollSince(peer, -1)

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
	w := &wait{ID: fmt.Sprintf("%d%x", os.Getpid(), time.Now().UnixNano()), Peer: name, Base: base,
		Task: msg, Start: time.Now().Unix()}
	bb, _ := json.Marshal(w)
	if err := atomicWrite(w.path(), bb); err != nil {
		fmt.Println("写等待记录失败，未派活：", err)
		os.Exit(1)
	}
	pid, err := spawnWatcher(w.ID)
	if err != nil {
		os.Remove(w.path()) // 不留下等不到的记录，免得 pending 里永远挂着一条
		fmt.Println("起 watcher 失败，已撤回等待记录：", err)
		os.Exit(1)
	}
	putWatcherPid(w.path(), w.ID, pid)
	fmt.Printf("已派活 id=%s 目标=%s（我不等，继续干我的）。它跑完后回信会带前缀【agent-wire 回信 · 来自 %s】投回我的会话，靠轮内搭车进我当前轮。在等谁：agent-wire pending\n", w.ID, name, name)
}

// queuedHas 对端的消息队列里还有没有我这条（还在排队 = 没轮到我，不该收口）。
func queuedHas(p *Peer, task string) (bool, error) {
	r, err := p.get("/queue")
	if err != nil {
		return false, err
	}
	defer r.Body.Close()
	var q struct {
		Queued []string `json:"queued"`
	}
	if json.NewDecoder(r.Body).Decode(&q) != nil {
		return false, fmt.Errorf("返回不是 JSON")
	}
	for _, x := range q.Queued {
		if x == task || strings.Contains(x, task) {
			return true, nil
		}
	}
	return false, nil
}

type histEntry struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // 可能是字符串，也可能是分段数组
	Reasoning  string          `json:"reasoning_content"`
	ToolCallID string          `json:"tool_call_id"`
}

// entryText 取条目的可见文本（字符串或分段数组两种形状都见过）。
func entryText(e histEntry) string {
	var raw string
	if json.Unmarshal(e.Content, &raw) != nil {
		var parts []struct {
			Text    string `json:"text"`
			Content string `json:"content"`
		}
		if json.Unmarshal(e.Content, &parts) == nil {
			for _, q := range parts {
				raw += q.Text + q.Content
			}
		}
	}
	return raw
}

// getHistory 取对端整段会话。/history **没有 limit 参数**（服务端不看这个参数），响应必然全量。
func getHistory(p *Peer) ([]histEntry, error) {
	r, err := p.getWith(httpcBig, "/history")
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	var h struct {
		Entries []histEntry `json:"entries"`
	}
	if json.NewDecoder(r.Body).Decode(&h) != nil {
		return nil, fmt.Errorf("返回不是 JSON")
	}
	return h.Entries, nil
}

// entryForTask 在 /history 里定位"含我这条原文的 user 条目"，取同一段里它之后最后一条有正文的
// assistant —— 那才是回答我的那条。续期久了事件帧早被窗口挤掉、认不出归属时，只有这个能对上。
func entryForTask(p *Peer, task string) (string, string) {
	if task == "" {
		return "", ""
	}
	es, err := getHistory(p)
	if err != nil {
		return "", ""
	}
	idx := -1
	for i, e := range es {
		if e.Role == "user" && strings.Contains(entryText(e), task) {
			idx = i // 取最后一次命中：同一条重发时以最近那次为准
		}
	}
	if idx < 0 {
		return "", ""
	}
	best := ""
	for j := idx + 1; j < len(es) && es[j].Role != "user"; j++ {
		if es[j].Role == "assistant" {
			if t := strings.TrimSpace(entryText(es[j])); t != "" {
				best = t
			}
		}
	}
	if best == "" {
		return "", ""
	}
	return clean(best), "正文（/history：我那条之后的回答）"
}

// lastEntry 取对端 /history 的最后一条消息，并标明它是什么：正文 / 思考 / 工具调用 / 工具结果 / 输入。
func lastEntry(p *Peer) (string, string) {
	es, err := getHistory(p)
	if err != nil || len(es) == 0 {
		return "", "取不到（/history 请求失败或为空）"
	}
	e := es[len(es)-1]
	raw := entryText(e)
	switch {
	case e.Role == "tool":
		return raw, fmt.Sprintf("工具返回结果（role=tool, %s）", e.ToolCallID)
	case e.Role == "user":
		return raw, "它收到的输入（role=user，多半是我或系统发的）"
	case strings.TrimSpace(raw) != "":
		return raw, "正文（role=assistant）"
	case strings.TrimSpace(e.Reasoning) != "":
		return e.Reasoning, "思考（role=assistant 的 reasoning，无正文——它还在中途）"
	default:
		return "", "末条为空（role=" + e.Role + "）"
	}
}
func orX(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: agent-wire list | pending | send <名字> <消息>   （send 一律异步：派完即返回，回信靠轮内搭车进我当前轮）")
		os.Exit(1)
	}
	c := loadCfg()
	if me := selfPeer(c); me != nil {
		flushUndelivered(me) // 定义了却一直没调用过（2026-11-10 修）：抽屉里的回信真去补投
		myID := ""
		if os.Args[1] == "wait-inject" { // 本进程在等的那条 id
			for i, a := range os.Args {
				if a == "--id" && i+1 < len(os.Args) {
					myID = os.Args[i+1]
				}
			}
		}
		reviveWatches(c, me, myID) // 同上：watcher 死了没人接，等待记录会永远挂着
	}
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
		// 名字之前的 token 才当 flag；名字之后的一律原样进消息。旧写法把任何以 "-"
		// 开头的词都当 flag，结果「- 先改封面」这种列表式消息整条发不出去（退出码 2）。
		var pos []string
		var flagArgs []string
		skipNext := false
		for i, tok := range rest {
			if skipNext {
				skipNext = false
				continue
			}
			if tok == "--wait" || tok == "-wait" {
				if i+1 < len(rest) {
					flagArgs = append(flagArgs, tok, rest[i+1])
					skipNext = true
				} else {
					flagArgs = append(flagArgs, tok)
				}
				continue
			}
			if len(pos) == 0 && strings.HasPrefix(tok, "-") {
				flagArgs = append(flagArgs, tok)
				continue
			}
			pos = append(pos, tok)
		}
		if len(flagArgs) > 0 { // 显式拒绝，不静默当没看见（--wait 曾解析了却从不生效）
			fmt.Println("不支持的开关:", strings.Join(flagArgs, " "), "—— 等待语义固定是「取一次 + 每 3 小时续期」，见 README §规则")
			os.Exit(2)
		}
		if len(pos) < 2 {
			fmt.Println("用法: agent-wire send <名字> <消息>")
			os.Exit(1)
		}
		doSend(c, pos[0], strings.Join(pos[1:], " "))
	default:
		fmt.Println("未知子命令:", os.Args[1])
		os.Exit(1)
	}
}
