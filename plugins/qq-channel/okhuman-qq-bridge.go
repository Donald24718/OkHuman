// OkHuman_p/qq-channel：OkHuman 的 QQ 官方渠道桥（独立进程）——TS 版 bridge.ts + qq.ts 的 1:1 Go 移植。
//
// 链路（常驻转发管道）：
//   上行：QQ 消息 → POST {实例}/enqueue { message }（入队即返，不等回合结束）
//   下行：桥常驻订阅 GET {实例}/events（SSE 广播）→ 按事件边界整段拆气泡发：
//         thinking 整段 ≤2950 字（[思考] 前缀）/ text 整段 ≤3000 字 / tool_call [工具] /
//         tool_result [结果] / finish flushAll+reply 兜底 / error [出错]
//         轮内 60s 静默 → 15s 轮询 /history 补发回尾 → 10min 放弃
//
// 运行：./okhuman-qq-bridge（输出重定向落日志）。配置：config.json（client_secret 不入日志）。
// Token 盘缓存：<data_dir>/channels/qq/tokens/<botId>.json（原子写）。
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ---------- 常量（与 TS 版一致） ----------

const (
	tokenURL            = "https://bots.qq.com/app/getAppAccessToken"
	wsURL               = "wss://api.sgroup.qq.com/websocket?v=1"
	tokenRefreshAheadMs = 5 * 60 * 1000
	reauthAfter         = 3
	thinkBubbleChars    = 2950 // thinking 留 [思考] 前缀余量
	textBubbleChars     = 3000
	minSendGapMs        = 1000 // c2c 连发限流
	stuckSilenceMs      = 60_000
	stuckPollMs         = 15_000
	stuckMaxMs          = 10 * 60_000
	sseReadWatchdog     = 25 * time.Second // 服务端 5s 心跳，25s 无数据 = 静默断线
	eventsReconnect     = 2 * time.Second
)

// 官方渠道新版位编号：频道消息 + 交互 + DM(1<<12) + 群/C2C(1<<25)
var intents = (1 << 30) | (1 << 26) | (1 << 12) | (1 << 25)

// c2cMsgSeq：c2c markdown 消息的全局递增序号（TS 版同为模块级共享）
var c2cMu sync.Mutex
var c2cMsgSeq int
var c2cSeqFile string // 持久化文件：QQ 要求 msg_seq 单调递增，重启归 1 可能被拒

func nextC2CSeq() int {
	c2cMu.Lock()
	c2cMsgSeq++
	if c2cSeqFile != "" {
		tmp := c2cSeqFile + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.Itoa(c2cMsgSeq)), 0644); err == nil {
			_ = os.Rename(tmp, c2cSeqFile)
		}
	}
	c2cMu.Unlock()
	return c2cMsgSeq
}

func c2cSeqSnapshot() int {
	c2cMu.Lock()
	defer c2cMu.Unlock()
	return c2cMsgSeq
}

// ---------- 小工具 ----------

func mask(s string) string {
	if utf16len(s) <= 4 {
		return "****"
	}
	return jsslice(s, 4) + "****"
}

// utf16len：JS 的 string.length 按 UTF-16 码元计（BMP 字符 1，astral 2）
func utf16len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// jsslice：按 UTF-16 码元取前 n 个（等价 JS 的 s.slice(0, n)，不切半码元）
func jsslice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	var b strings.Builder
	for _, r := range s {
		u := 1
		if r > 0xFFFF {
			u = 2
		}
		if count+u > n {
			break
		}
		b.WriteRune(r)
		count += u
	}
	return b.String()
}

// 滑动窗口限流（1 分钟窗口）
type WindowLimiter struct {
	mu    sync.Mutex
	limit int
	hits  []int64
}

func (w *WindowLimiter) allow(now int64) bool {
	// 每条消息一个 goroutine 并发调用 → 必须持锁（否则 hits 并发 append = 数据竞争）
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := now - 60_000
	for len(w.hits) > 0 && w.hits[0] <= cutoff {
		w.hits = w.hits[1:]
	}
	if w.limit > 0 && len(w.hits) >= w.limit {
		return false
	}
	w.hits = append(w.hits, now)
	return true
}

// ---------- 长文本分块（TS splitText 移植：换行边界切分；跨块围栏补开/闭；表格原子化） ----------

var qqMdFenceRe = regexp.MustCompile("^(`{3,}|~{3,})")
var qqMdTableSepRe = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?(\s*\|\s*:?-{3,}:?)*\s*\|?\s*$`)

func isMdTableSeparator(line string) bool {
	return qqMdTableSepRe.MatchString(line) && strings.Contains(line, "-")
}

func collectMdTableLines(lines []string, start int) ([]string, int) {
	tableLines := []string{lines[start], lines[start + 1]}
	cursor := start + 2
	for cursor < len(lines) && strings.Contains(lines[cursor], "|") {
		if qqMdFenceRe.MatchString(strings.TrimSpace(lines[cursor])) {
			break
		}
		tableLines = append(tableLines, lines[cursor])
		cursor++
	}
	return tableLines, cursor
}

func splitMdTableBlock(tableLines []string, maxLen int) []string {
	if len(tableLines) < 2 {
		return []string{strings.Join(tableLines, "\n")}
	}
	header, separator := tableLines[0], tableLines[1]
	dataRows := tableLines[2:]
	if len(dataRows) == 0 {
		return []string{strings.Join(tableLines, "\n")}
	}
	preamble := header + "\n" + separator
	var chunks []string
	var currentRows []string
	currentLen := utf16len(preamble) + 1
	for _, row := range dataRows {
		rowLen := utf16len(row) + 1
		if len(currentRows) > 0 && currentLen+rowLen > maxLen {
			chunks = append(chunks, preamble+"\n"+strings.Join(currentRows, "\n"))
			currentRows = nil
			currentLen = utf16len(preamble) + 1
		}
		currentRows = append(currentRows, row)
		currentLen += rowLen
	}
	if len(currentRows) > 0 {
		chunks = append(chunks, preamble+"\n"+strings.Join(currentRows, "\n"))
	}
	return chunks
}

func splitText(text string, maxLen int) []string {
	if text == "" {
		return nil
	}
	if utf16len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	var current []string
	length := 0
	fenceOpen := ""
	flush := func() {
		body := strings.TrimRight(strings.Join(current, ""), "\n")
		if fenceOpen != "" {
			body += "\n```"
		}
		chunks = append(chunks, body)
		current = nil
		length = 0
	}
	flushFor := func(incomingLen int) {
		if len(current) == 0 || length+incomingLen <= maxLen {
			return
		}
		savedFence := fenceOpen
		flush()
		if savedFence != "" {
			reopener := savedFence + "\n"
			current = append(current, reopener)
			length = utf16len(reopener)
		}
	}
	emitLine := func(line string) {
		lineWithNl := line + "\n"
		flushFor(utf16len(lineWithNl))
		if utf16len(lineWithNl) > maxLen {
			// 行本身超限 → 按 UTF-16 长度强制切（按 rune 数切对全增补字符行会产出 2 倍长度被拒收）
			var cur strings.Builder
			for _, r := range line {
				if cur.Len() > 0 && utf16len(cur.String())+utf16len(string(r)) > maxLen {
					chunks = append(chunks, cur.String())
					cur.Reset()
				}
				cur.WriteRune(r)
			}
			if cur.Len() > 0 {
				chunks = append(chunks, cur.String())
			}
			return
		}
		current = append(current, lineWithNl)
		length += utf16len(lineWithNl)
	}
	emitTableChunk := func(tableText string) {
		block := strings.TrimRight(tableText, "\n") + "\n"
		if len(current) > 0 && length+utf16len(block) > maxLen {
			flush()
		}
		if utf16len(block) > maxLen && len(current) == 0 {
			chunks = append(chunks, strings.TrimRight(block, "\n"))
			return
		}
		current = append(current, block)
		length += utf16len(block)
	}
	lines := strings.Split(text, "\n")
	i := 0
	for i < len(lines) {
		line := lines[i]
		stripped := strings.TrimSpace(line)
		if qqMdFenceRe.MatchString(stripped) {
			if fenceOpen != "" {
				fenceOpen = ""
			} else {
				fenceOpen = stripped
			}
			emitLine(line)
			i++
			continue
		}
		isTableStart := fenceOpen == "" && strings.Contains(line, "|") && i+1 < len(lines) && isMdTableSeparator(lines[i+1])
		if isTableStart {
			tableLines, nextI := collectMdTableLines(lines, i)
			for _, tc := range splitMdTableBlock(tableLines, maxLen) {
				emitTableChunk(tc)
			}
			i = nextI
			continue
		}
		emitLine(line)
		i++
	}
	if len(current) > 0 {
		body := strings.TrimRight(strings.Join(current, ""), "\n")
		if fenceOpen != "" {
			body += "\n```"
		}
		chunks = append(chunks, body)
	}
	var out []string
	for _, c := range chunks {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

// ---------- URL 净化（QQ 纯文本拒收链接；两级降级） ----------

var urlPattern = regexp.MustCompile(`(?i)https?://[^\s]+|www\.[^\s]+`)
var bareDomainPattern = regexp.MustCompile(`(?i)https?://[^\s]+|www\.[^\s]+|\b[\w][\w.-]*\.(?:com|cn|org|net|edu|gov|io|co|cc|tv|me|info|biz|app|dev|top|xyz|site|vip|shop|tech|club|pro|live|mobi|asia|wiki)(?:\.[a-z]{2,3})?\b(?:/[^\s]*)?`)
const urlPlaceholder = "[链接已省略]"

func sanitizeQQText(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	n := 0
	out := urlPattern.ReplaceAllStringFunc(text, func(string) string {
		n++
		return urlPlaceholder
	})
	return out, n > 0
}

func aggressiveSanitizeQQText(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	n := 0
	out := bareDomainPattern.ReplaceAllStringFunc(text, func(string) string {
		n++
		return urlPlaceholder
	})
	return out, n > 0
}

func isUrlContentError(body string) bool {
	return strings.Contains(body, "304003") || strings.Contains(body, "40034028") || strings.Contains(body, "不允许包含url")
}

// markdown 发送校验失败标记（50056 / 40034012 / msg_type…）→ 降级纯文本
func isMarkdownRejectPayload(payloadText string) bool {
	return strings.Contains(payloadText, "markdown") ||
		strings.Contains(payloadText, "msg_type") ||
		strings.Contains(payloadText, "msg type") ||
		strings.Contains(payloadText, "message type") ||
		strings.Contains(payloadText, "50056") ||
		strings.Contains(payloadText, "40034012")
}

// ---------- 配置 ----------

type InstanceCfg struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type BotCfg struct {
	AppID         string `json:"app_id"`
	ClientSecret  string `json:"client_secret"`
	ChannelID     string `json:"channel_id"`
	Instance      string `json:"instance"`
	MaxMsgsPerMin int    `json:"max_msgs_per_min"`
}

type SharedCfg struct {
	DataDir          string `json:"data_dir"`
	Enabled          bool   `json:"enabled"`
	ReconnectMs      int    `json:"reconnect_ms"`
	ReauthMs         int    `json:"reauth_ms"`
	HeartbeatMs      int    `json:"heartbeat_ms"`
	MaxQueuePerMin   int    `json:"max_queue_per_min"`
	MaxMsgChars      int    `json:"max_msg_chars"`
	SendFailStop     int    `json:"send_fail_stop"`
	AckMessage       string `json:"ack_message"`
	MarkdownEnabled  bool `json:"markdown_enabled"`
}

type BridgeConfig struct {
	Shared    SharedCfg
	Bots      []BotCfg
	Instances []InstanceCfg
}

func defaultSharedCfg() SharedCfg {
	return SharedCfg{
		DataDir:          "data",
		Enabled:          true,
		ReconnectMs:      3000,
		ReauthMs:         30000,
		HeartbeatMs:      30000,
		MaxQueuePerMin:   100,
		MaxMsgChars:      4000,
		SendFailStop:     5,
		AckMessage:       "收到",
		MarkdownEnabled:  true,
	}
}

// 插件根目录：exe 同目录有 config.json 用它；否则回退 CWD
func pluginRoot() string {
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		if _, e := os.Stat(filepath.Join(d, "config.json")); e == nil {
			return d
		}
	}
	return "."
}

func mustJSON(v interface{}) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSpace(b.String())
}

func loadConfig(root string) (*BridgeConfig, error) {
	raw, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("config.json 读取失败（需含 instances[] + bots[]）：%v", err)
	}
	cfg := &BridgeConfig{Shared: defaultSharedCfg()}
	if err := json.Unmarshal(raw, &cfg.Shared); err != nil {
		return nil, fmt.Errorf("config.json 读取失败（需含 instances[] + bots[]）：%v", err)
	}
	var file struct {
		Bots      []BotCfg      `json:"bots"`
		Instances []InstanceCfg `json:"instances"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("config.json 读取失败（需含 instances[] + bots[]）：%v", err)
	}
	cfg.Bots = file.Bots
	if len(file.Instances) == 0 {
		return nil, fmt.Errorf("config.json 缺 instances[]（至少一项 { id, url }）")
	}
	seen := map[string]bool{}
	for _, it := range file.Instances {
		if it.ID == "" || !strings.HasPrefix(it.URL, "http") {
			return nil, fmt.Errorf("instances[] 每项需 { id: 非空字符串, url: http(s) 地址 }：%s", mustJSON(it))
		}
		if seen[it.ID] {
			return nil, fmt.Errorf("instances[].id 重复：%s", it.ID)
		}
		seen[it.ID] = true
	}
	cfg.Instances = file.Instances
	if !filepath.IsAbs(cfg.Shared.DataDir) {
		cfg.Shared.DataDir = filepath.Join(root, cfg.Shared.DataDir)
	}
	// 数字字段 ≤0 一律回退默认（否则 NewTicker(0) panic / 重连紧循环 / 限流失效）
	if cfg.Shared.HeartbeatMs <= 0 {
		cfg.Shared.HeartbeatMs = 30000
	}
	if cfg.Shared.ReconnectMs <= 0 {
		cfg.Shared.ReconnectMs = 3000
	}
	if cfg.Shared.ReauthMs <= 0 {
		cfg.Shared.ReauthMs = 30000
	}
	if cfg.Shared.MaxQueuePerMin <= 0 {
		cfg.Shared.MaxQueuePerMin = 100
	}
	if cfg.Shared.MaxMsgChars <= 0 {
		cfg.Shared.MaxMsgChars = 4000
	}
	if cfg.Shared.SendFailStop <= 0 {
		cfg.Shared.SendFailStop = 5
	}
	return cfg, nil
}

// ---------- QQ 网关（TS QQChannel 移植） ----------

type tokenVal struct {
	value     string
	expiresAt int64
}

type qqAuthor struct {
	Bot        *bool   `json:"bot"`
	ID         *string `json:"id"`
	UserOpenID *string `json:"user_openid"`
}

// 频道/C2C 消息载荷（超集结构）
type qqMsg struct {
	ChannelID   *string         `json:"channel_id"`
	Content     *string         `json:"content"`
	Author      *qqAuthor       `json:"author"`
	ID          *string         `json:"id"`
	UserOpenID  *string         `json:"user_openid"`
	Attachments []*qqAttachment `json:"attachments"` // 2026-09-15 方向A：用户发来附件
}

// qqAttachment 消息附件（2026-09-15 方向A，用到的字段：CDN 直链 url + 原文件名 + 类型 + 语音识别文本）
type qqAttachment struct {
	URL          *string `json:"url"`
	Filename     *string `json:"filename"`
	ContentType  *string `json:"content_type"`
	ASRReferText *string `json:"asr_refer_text"`
}

type gatewayFrame struct {
	Op int             `json:"op"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
	D  json.RawMessage `json:"d"`
}

type QQChannel struct {
	shared    SharedCfg
	bot       BotCfg
	botID     string
	queueLim  *WindowLimiter
	agentLim  *WindowLimiter
	tokenFile string
	onStream      func(string) error
	onMessage     func(string) (string, error)
	onAttachments func(string) error // 2026-09-15 方向A：附件落盘后 /inject 路径文本通知
	logf          func(string)

	mu             sync.Mutex
	token          *tokenVal
	seq            int
	sessionID      string
	z              int
	ws             *websocket.Conn
	heartbeatStop  chan struct{}
	reconnectTimer *time.Timer
	stopped        bool
	abnormalRecon  int
	sendFailStreak int

	wmu sync.Mutex
}

func NewQQChannel(shared SharedCfg, bot BotCfg, dataDir string,
	onStream func(string) error, onMessage func(string) (string, error),
	onAttachments func(string) error, logf func(string)) *QQChannel {
	return &QQChannel{
		shared:        shared,
		bot:           bot,
		botID:         bot.AppID,
		queueLim:      &WindowLimiter{limit: shared.MaxQueuePerMin},
		agentLim:      &WindowLimiter{limit: bot.MaxMsgsPerMin},
		tokenFile:     filepath.Join(dataDir, "channels", "qq", "tokens", bot.AppID+".json"),
		onStream:      onStream,
		onMessage:     onMessage,
		onAttachments: onAttachments,
		logf:          logf,
	}
}

func (c *QQChannel) start() error {
	c.mu.Lock()
	c.loadTokenCacheLocked()
	c.mu.Unlock()
	return c.connect(false)
}

func (c *QQChannel) stop() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.clearTimers()
	c.closeSocket()
}

// sendTo：向指定目标发一条消息（供桥级 /events 常驻转发循环调用）
func (c *QQChannel) sendTo(target, text string) error {
	return c.sendReply(target, text)
}

func (c *QQChannel) clearTimers() {
	c.mu.Lock()
	if c.reconnectTimer != nil {
		c.reconnectTimer.Stop()
		c.reconnectTimer = nil
	}
	c.stopHeartbeatLocked()
	c.mu.Unlock()
}

// ---------- token ----------

func (c *QQChannel) loadTokenCacheLocked() {
	if c.token != nil {
		return
	}
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return // 无缓存
	}
	var j struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		c.logf("token 缓存文件损坏，忽略")
		return
	}
	if j.AccessToken != "" && j.ExpiresAt > time.Now().UnixMilli()+tokenRefreshAheadMs {
		c.token = &tokenVal{value: j.AccessToken, expiresAt: j.ExpiresAt}
		c.logf("token 盘缓存已加载（mask=" + mask(j.AccessToken) + "）")
	}
}

func (c *QQChannel) fetchTokenLocked() (*tokenVal, error) {
	body, _ := json.Marshal(map[string]string{"appId": c.bot.AppID, "clientSecret": c.bot.ClientSecret})
	req, _ := http.NewRequest("POST", tokenURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token 获取失败：%v", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("token 获取失败：HTTP %d", res.StatusCode)
	}
	var j struct {
		AccessToken string      `json:"access_token"`
		ExpiresIn   json.Number `json:"expires_in"` // QQ 返回字符串 "7156"（TS 靠 JS 隐式转型掩盖，Go 严格解析会整包失败——2026-09-19 修）
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil || j.AccessToken == "" {
		return nil, fmt.Errorf("token 获取失败：响应无 access_token")
	}
	einStr := "?"
	ein := 7200
	if j.ExpiresIn.String() != "" {
		if n, err := j.ExpiresIn.Int64(); err == nil && n > 0 {
			ein = int(n)
			einStr = j.ExpiresIn.String()
		}
	}
	now := time.Now().UnixMilli()
	t := &tokenVal{value: j.AccessToken, expiresAt: now + int64(ein) * 1000}
	c.token = t
	c.saveTokenCacheLocked(t)
	c.logf("token 已获取（mask=" + mask(t.value) + "，expires_in=" + einStr + "s）")
	return t, nil
}

func (c *QQChannel) saveTokenCacheLocked(t *tokenVal) {
	dir := filepath.Dir(c.tokenFile)
	_ = os.MkdirAll(dir, 0755)
	b, _ := json.Marshal(map[string]interface{}{"access_token": t.value, "expires_at": t.expiresAt})
	tmp := c.tokenFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil { // 0600：内容是 live access_token
		return
	}
	_ = os.Rename(tmp, c.tokenFile) // 原子替换
}

func (c *QQChannel) ensureToken(force bool) (*tokenVal, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if force {
		c.token = nil // 强制（9009 / 升级重认证）→ 跳过内存与盘缓存
		return c.fetchTokenLocked()
	}
	c.loadTokenCacheLocked()
	if c.token != nil && c.token.expiresAt > time.Now().UnixMilli()+tokenRefreshAheadMs {
		return c.token, nil
	}
	c.token = nil // 过期 → 重取
	return c.fetchTokenLocked()
}

// ---------- WS ----------

// 主动关当前 socket（this.ws 先置空，readLoop 收尾判 stale 后不会再排重连）
func (c *QQChannel) closeSocket() {
	c.mu.Lock()
	ws := c.ws
	c.ws = nil
	c.stopHeartbeatLocked()
	c.mu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
}

func (c *QQChannel) connect(forceToken bool) error {
	token, err := c.ensureToken(forceToken)
	if err != nil {
		c.mu.Lock()
		stopped := c.stopped
		c.mu.Unlock()
		if stopped {
			return nil
		}
		c.logf("连接失败：" + err.Error())
		c.scheduleReconnect("connect-fail", forceToken || strings.Contains(err.Error(), "token"))
		return nil
	}
	if err := c.openSocket(token); err != nil {
		c.mu.Lock()
		stopped := c.stopped
		c.mu.Unlock()
		if stopped {
			return nil
		}
		c.logf("连接失败：" + err.Error())
		c.scheduleReconnect("connect-fail", forceToken || strings.Contains(err.Error(), "token"))
	}
	return nil
}

func (c *QQChannel) openSocket(token *tokenVal) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.Dial(wsURL+"&access_token="+url.QueryEscape(token.value), nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.ws = ws
	resume := c.sessionID != "" && c.seq >= 0
	sess, seq := c.sessionID, c.seq
	c.mu.Unlock()
	if resume {
		c.logf(fmt.Sprintf("网关已连接（resume session，seq=%d）", seq))
		c.send(map[string]interface{}{"op": 2, "d": map[string]interface{}{
			"token": "QQBot " + token.value, "session_id": sess, "seq": seq, "shard": []int{0, 1},
		}})
	} else {
		c.logf("网关已连接（IDENTIFY，token mask=" + mask(token.value) + "）")
		c.send(map[string]interface{}{"op": 2, "d": map[string]interface{}{
			"token": "QQBot " + token.value, "intents": intents, "shard": []int{0, 1},
		}})
	}
	go c.readLoop(ws)
	return nil
}

func (c *QQChannel) readLoop(ws *websocket.Conn) {
	code := 1006
	for {
		// 读看门狗：网关每心跳有 op11 ack 回流，60s（2 倍心跳）无任何数据 = 静默断线
		_ = ws.SetReadDeadline(time.Now().Add(2 * time.Duration(c.shared.HeartbeatMs) * time.Millisecond))
		_, msg, err := ws.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				code = ce.Code
			}
			break
		}
		var frame gatewayFrame
		if jerr := json.Unmarshal(msg, &frame); jerr != nil {
			continue
		}
		c.handleFrame(&frame)
	}
	c.handleClose(ws, code)
}

func (c *QQChannel) handleClose(ws *websocket.Conn, code int) {
	c.mu.Lock()
	if c.ws != ws {
		c.mu.Unlock()
		return // stale（已主动关）
	}
	c.ws = nil
	c.stopHeartbeatLocked()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return
	}
	if code != 1000 && code != 1001 {
		c.mu.Lock()
		c.abnormalRecon++
		n := c.abnormalRecon
		c.mu.Unlock()
		c.logf(fmt.Sprintf("网关异常断开（code=%d，连续异常重连 %d 次）", code, n))
	} else {
		c.logf(fmt.Sprintf("网关正常断开（code=%d）", code))
	}
	c.scheduleReconnect("close", false)
}

// 安排一次重连；连续异常 ≥3 升级重认证（reauth_ms + 强制重取 token）
func (c *QQChannel) scheduleReconnect(reason string, forceToken bool) {
	c.mu.Lock()
	if c.stopped || c.reconnectTimer != nil {
		c.mu.Unlock()
		return
	}
	needReauth := forceToken || c.abnormalRecon >= reauthAfter
	delayMs := c.shared.ReconnectMs
	if needReauth {
		delayMs = c.shared.ReauthMs
	}
	if c.abnormalRecon >= reauthAfter {
		c.logf(fmt.Sprintf("连续异常重连 %d ≥ %d → 升级重认证（间隔 %dms，强制重取 token=%v）",
			c.abnormalRecon, reauthAfter, delayMs, forceToken))
	}
	c.reconnectTimer = time.AfterFunc(time.Duration(delayMs)*time.Millisecond, func() {
		c.mu.Lock()
		c.reconnectTimer = nil
		stopped := c.stopped
		c.mu.Unlock()
		if stopped {
			return
		}
		c.logf(fmt.Sprintf("重连中（reason=%s，forceToken=%v）", reason, needReauth))
		_ = c.connect(needReauth)
	})
	c.mu.Unlock()
}

// ---------- 帧处理 ----------

func (c *QQChannel) handleFrame(f *gatewayFrame) {
	// 带 seq 的帧：更新 seq 并回 op11 ack（QQ 官方协议要求）
	if f.S != nil {
		c.mu.Lock()
		c.seq = int(*f.S)
		c.mu.Unlock()
		c.send(map[string]interface{}{"op": 11, "d": map[string]interface{}{"s": *f.S}})
	}
	switch f.Op {
	case 0:
		var d map[string]interface{}
		_ = json.Unmarshal(f.D, &d)
		if code, ok := d["code"].(float64); ok {
			if code == 9009 {
				c.logf("网关事件 9009（token 失效）→ 重取 token 并 reconnect")
				c.closeSocket()
				c.scheduleReconnect("9009", true)
				return
			}
			if code == 9008 {
				c.logf("网关事件 9008（WebSocket 断开）→ reconnect（保 token）")
				c.closeSocket()
				c.scheduleReconnect("9008", false)
				return
			}
		}
		switch f.T {
		case "READY":
			sid := ""
			if d != nil {
				if s, ok := d["session_id"].(string); ok {
					sid = s
				}
			}
			c.mu.Lock()
			c.sessionID = sid
			c.abnormalRecon = 0
			c.mu.Unlock()
			c.startHeartbeat()
			sidShown := sid
			if sidShown == "" {
				sidShown = "?"
			}
			c.logf(fmt.Sprintf("READY：会话建立（session_id=%s，心跳 %dms）", sidShown, c.shared.HeartbeatMs))
			return
		case "RESUMED":
			c.mu.Lock()
			c.abnormalRecon = 0
			c.mu.Unlock()
			c.logf("RESUMED：会话恢复成功")
			return
		case "MESSAGE_CREATE", "AT_MESSAGE_CREATE":
			var m qqMsg
			_ = json.Unmarshal(f.D, &m)
			go c.handleMessage(&m)
			return
		case "C2C_MESSAGE_CREATE":
			var m qqMsg
			_ = json.Unmarshal(f.D, &m)
			go c.handleC2CMessage(&m)
			return
		default:
			return // 其余事件（GUILD_CREATE 等）忽略
		}
	case 1:
		// 服务端要求立即心跳
		c.sendHeartbeat()
		return
	case 7:
		c.logf("op7 RECONNECT：服务端要求重连（保会话，走 resume）")
		c.closeSocket()
		c.scheduleReconnect("op7", false)
		return
	case 9:
		var canResume bool
		_ = json.Unmarshal(f.D, &canResume)
		if !canResume {
			c.mu.Lock()
			c.sessionID = ""
			c.mu.Unlock()
		}
		c.mu.Lock()
		c.abnormalRecon++
		c.mu.Unlock()
		c.logf(fmt.Sprintf("op9 INVALID_SESSION（可恢复=%v）→ reconnect", canResume))
		c.closeSocket()
		c.scheduleReconnect("op9", false)
		return
	default:
		return // op10 HELLO 等：心跳间隔以 cfg.heartbeat_ms 为准
	}
}

func (c *QQChannel) startHeartbeat() {
	c.mu.Lock()
	c.stopHeartbeatLocked()
	stop := make(chan struct{})
	c.heartbeatStop = stop
	ms := c.shared.HeartbeatMs
	c.mu.Unlock()
	tk := time.NewTicker(time.Duration(ms) * time.Millisecond)
	go func() {
		for {
			select {
			case <-tk.C:
				c.sendHeartbeat()
			case <-stop:
				tk.Stop()
				return
			}
		}
	}()
}

func (c *QQChannel) stopHeartbeatLocked() {
	if c.heartbeatStop != nil {
		close(c.heartbeatStop)
		c.heartbeatStop = nil
	}
}

func (c *QQChannel) sendHeartbeat() {
	c.mu.Lock()
	c.z++
	z, seq := c.z, c.seq
	c.mu.Unlock()
	c.send(map[string]interface{}{"op": 1, "d": map[string]interface{}{
		"t": time.Now().UnixMilli(), "s": seq, "z": z,
	}})
}

func (c *QQChannel) send(obj map[string]interface{}) {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return
	}
	b, _ := json.Marshal(obj)
	c.wmu.Lock()
	err := ws.WriteMessage(websocket.TextMessage, b)
	c.wmu.Unlock()
	if err != nil {
		c.logf("WS send 失败：" + err.Error())
	}
}

// ---------- 消息处理 ----------

func (c *QQChannel) handleMessage(m *qqMsg) {
	now := time.Now().UnixMilli()
	// 1. 队列限流（所有上行消息）
	if !c.queueLim.allow(now) {
		c.logf(fmt.Sprintf("队列限流触发（>%d/min），消息丢弃", c.shared.MaxQueuePerMin))
		return
	}
	// 2. agent 限流
	if !c.agentLim.allow(now) {
		c.logf(fmt.Sprintf("agent 限流触发（>%d/min），消息丢弃", c.bot.MaxMsgsPerMin))
		return
	}
	// 3. 滤 bot 自身消息
	if m.Author != nil && m.Author.Bot != nil && *m.Author.Bot {
		return
	}
	chID := ""
	if m.ChannelID != nil {
		chID = *m.ChannelID
	}
	// 4. 渠道过滤
	if c.bot.ChannelID == "" {
		shown := chID
		if shown == "" {
			shown = "?"
		}
		c.logf(fmt.Sprintf("channel_id 未配置，消息不转发（channel=%s）", shown))
		return
	}
	if chID != c.bot.ChannelID {
		return
	}
	// 5. 忽略空内容
	content := ""
	if m.Content != nil {
		content = strings.TrimSpace(*m.Content)
	}
	if content == "" {
		return
	}
	// 6. 截断
	text := content
	if utf16len(content) > c.shared.MaxMsgChars {
		text = jsslice(content, c.shared.MaxMsgChars)
	}
	c.logf(fmt.Sprintf("频道消息收到（channel=%s），转发 agent", chID))
	// 7. 即时 ACK
	if c.shared.AckMessage != "" {
		_ = c.sendReply(c.bot.ChannelID, c.shared.AckMessage)
	}
	// 8'. 流式模式（桥始终提供 onStream）
	if c.onStream != nil {
		if err := c.onStream(text); err != nil {
			c.logf("onStream 异常（不回复）：" + err.Error())
		}
		return
	}
	// 8. agent 一轮（异常只记日志、不回复、不崩）
	reply, err := c.onMessage(text)
	if err != nil {
		c.logf("onMessage 异常（不回复）：" + err.Error())
		return
	}
	// 9. 空回复不发
	replyText := strings.TrimSpace(reply)
	if replyText == "" {
		return
	}
	// 10. 分块回复
	_ = c.sendReply(chID, replyText)
}

// 私聊（C2C）消息处理：与 handleMessage 同构，差异在 sender 抽取与 c2c:<openid> 匹配
func (c *QQChannel) handleC2CMessage(m *qqMsg) {
	now := time.Now().UnixMilli()
	if !c.queueLim.allow(now) {
		c.logf(fmt.Sprintf("队列限流触发（>%d/min），消息丢弃", c.shared.MaxQueuePerMin))
		return
	}
	if !c.agentLim.allow(now) {
		c.logf(fmt.Sprintf("agent 限流触发（>%d/min），消息丢弃", c.bot.MaxMsgsPerMin))
		return
	}
	// 3. sender 抽取（user_openid 优先，逐级回退）
	sender := ""
	if m.Author != nil && m.Author.UserOpenID != nil && *m.Author.UserOpenID != "" {
		sender = *m.Author.UserOpenID
	} else if m.UserOpenID != nil && *m.UserOpenID != "" {
		sender = *m.UserOpenID
	} else if m.Author != nil && m.Author.ID != nil && *m.Author.ID != "" {
		sender = *m.Author.ID
	} else if m.ID != nil && *m.ID != "" {
		sender = *m.ID
	}
	if sender == "" {
		return
	}
	// 4. 渠道过滤
	if c.bot.ChannelID == "" {
		c.logf(fmt.Sprintf("channel_id 未配置，私聊不转发（sender=%s）", sender))
		return
	}
	if c.bot.ChannelID != "c2c:"+sender {
		c.logf(fmt.Sprintf("c2c 渠道未绑定，不转发（sender=%s，绑定=%s）", sender, c.bot.ChannelID))
		return
	}
	// 5. 附件先行（2026-09-15 方向A）：下载 → /inject 文本通知路径（agent 自己去看）；纯附件消息不触发回合
	var atts []*qqAttachment
	for _, a := range m.Attachments {
		if a != nil && a.URL != nil && *a.URL != "" {
			atts = append(atts, a)
		}
	}
	content := ""
	if m.Content != nil {
		content = strings.TrimSpace(*m.Content)
	}
	if content == "" && len(atts) == 0 {
		return
	}
	if len(atts) > 0 && c.onAttachments != nil {
		lines := []string{}
		for _, a := range atts {
			landed, ok := c.downloadAttachment(a)
			if !ok {
				continue
			}
			line := fmt.Sprintf("用户发来的附件已保存到：%s（%s %.1fKB，原文件名 %s）", landed.path, landed.fileType, float64(landed.size)/1024, landed.name)
			if a.ASRReferText != nil && strings.TrimSpace(*a.ASRReferText) != "" {
				line += "\n语音识别文本：" + strings.TrimSpace(*a.ASRReferText)
			}
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			if err := c.onAttachments(strings.Join(lines, "\n")); err != nil {
				c.logf("附件通知 /inject 失败（文件已落 /tmp/qq-inbox）：" + err.Error())
			}
		}
	}
	if content == "" {
		return // 纯附件：不触发回合，等下一条文字消息带上
	}
	// 6. 截断
	text := content
	if utf16len(content) > c.shared.MaxMsgChars {
		text = jsslice(content, c.shared.MaxMsgChars)
	}
	c.logf(fmt.Sprintf("私聊收到（sender=%s），转发 agent", sender))
	// 7. 即时 ACK
	if c.shared.AckMessage != "" {
		_ = c.sendReply("c2c:"+sender, c.shared.AckMessage)
	}
	// 8'. 流式模式
	if c.onStream != nil {
		if err := c.onStream(text); err != nil {
			c.logf("onStream 异常（不回复）：" + err.Error())
		}
		return
	}
	// 8. agent 一轮
	reply, err := c.onMessage(text)
	if err != nil {
		c.logf("onMessage 异常（不回复）：" + err.Error())
		return
	}
	// 9. 空回复不发
	replyText := strings.TrimSpace(reply)
	if replyText == "" {
		return
	}
	// 10. 分块回复
	_ = c.sendReply("c2c:"+sender, replyText)
}

// 2026-09-15 方向A：附件（桥已下载落盘）→ 实例 /inject 路径文本通知，agent 自己去看
func makeOnAttachments(instanceURL, instanceID string) func(string) error {
	return func(note string) error {
		base := strings.TrimRight(instanceURL, "/")
		b, _ := json.Marshal(map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": note}},
			"note":    "QQ附件",
		})
		req, _ := http.NewRequest("POST", base+"/inject", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			var j struct {
				Error *string `json:"error"`
			}
			_ = json.Unmarshal(body, &j)
			detail := "（无错误详情）"
			if j.Error != nil && *j.Error != "" {
				detail = *j.Error
			}
			return fmt.Errorf("实例 %s /inject %d：%s", instanceID, res.StatusCode, detail)
		}
		return nil
	}
}

// 按 ≤3000 字分块 POST；发送前主动剥 URL；连续失败 ≥ send_fail_stop → 停发本轮

func (c *QQChannel) sendReply(channelID, text string) error {
	token0, err := c.ensureToken(false)
	if err != nil {
		return err
	}
	token := token0.value
	// 2026-09-24：原 [File: <路径>] 正文标记自动发文件整块删除（纯副作用：思考段引用路径会
	// 误发，09-24 一图发 3 次）。发文件一律走 outbox 手动投递（见 main 里 outbox watcher）。
	cleanText, hadURL := sanitizeQQText(text)
	if hadURL {
		c.logf("回复含 URL：已剥离后发送（QQ 纯文本消息不允许链接）")
	}
	for _, chunk := range splitText(cleanText, textBubbleChars) {
		ok, err := c.sendChunkWithFallback(channelID, chunk, token)
		if err != nil {
			return err
		}
		if ok == "auth" {
			c.logf("发送 401（token 失效）→ 强制刷新 token 重试本块")
			t2, err := c.ensureToken(true)
			if err != nil {
				return err
			}
			token = t2.value
			ok, err = c.sendChunkWithFallback(channelID, chunk, token)
			if err != nil {
				return err
			}
			if ok == "auth" {
				c.logf("刷新 token 后仍 401（token 服务异常？）→ 本块按发送失败计")
				ok = "fail"
			}
		}
		if ok == "fail" {
			c.mu.Lock()
			c.sendFailStreak++
			n, stopAt := c.sendFailStreak, c.shared.SendFailStop
			c.mu.Unlock()
			c.logf(fmt.Sprintf("回复发送失败（连续 %d/%d）", n, stopAt))
			if n >= stopAt {
				c.logf("连续发送失败达上限，停发本轮（防重试风暴）")
				return nil
			}
			continue
		}
		c.mu.Lock()
		c.sendFailStreak = 0
		c.mu.Unlock()
	}
	return nil
}

// 2026-09-15 方向B：本地文件发 c2c 媒体消息（base64 上传 /v2/users/{openid}/files → msg_type 7 投递）
func (c *QQChannel) nextSeqLocked() int {
	c.mu.Lock()
	c.seq++
	s := c.seq
	c.mu.Unlock()
	return s
}

const maxFileSendBytes = int64(4.5 * 1024 * 1024) // QQ 官方媒体上传上限约 6MB，留余量预检

func (c *QQChannel) sendFileMessage(channelID, filePath, token string) bool {
	openid := strings.TrimPrefix(channelID, "c2c:")
	name := filepath.Base(filePath)
	fileType := qqFileTypeByExt(name)
	data, err := os.ReadFile(filePath)
	if err != nil {
		c.logf("文件读取失败（跳过）：" + name + " " + err.Error())
		return false
	}
	// QQ 官方媒体上传上限约 6MB，留余量预检 4.5MB（2026-09-24 从已删的正文标记路径移入）
	if int64(len(data)) > maxFileSendBytes {
		c.logf(fmt.Sprintf("文件超 4.5MB 上限未发（实际 %.1fMB）：%s", float64(len(data))/1048576, name))
		return false
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	upBody, _ := json.Marshal(map[string]interface{}{
		"file_type":    fileType,
		"srv_send_msg": false,
		"file_data":    b64,
		"file_name":    name,
	})
	upReq, _ := http.NewRequest("POST", "https://api.sgroup.qq.com/v2/users/"+openid+"/files", bytes.NewReader(upBody))
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "QQBot "+token)
	upRes, err := (&http.Client{Timeout: 60 * time.Second}).Do(upReq)
	if err != nil {
		c.logf("文件上传异常：" + name + " " + err.Error())
		return false
	}
	upRespBody, _ := io.ReadAll(upRes.Body)
	upRes.Body.Close()
	if upRes.StatusCode < 200 || upRes.StatusCode >= 300 {
		c.logf(fmt.Sprintf("文件上传失败（HTTP %d）：%s %s", upRes.StatusCode, name, jsslice(string(upRespBody), 200)))
		return false
	}
	var upJson struct {
		FileInfo string `json:"file_info"` // 2026-09-24 修：QQ 返回的是 base64 字符串，不是对象（原 map 解析恒 nil → “缺 file_info”）
	}
	if json.Unmarshal(upRespBody, &upJson) != nil || upJson.FileInfo == "" {
		c.logf("文件上传返回缺 file_info：" + name)
		return false
	}
	msgBody, _ := json.Marshal(map[string]interface{}{
		"msg_type": 7,
		"media":    map[string]interface{}{"file_info": upJson.FileInfo},
		"msg_seq":  nextC2CSeq(), // 2026-09-24 修：原用 WS 会话 seq（c.seq），与 c2c 文本消息的 c2cMsgSeq 不同源，回退会被 QQ 拒
		"content":  name,
	})
	msgReq, _ := http.NewRequest("POST", "https://api.sgroup.qq.com/v2/users/"+openid+"/messages", bytes.NewReader(msgBody))
	msgReq.Header.Set("Content-Type", "application/json")
	msgReq.Header.Set("Authorization", "QQBot "+token)
	msgRes, err := (&http.Client{Timeout: 15 * time.Second}).Do(msgReq)
	if err != nil {
		c.logf("文件消息发送异常：" + name + " " + err.Error())
		return false
	}
	msgRespBody, _ := io.ReadAll(msgRes.Body)
	msgRes.Body.Close()
	if msgRes.StatusCode < 200 || msgRes.StatusCode >= 300 {
		c.logf(fmt.Sprintf("文件消息发送失败（HTTP %d）：%s %s", msgRes.StatusCode, name, jsslice(string(msgRespBody), 200)))
		return false
	}
	c.logf(fmt.Sprintf("文件已发：%s（type=%d，%dKB）", name, fileType, len(data)/1024))
	return true
}

// qqFileTypeByExt QQ 媒体类型：1 图片 / 2 视频 / 3 音频 / 4 文件
func qqFileTypeByExt(name string) int {
	ext := ""
	if i := strings.LastIndex(name, "."); i >= 0 && i < len(name)-1 {
		ext = strings.ToLower(name[i+1:])
	}
	switch ext {
	case "png", "jpg", "jpeg", "webp", "gif", "bmp":
		return 1
	case "mp4", "mov", "avi", "mkv", "flv", "webm", "m4v":
		return 2
	case "mp3", "wav", "m4a", "ogg", "flac":
		return 3
	}
	return 4
}

// 2026-09-15 方向A：下载 QQ 附件 → /tmp/qq-inbox/<ts36>-<安全名>（30s 超时；失败返回 false + 日志，不打断主流程）
var unsafeNameRe = regexp.MustCompile(`[^0-9A-Za-z._\-一-龥]`)

type qqLandedFile struct {
	path     string
	name     string
	fileType string
	size     int
}

func (c *QQChannel) downloadAttachment(a *qqAttachment) (qqLandedFile, bool) {
	safe := "qqfile"
	if a.Filename != nil && *a.Filename != "" {
		r := []rune(unsafeNameRe.ReplaceAllString(*a.Filename, "_"))
		if len(r) > 64 {
			r = r[:64]
		}
		safe = string(r)
		if safe == "" {
			safe = "qqfile"
		}
	}
	dir := "/tmp/qq-inbox"
	path := filepath.Join(dir, ts36(time.Now().UnixMilli())+"-"+safe)
	name := safe
	if a.Filename != nil && *a.Filename != "" {
		name = *a.Filename
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", *a.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.logf("附件下载异常（跳过）：" + err.Error())
		return qqLandedFile{}, false
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		c.logf(fmt.Sprintf("附件下载失败 HTTP %d（原文件名 %s），跳过", res.StatusCode, name))
		return qqLandedFile{}, false
	}
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		c.logf("附件读取异常（跳过）：" + err.Error())
		return qqLandedFile{}, false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.logf("附件目录创建失败：" + err.Error())
		return qqLandedFile{}, false
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		c.logf("附件落盘失败：" + err.Error())
		return qqLandedFile{}, false
	}
	fileType := "file"
	if a.ContentType != nil && *a.ContentType != "" {
		fileType = *a.ContentType
	} else if i := strings.LastIndex(safe, "."); i >= 0 && len(safe)-i-1 <= 8 {
		fileType = strings.ToLower(safe[i+1:])
	}
	c.logf(fmt.Sprintf("附件已落：%s（%d 字节，原文件名 %s）", path, len(buf), name))
	return qqLandedFile{path: path, name: name, fileType: fileType, size: len(buf)}, true
}

// ts36 十进制整数 → base36（对齐 TS Date.now().toString(36)）
func ts36(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{digits[n%36]}, out...)
		n /= 36
	}
	return string(out)
}

// 发一个分块；被 QQ 按 URL 内容拒收（304003/40034028）时激进剥裸域名重试一次
// 返回 {"ok"|"auth"|"fail", err}
func (c *QQChannel) sendChunkWithFallback(channelID, chunk, token string) (string, error) {
	aggressive, _ := aggressiveSanitizeQQText(chunk)
	attempts := []string{chunk}
	if aggressive != chunk {
		attempts = append(attempts, aggressive)
	}
	isC2C := strings.HasPrefix(channelID, "c2c:")
	useMd := c.shared.MarkdownEnabled
	for _, text := range attempts {
		for mdTry := 0; mdTry < 2; mdTry++ {
			var sendURL string
			if isC2C {
				sendURL = "https://api.sgroup.qq.com/v2/users/" + channelID[len("c2c:"):] + "/messages"
			} else {
				sendURL = "https://api.sgroup.qq.com/channels/" + channelID + "/messages"
			}
			// markdown 消息：{"markdown":{"content"}}；c2c 需 msg_type（md=2/纯文本=0）+ msg_seq
			body := map[string]interface{}{}
			if useMd {
				body["markdown"] = map[string]string{"content": text}
			} else {
				body["content"] = text
			}
			if isC2C {
				if useMd {
					body["msg_type"] = 2
				} else {
					body["msg_type"] = 0
				}
				body["msg_seq"] = nextC2CSeq()
			}
			b, _ := json.Marshal(body)
			req, _ := http.NewRequest("POST", sendURL, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "QQBot "+token)
			client := &http.Client{Timeout: 15 * time.Second} // QQ API 偶发挂起，无超时会卡死转发循环
			res, err := client.Do(req)
			if err != nil {
				c.logf("回复发送失败：" + err.Error())
				return "fail", nil
			}
			kind := "guild"
			if isC2C {
				kind = "c2c"
			}
			c.logf(fmt.Sprintf("[DEBUG] POST %s md=%v seq=%d len=%d :: %s -> HTTP %d",
				kind, useMd, c2cSeqSnapshot(), utf16len(text), jsslice(text, 80), res.StatusCode))
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				if text != chunk {
					c.logf("回复分块经激进 URL 剥离后发送成功")
				}
				res.Body.Close()
				return "ok", nil
			}
			if res.StatusCode == 401 {
				res.Body.Close()
				return "auth", nil // token 失效：交 sendReply 刷新重试
			}
			respBody, _ := io.ReadAll(res.Body)
			res.Body.Close()
			c.logf(fmt.Sprintf("[DEBUG] 非2xx 响应体（%d）：%s", res.StatusCode, jsslice(string(respBody), 300)))
			payloadText := strings.ToLower(string(respBody))
			if useMd && res.StatusCode >= 400 && res.StatusCode < 500 && isMarkdownRejectPayload(payloadText) {
				useMd = false // markdown 校验失败 → 降级纯文本重试同一块
				continue
			}
			if text == chunk && len(attempts) > 1 && isUrlContentError(string(respBody)) {
				c.logf(fmt.Sprintf("回复被 QQ 拒收（URL 内容，HTTP %d）→ 激进剥离裸域名重试", res.StatusCode))
				break
			}
			extra := ""
			if len(respBody) > 0 {
				extra = "：" + jsslice(string(respBody), 200)
			}
			c.logf(fmt.Sprintf("回复发送失败（HTTP %d）%s", res.StatusCode, extra))
			return "fail", nil
		}
	}
	return "fail", nil
}

// ---------- SSE 帧解析 ----------

// 从累积 buffer 里切出完整 SSE 帧（\n\n 分隔）→ [剩余buffer, 帧列表]
func takeSSEFrames(buf string) (string, []string) {
	var frames []string
	for {
		idx := strings.Index(buf, "\n\n")
		if idx < 0 {
			break
		}
		frames = append(frames, buf[:idx])
		buf = buf[idx+2:]
	}
	return buf, frames
}

// 解析单个帧 → { event, data }（data 为 JSON 解析结果，失败保留原文）
func parseSSEFrame(frame string) (string, interface{}) {
	event := ""
	var data interface{}
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "event: ") {
			event = line[7:]
		} else if strings.HasPrefix(line, "data: ") {
			payload := line[6:]
			var v interface{}
			if err := json.Unmarshal([]byte(payload), &v); err == nil {
				data = v
			} else {
				data = payload
			}
		}
		// 其余行（id:/retry:/注释 ": ping"）忽略
	}
	return event, data
}

// ---------- 事件 → QQ 消息映射 ----------

type EventForwarder struct {
	thinkBuf    string
	textBuf     string
	textEmitted bool
}

// 喂一个 SSE 事件帧，返回要发的 QQ 消息列表（0..n 条）
func (f *EventForwarder) update(eventType string, data interface{}) []string {
	e, _ := data.(map[string]interface{})
	if e == nil {
		e = map[string]interface{}{}
	}
	switch eventType {
	case "delta":
		out := []string{}
		if s, ok := e["thinking"].(string); ok && s != "" {
			f.thinkBuf += s
		}
		if s, ok := e["text"].(string); ok && s != "" {
			// 正文开始 = thinking 结束边界：先把整段思考拆气泡发出
			if f.thinkBuf != "" {
				t := f.thinkBuf
				f.thinkBuf = ""
				for _, part := range splitText(t, thinkBubbleChars) {
					out = append(out, "[思考] "+part)
				}
			}
			f.textBuf += s
		}
		return out
	case "tool_call":
		out := f.flushAll()
		args := ""
		if e["args"] != nil {
			if b, err := json.Marshal(e["args"]); err == nil {
				args = string(b)
			}
		}
		name, _ := e["name"].(string)
		if name == "" {
			name = "?"
		}
		out = append(out, strings.TrimSpace("[工具] "+name+" "+jsslice(args, 300)))
		return out
	case "tool_result":
		out := f.flushAll()
		name, _ := e["name"].(string)
		if name == "" {
			name = "?"
		}
		content, _ := e["content"].(string)
		if content != "" {
			out = append(out, "[结果] "+name+"："+jsslice(content, 300))
		} else {
			out = append(out, "[结果] "+name)
		}
		return out
	case "finish":
		out := f.flushAll()
		// 全程无 delta（非流式兜底）：发 finish.reply
		if !f.textEmitted {
			if r, ok := e["reply"].(string); ok && strings.TrimSpace(r) != "" {
				out = append(out, strings.TrimSpace(r))
			}
		}
		return out
	case "error":
		// 出错前先把已攒的尾巴发出去
		out := f.flushAll()
		msg, _ := e["message"].(string)
		if msg == "" {
			msg = "未知错误"
		}
		out = append(out, "[出错] "+msg)
		return out
	case "stopped":
		// /stop 收尾发的是 stopped（非 finish）——同样 flush 尾巴
		return f.flushAll()
	default:
		return nil // run_start/run_msg/done/run_end/hello/bg_piggyback 等不转发
	}
}

// 回合收尾后重置 buffer（每个回合一份）
func (f *EventForwarder) reset() {
	f.thinkBuf = ""
	f.textBuf = ""
	f.textEmitted = false
}

func (f *EventForwarder) flushAll() []string {
	out := []string{}
	if f.thinkBuf != "" {
		for _, part := range splitText(f.thinkBuf, thinkBubbleChars) {
			out = append(out, "[思考] "+part)
		}
		f.thinkBuf = ""
	}
	if f.textBuf != "" {
		for _, part := range splitText(f.textBuf, textBubbleChars) {
			out = append(out, part)
		}
		f.textBuf = ""
		f.textEmitted = true
	}
	return out
}

// run_end 安全网：正常 finish/stopped 已 flush 时返回空
func (f *EventForwarder) tailFlush() []string {
	return f.flushAll()
}

// ---------- /events 常驻下行转发循环 ----------

type eventsLoop struct {
	url    string
	ch     *QQChannel
	target string
	botID  string
	logf   func(string)
	stopping *atomic.Bool

	fwd EventForwarder

	mu              sync.Mutex
	lastSendAt      int64
	roundActive     bool
	delivered       bool
	lastEventAt     int64
	roundStartAt    int64
	historyBaseline int
	stuckStop       chan struct{}
}

func (el *eventsLoop) setStuckTimer(on bool) {
	el.mu.Lock()
	defer el.mu.Unlock()
	if on && el.stuckStop == nil {
		stop := make(chan struct{})
		el.stuckStop = stop
		go el.stuckLoop(stop)
	} else if !on && el.stuckStop != nil {
		close(el.stuckStop)
		el.stuckStop = nil
	}
}

func (el *eventsLoop) stuckLoop(stop chan struct{}) {
	tk := time.NewTicker(time.Duration(stuckPollMs) * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			el.mu.Lock()
			active, delivered := el.roundActive, el.delivered
			lastEvent, roundStart := el.lastEventAt, el.roundStartAt
			el.mu.Unlock()
			if !active || delivered {
				return
			}
			now := time.Now().UnixMilli()
			if now-lastEvent <= stuckSilenceMs {
				continue
			}
			if now-roundStart > stuckMaxMs {
				el.logf(fmt.Sprintf("静默兜底放弃（轮内 %ds 无收尾）", (now-roundStart)/1000))
				el.setStuckTimer(false)
				return
			}
			if el.deliverLostTail() {
				el.mu.Lock()
				el.delivered = true
				el.roundActive = false
				el.mu.Unlock()
				el.setStuckTimer(false)
			}
		}
	}
}

// 静默兜底：查 /history，若本轮已收尾但桥没收齐收尾帧，把最终回复补发给 QQ
func (el *eventsLoop) deliverLostTail() bool {
	base := strings.TrimRight(el.url, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Get(base + "/history")
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false
	}
	var j struct {
		Entries []struct {
			Role      string        `json:"role"`
			Content   *string       `json:"content"`
			ToolCalls []interface{} `json:"tool_calls"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		return false
	}
	// 无 tool_calls 的 assistant = 轮次最终回复（工具循环中途的 assistant 带 tool_calls）
	plain := 0
	lastContent := ""
	for i := range j.Entries {
		e := j.Entries[i]
		if e.Role == "assistant" && len(e.ToolCalls) == 0 {
			plain++
			if e.Content != nil {
				lastContent = *e.Content
			}
		}
	}
	el.mu.Lock()
	baseline := el.historyBaseline
	el.mu.Unlock()
	if plain <= baseline {
		return false // 本轮尚未收尾（LLM 真慢）
	}
	if strings.TrimSpace(lastContent) == "" {
		return true // 空回复（如强停轮）：视为已交付
	}
	for _, part := range splitText(lastContent, 3000) {
		el.sendWithGap(part, fmt.Sprintf("兜底补发失败（bot=%s）", el.ch.botID))
	}
	preview := strings.ReplaceAll(jsslice(lastContent, 80), "\n", " ")
	el.logf(fmt.Sprintf("静默兜底：已补发本轮回尾（%d 字，前 80：%s）", utf16len(lastContent), preview))
	return true
}

func countPlainAssistants(base string) int {
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Get(base + "/history")
	if err != nil {
		return 0
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return 0
	}
	var j struct {
		Entries []struct {
			Role      string        `json:"role"`
			ToolCalls []interface{} `json:"tool_calls"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		return 0
	}
	n := 0
	for _, e := range j.Entries {
		if e.Role == "assistant" && len(e.ToolCalls) == 0 {
			n++
		}
	}
	return n
}

// 相邻消息至少隔 minSendGapMs（思考块本身 3~4s 一条，基本无感）
func (el *eventsLoop) sendWithGap(msg, failLabel string) {
	el.mu.Lock()
	gapWait := el.lastSendAt + minSendGapMs - time.Now().UnixMilli()
	el.mu.Unlock()
	if gapWait > 0 {
		time.Sleep(time.Duration(gapWait) * time.Millisecond)
	}
	if err := el.ch.sendTo(el.target, msg); err != nil {
		el.logf(failLabel + "：" + err.Error())
	}
	el.mu.Lock()
	el.lastSendAt = time.Now().UnixMilli()
	el.mu.Unlock()
}

// 25s 读看门狗：一次 read 与 25s 超时赛跑
func readChunkWithTimeout(r io.Reader, d time.Duration) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 16384)
		n, err := r.Read(buf)
		ch <- result{buf[:n], err}
	}()
	select {
	case res := <-ch:
		return res.b, res.err
	case <-time.After(d):
		return nil, fmt.Errorf("25s 无数据（疑似静默断线）")
	}
}

func (el *eventsLoop) subscribeOnce(traceCount *int, traceStart *time.Time) error {
	base := strings.TrimRight(el.url, "/")
	req, _ := http.NewRequest("GET", base+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{} // 长连接，不设超时
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	el.logf(fmt.Sprintf("已订阅 /events（bot=%s target=%s）", el.botID, el.target))
	buf := ""
	for {
		data, rerr := readChunkWithTimeout(res.Body, sseReadWatchdog)
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil // 服务端正常关流 → 立即重订（不记断线日志）
			}
			return rerr
		}
		if el.stopping.Load() {
			return nil
		}
		buf += string(data)
		rest, frames := takeSSEFrames(buf)
		buf = rest
		*traceCount += len(frames)
		now := time.Now()
		if now.Sub(*traceStart) >= 30*time.Second {
			el.logf(fmt.Sprintf("[TRACE] 30s 收到帧 %d 个，buf 残留 %d 字符", *traceCount, utf16len(buf)))
			*traceCount = 0
			*traceStart = now
		}
		for _, frame := range frames {
			event, data := parseSSEFrame(frame)
			if event == "run_start" || event == "hello" {
				el.fwd.reset()
				if event == "run_start" {
					el.mu.Lock()
					el.roundActive = true
					el.delivered = false
					el.roundStartAt = time.Now().UnixMilli()
					el.historyBaseline = countPlainAssistants(base) // 持锁：stuckLoop 并发读
					el.mu.Unlock()
					el.setStuckTimer(true)
				}
			}
			if event != "" {
				el.mu.Lock()
				el.lastEventAt = time.Now().UnixMilli() // 心跳（event=""）不算事件帧
				if event == "finish" || event == "stopped" {
					el.delivered = true
				}
				delivered := el.delivered
				el.mu.Unlock()
				if event == "run_end" {
					el.mu.Lock()
					el.roundActive = false
					el.mu.Unlock()
					el.setStuckTimer(false)
					// 安全网：run_end 到了但收尾没 flush → 把尾巴补发
					if !delivered {
						for _, msg := range el.fwd.tailFlush() {
							el.sendWithGap(msg, fmt.Sprintf("run_end 补发失败（bot=%s）", el.botID))
						}
					}
				}
			}
			for _, msg := range el.fwd.update(event, data) {
				el.sendWithGap(msg, fmt.Sprintf("转发失败（bot=%s %s）", el.botID, event))
			}
		}
	}
}

func (el *eventsLoop) run() {
	traceCount := 0
	traceStart := time.Now()
	for {
		if el.stopping.Load() {
			return
		}
		if err := el.subscribeOnce(&traceCount, &traceStart); err != nil {
			if el.stopping.Load() {
				return
			}
			el.logf(fmt.Sprintf("/events 断线（bot=%s %v），2s 后重连", el.botID, err))
			time.Sleep(eventsReconnect)
		} else if !el.stopping.Load() {
			// 干净关闭（服务端先断，如 200 后立即 EOF）：同样短睡，避免紧循环重连捶打服务端
			el.logf(fmt.Sprintf("/events 被服务端关闭（bot=%s），2s 后重连", el.botID))
			time.Sleep(eventsReconnect)
		}
	}
}

// ---------- 上行转发 ----------

// QQ 消息 → 该 bot 绑定实例的 /enqueue（入队即返；事件走 /events 下行）
func makeOnStream(instanceURL, instanceID string, logf func(string)) func(string) error {
	return func(text string) error {
		base := strings.TrimRight(instanceURL, "/")
		b, _ := json.Marshal(map[string]string{"message": text})
		req, _ := http.NewRequest("POST", base+"/enqueue", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 15 * time.Second}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		var j struct {
			Queued   *bool   `json:"queued"`
			Position *int    `json:"position"`
			Error    *string `json:"error"`
		}
		_ = json.Unmarshal(body, &j)
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			detail := "（无错误详情）"
			if j.Error != nil && *j.Error != "" {
				detail = *j.Error
			}
			return fmt.Errorf("实例 %s /enqueue %d：%s", instanceID, res.StatusCode, detail)
		}
		q := "?"
		if j.Queued != nil {
			q = strconv.FormatBool(*j.Queued)
		}
		p := "?"
		if j.Position != nil {
			p = strconv.Itoa(*j.Position)
		}
		logf(fmt.Sprintf("已入队 实例=%s（queued=%s pos=%s）：%s", instanceID, q, p, jsslice(text, 60)))
		return nil
	}
}

// 兜底（无 /events 场景/旧实例）：同步 /chat
func makeOnMessage(instanceURL, instanceID string) func(string) (string, error) {
	return func(text string) (string, error) {
		base := strings.TrimRight(instanceURL, "/")
		b, _ := json.Marshal(map[string]string{"message": text})
		req, _ := http.NewRequest("POST", base+"/chat", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 120 * time.Second}
		res, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			var j struct {
				Error *string `json:"error"`
			}
			_ = json.Unmarshal(body, &j)
			detail := "（无错误详情）"
			if j.Error != nil && *j.Error != "" {
				detail = *j.Error
			}
			return "", fmt.Errorf("实例 %s /chat %d：%s", instanceID, res.StatusCode, detail)
		}
		var j struct {
			Reply *string `json:"reply"`
		}
		_ = json.Unmarshal(body, &j)
		if j.Reply == nil {
			return "", fmt.Errorf("实例 %s /chat 返回缺 reply 字段", instanceID)
		}
		return *j.Reply, nil
	}
}

// ---------- 启动 ----------

func main() {
	root := pluginRoot()
	cfg, err := loadConfig(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	if !cfg.Shared.Enabled {
		fmt.Println("[qq-bridge] shared.enabled=false，桥不启动（配置 enabled 为 true 启用）")
		os.Exit(0)
	}
	// 单实例锁——防双桥（双桥会双份转发/双份入队）
	lockPath := filepath.Join(cfg.Shared.DataDir, "channels", "qq", "bridge.lock")
	os.MkdirAll(filepath.Dir(lockPath), 0755)
	acquireLock := func() bool {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			_, werr := f.WriteString(strconv.Itoa(os.Getpid()))
			f.Close()
			if werr != nil {
				fmt.Fprintf(os.Stderr, "[qq-bridge] 桥锁写入失败（不阻塞启动）：%v\n", werr)
			}
			return true
		}
		// 锁已存在：旧进程存活 → 退出；陈旧锁 → 清掉重试
		if data, rerr := os.ReadFile(lockPath); rerr == nil {
			if oldPid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && oldPid > 0 {
				if _, serr := os.Stat(fmt.Sprintf("/proc/%d", oldPid)); serr == nil {
					fmt.Fprintf(os.Stderr, "[qq-bridge] 已有桥在运行（PID %d），本进程退出\n", oldPid)
					os.Exit(1)
				}
				fmt.Fprintf(os.Stderr, "[qq-bridge] 检测到陈旧锁（PID %d 已不在），清除\n", oldPid)
			}
		}
		os.Remove(lockPath)
		return false
	}
	if !acquireLock() {
		acquireLock() // O_EXCL 抢占，防两进程同时判陈旧而双启
	}
	validBots := []BotCfg{}
	for _, b := range cfg.Bots {
		if b.AppID != "" && b.ClientSecret != "" {
			validBots = append(validBots, b)
		}
	}
	if len(validBots) == 0 {
		fmt.Fprintln(os.Stderr, "config.json bots[] 无有效 bot（每个需 app_id + client_secret，可选 channel_id / instance / max_msgs_per_min）")
		os.Exit(1)
	}
	// c2c msg_seq 从盘上恢复（QQ 要求单调递增）
	c2cSeqFile = filepath.Join(cfg.Shared.DataDir, "channels", "qq", "msg_seq")
	if data, err := os.ReadFile(c2cSeqFile); err == nil {
		if n, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && n > 0 {
			c2cMsgSeq = n
		}
	}

	logf := func(m string) {
		// TS 版 new Date().toISOString().slice(11,19) = UTC 时
		fmt.Printf("[%s] [qq-bridge] %s\n", time.Now().UTC().Format("15:04:05"), m)
	}

	urlOfInstance := func(instanceID string) string {
		for _, i := range cfg.Instances {
			if i.ID == instanceID {
				return i.URL
			}
		}
		return cfg.Instances[0].URL
	}

	stopping := &atomic.Bool{}
	var started []*QQChannel
	for _, bot := range validBots {
		instanceID := bot.Instance
		if instanceID == "" {
			instanceID = "default"
		}
		instURL := urlOfInstance(instanceID)
		botID := bot.AppID
		ch := NewQQChannel(cfg.Shared, bot, cfg.Shared.DataDir,
			makeOnStream(instURL, instanceID, logf),
			makeOnMessage(instURL, instanceID),
			makeOnAttachments(instURL, instanceID),
			func(m string) { logf("[bot=" + botID + "] " + m) })
		started = append(started, ch)
		go func(ch *QQChannel, instanceID, instURL string) {
			if err := ch.start(); err != nil {
				logf(fmt.Sprintf("启动失败（bot=%s）：%v", ch.botID, err))
			} else {
				logf(fmt.Sprintf("已连接（bot=%s → 实例=%s %s）", ch.botID, instanceID, instURL))
			}
		}(ch, instanceID, instURL)
		// 下行：bot 配了转发目标才起 /events 循环
		target := strings.TrimSpace(bot.ChannelID)
		if target != "" {
			go (&eventsLoop{url: instURL, ch: ch, target: target, botID: botID, logf: logf, stopping: stopping}).run()
		} else {
			logf(fmt.Sprintf("bot=%s 未配 channel_id，/events 下行循环不启动（上行仍入队）", botID))
		}
	}
	if len(started) == 0 {
		logf("无有效 bot，退出")
		os.Exit(1)
	}
	ids := make([]string, len(cfg.Instances))
	for i, inst := range cfg.Instances {
		ids[i] = inst.ID
	}
	logf(fmt.Sprintf("桥已启动：%d 个 bot → %d 个实例（%s；流式下行 /events；token 缓存 <%s/channels/qq/tokens/>)",
		len(started), len(cfg.Instances), strings.Join(ids, ","), cfg.Shared.DataDir))

	// 2026-09-24 outbox 手动投递：cp <文件> <DataDir>/channels/qq/outbox/<botAppID>/ = 向该 bot 的 c2c
	// 渠道明确发一条媒体消息（不扫聊天文本，谁放进来谁负责）。发送成功 → 归档 sent/；失败 → 留在原位下轮重试。
	outboxRoot := filepath.Join(cfg.Shared.DataDir, "channels", "qq", "outbox")
	os.MkdirAll(outboxRoot, 0755)
	for _, ch := range started {
		if !strings.HasPrefix(ch.bot.ChannelID, "c2c:") {
			continue
		}
		dir := filepath.Join(outboxRoot, ch.botID)
		os.MkdirAll(filepath.Join(dir, "sent"), 0755)
		go func(ch *QQChannel, dir, botID string) {
			tk := time.NewTicker(2 * time.Second)
			defer tk.Stop()
			for range tk.C {
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if e.IsDir() || !e.Type().IsRegular() {
						continue
					}
					p := filepath.Join(dir, e.Name())
					token, terr := ch.ensureToken(false)
					if terr != nil {
						logf(fmt.Sprintf("[bot=%s] outbox 暂缓（token 不可用，下轮重试）：%v", botID, terr))
						break
					}
					if ch.sendFileMessage(ch.bot.ChannelID, p, token.value) {
						dest := filepath.Join(dir, "sent", fmt.Sprintf("%s.%d", e.Name(), time.Now().Unix()))
						if merr := os.Rename(p, dest); merr != nil {
							logf(fmt.Sprintf("[bot=%s] outbox 归档失败（文件已发）：%v", botID, merr))
						} else {
							logf(fmt.Sprintf("[bot=%s] outbox 投递完成：%s", botID, e.Name()))
						}
					}
				}
			}
		}(ch, dir, ch.botID)
	}
	logf(fmt.Sprintf("outbox 就绪：cp <文件> %s/<botAppID>/ = 一次手动媒体投递（成功归档 sent/）", outboxRoot))

	// 常驻：QQChannel 内部自重连/重认证；SIGINT 优雅收尾
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	for s := range sigCh {
		if s == syscall.SIGTERM {
			os.Exit(0)
		}
		if stopping.Swap(true) {
			continue
		}
		logf("收到 SIGINT，关闭所有 WS…")
		for _, ch := range started {
			ch.stop()
		}
		os.Exit(0)
	}
}
