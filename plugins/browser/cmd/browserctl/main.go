// OkHuman_p/browser — 浏览器动作 CLI（browserctl）
//
// agent 经 bash 调用，每次是一个短连接（连 CDP → 执行动作 → 退出）。
// 每个动作都会刷新 browserd 的心跳文件（让守护进程知道实例还在被使用）。
//
// TS 版 browserctl.ts 的 1:1 Go 移植（命令集/输出文案/退出码逐字对齐：
// 0 成功；2 实例未起（withInstance 文件检查）；3 其余一切抛出错误含用法错——
// 与 TS 实现一致：main().catch 一律 exit 3）。
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var agentID = func() string {
	raw := os.Getenv("OKH_BROWSER_AGENT")
	if regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(raw) {
		return raw
	}
	return ""
}()

func activeTabFile(port int) string {
	if agentID != "" {
		return fmt.Sprintf("/tmp/okhuman-browser-%d.active-%s", port, agentID)
	}
	return fmt.Sprintf("/tmp/okhuman-browser-%d.active", port)
}

func tabOrderFile(port int) string {
	return fmt.Sprintf("/tmp/okhuman-browser-%d.taborder", port)
}

func hbFileOf(port int) string    { return fmt.Sprintf("/tmp/okhuman-browser-%d.hb", port) }
func stateFileOf(port int) string { return fmt.Sprintf("/tmp/okhuman-browser-%d.state", port) }
func stopFileOf(port int) string  { return fmt.Sprintf("/tmp/okhuman-browser-%d.stop", port) }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

var httpc = &http.Client{Timeout: 60 * time.Second}

// ---------- JS 数值/字符串打印对齐 ----------

func jsNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// jsFixed 模拟 JS toFixed：四舍五入（half away from zero），固定位数
func jsFixed(v float64, digits int) string {
	mult := math.Pow(10, float64(digits))
	r := math.Round(v*mult) / mult
	return strconv.FormatFloat(r, 'f', digits, 64)
}

// jsString 模拟 JS JSON.stringify(string)：双引号 + 转义，不转义 HTML 字符
func jsString(s string) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func jsQuote(s string) string { return jsString(s) }

// ---------- page target 管理 ----------

type pageInfo struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	WsURL   string `json:"webSocketDebuggerUrl"`
}

func getJSON(url string, v interface{}) error {
	r, err := httpc.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", r.StatusCode)
	}
	return json.NewDecoder(r.Body).Decode(v)
}

// listPages 取所有 page target。/json/list 的枚举顺序不可靠——index 一律用 orderedPages。
func listPages(port int) ([]pageInfo, error) {
	var list []pageInfo
	if err := getJSON(fmt.Sprintf("http://127.0.0.1:%d/json/list", port), &list); err != nil {
		return nil, err
	}
	out := []pageInfo{}
	for _, p := range list {
		if p.Type == "page" {
			out = append(out, p)
		}
	}
	return out, nil
}

// orderedPages index 按持久化顺序：targetId 顺序存 taborder 文件——既有 tab 位置不变，
// 新开 tab 追加末尾，关闭的剔除。index 不受 /json/list 枚举顺序漂移影响。
func orderedPages(port int) ([]pageInfo, error) {
	pages, err := listPages(port)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	byID := map[string]pageInfo{}
	for _, p := range pages {
		live[p.ID] = true
		byID[p.ID] = p
	}
	var order []string
	if b, err := os.ReadFile(tabOrderFile(port)); err == nil {
		json.Unmarshal(b, &order)
	}
	var final []string
	for _, id := range order {
		if live[id] {
			final = append(final, id)
		}
	}
	for _, p := range pages {
		keep := false
		for _, id := range final {
			if id == p.ID {
				keep = true
				break
			}
		}
		if !keep {
			final = append(final, p.ID)
		}
	}
	b, _ := json.Marshal(final)
	os.WriteFile(tabOrderFile(port), b, 0644)
	out := make([]pageInfo, 0, len(final))
	for _, id := range final {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// activeTargetId 解析活跃 tab：优先状态文件里的 targetId；失效/不存在则回退第一个 page。
func activeTargetId(port int) (string, error) {
	id := ""
	if b, err := os.ReadFile(activeTabFile(port)); err == nil {
		id = strings.TrimSpace(string(b))
	}
	pages, err := orderedPages(port)
	if err != nil {
		return "", err
	}
	if len(pages) == 0 {
		return "", errors.New("无 page target")
	}
	if id != "" {
		for _, p := range pages {
			if p.ID == id {
				return p.ID, nil
			}
		}
		// 活跃 tab 已被关（如外部关）→ 落到第一个
	}
	return pages[0].ID, nil
}

func setActiveTab(port int, targetID string) {
	os.WriteFile(activeTabFile(port), []byte(targetID), 0644)
}

func clearActiveTab(port int) {
	os.Remove(activeTabFile(port))
}

// newTab 开新 tab（新版 chromium 的 /json/new 只认 PUT）
func newTab(port int, url string) (pageInfo, error) {
	var t pageInfo
	err := doMethod("PUT", fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", port, httpQueryEscape(url)), &t)
	if err != nil {
		var he *httpErr
		if errors.As(err, &he) {
			return t, fmt.Errorf("开新 tab 失败: HTTP %d", he.Code)
		}
		return t, fmt.Errorf("开新 tab 失败: %v", err)
	}
	return t, nil
}

func closeTab(port int, targetID string) error {
	err := doMethod("GET", fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", port, targetID), nil)
	if err != nil {
		var he *httpErr
		if errors.As(err, &he) {
			return fmt.Errorf("关 tab 失败: HTTP %d", he.Code)
		}
		return fmt.Errorf("关 tab 失败: %v", err)
	}
	return nil
}

type httpErr struct {
	Code int
}

func (e *httpErr) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

func doMethod(method, url string, v interface{}) error {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return err
	}
	r, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return &httpErr{Code: r.StatusCode}
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(v)
}

func httpQueryEscape(s string) string {
	// 对齐 JS encodeURIComponent（不编码 A-Za-z0-9 - _ . ! ~ * ' ( )）
	// 必须逐字节转义：按 rune 转只留首字节，多字节 UTF-8 的续节会被丢（中文 URL 变坏）
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' || c == '\'' || c == '(' || c == ')' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ---------- CDP 短连接 ----------

type cdpMsg struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Err    *cdpError       `json:"error"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cdpClient struct {
	conn    *websocket.Conn
	sendMu  sync.Mutex
	idc     int
	pendMu  sync.Mutex
	pending map[int]chan cdpMsg
	evMu    sync.Mutex
	events  []cdpMsg
	page    pageInfo
}

// connectCdP 连指定 page（targetId；缺省第一个），返回 send + 事件队列。用完 close()。
func connectCdP(port int, targetID string) (*cdpClient, error) {
	if _, err := httpc.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port)); err != nil {
		return nil, fmt.Errorf("CDP 不可达（端口 %d）: %v", port, err)
	}
	pages, err := listPages(port)
	if err != nil {
		return nil, err
	}
	page := pageInfo{}
	found := false
	if targetID != "" {
		for _, p := range pages {
			if p.ID == targetID {
				page, found = p, true
				break
			}
		}
	} else {
		for _, p := range pages {
			if p.Type == "page" {
				page, found = p, true
				break
			}
		}
	}
	if !found {
		if targetID != "" {
			return nil, fmt.Errorf("target %s 不存在（tab 可能已被关）", targetID)
		}
		return nil, errors.New("无 page target")
	}
	ws, _, err := websocket.DefaultDialer.Dial(page.WsURL, nil)
	if err != nil {
		return nil, errors.New("WebSocket 连接失败")
	}
	c := &cdpClient{conn: ws, pending: map[int]chan cdpMsg{}, page: page}
	go c.readLoop()
	if _, err := c.send("Page.enable", nil); err != nil {
		ws.Close()
		return nil, err
	}
	return c, nil
}

func (c *cdpClient) readLoop() {
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg cdpMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.ID != nil {
			c.pendMu.Lock()
			ch, ok := c.pending[*msg.ID]
			if ok {
				delete(c.pending, *msg.ID)
			}
			c.pendMu.Unlock()
			if ok {
				ch <- msg
			}
		} else if msg.Method != "" {
			c.evMu.Lock()
			c.events = append(c.events, msg)
			c.evMu.Unlock()
		}
	}
}

func (c *cdpClient) hasEvent(method string) bool {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	for _, e := range c.events {
		if e.Method == method {
			return true
		}
	}
	return false
}

// send 发 CDP 命令并等响应（30s 看门狗；TS 为无限等待，此处超时给明确报错）
func (c *cdpClient) send(method string, params interface{}) (json.RawMessage, error) {
	c.idc++
	id := c.idc
	ch := make(chan cdpMsg, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()
	payload := map[string]interface{}{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	b, _ := json.Marshal(payload)
	c.sendMu.Lock()
	err := c.conn.WriteMessage(websocket.TextMessage, b)
	c.sendMu.Unlock()
	if err != nil {
		return nil, errors.New("CDP 发送失败（连接已断？）")
	}
	select {
	case msg := <-ch:
		// TS 语义：CDP 协议错误（msg.error）不上抛——调用方按"值缺失"处理
		// （如 doBack 的 history.back() 在导航中拿到错误响应被静默丢弃）。
		// 只有传输层失败（写失败/30s 看门狗）才返回 error。
		if msg.Err != nil {
			return nil, nil
		}
		return msg.Result, nil
	case <-time.After(30 * time.Second):
		c.pendMu.Lock()
		delete(c.pending, id) // 清掉悬挂条目；迟到的响应会被 readLoop 丢弃
		c.pendMu.Unlock()
		return nil, fmt.Errorf("CDP %s 响应超时（30s）", method)
	}
}

// evaluate 跑 Runtime.evaluate（returnByValue），返回 value（present=false 表示 undefined）。
// TS 语义：JS 异常/CDP 协议错误不上抛——调用方只读 value（异常 → present=false），
// 只有传输层错误（连接断/超时）才返回 error。doEval 单独走 send() 检查 exceptionDetails。
func (c *cdpClient) evaluate(expression string, awaitPromise bool) (interface{}, bool, error) {
	res, err := c.send("Runtime.evaluate", map[string]interface{}{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  awaitPromise,
	})
	if err != nil {
		return nil, false, err
	}
	var r struct {
		Value json.RawMessage `json:"value"`
	}
	if res != nil {
		var raw map[string]json.RawMessage
		json.Unmarshal(res, &raw)
		if rd, ok := raw["result"]; ok {
			json.Unmarshal(rd, &r)
		}
	}
	if len(r.Value) > 0 {
		var v interface{}
		json.Unmarshal(r.Value, &v)
		return v, true, nil
	}
	return nil, false, nil
}

// clearTextMarker 清除 text: 解析留下的临时属性（无匹配时是无害空操作）
func (c *cdpClient) clearTextMarker() {
	c.evaluate(`document.querySelectorAll('[data-bctl-text]').forEach(e=>e.removeAttribute('data-bctl-text'));1`, false)
}

// ---------- 选择器解析 ----------

// resolveSelector `text:<txt>` 选择器：按可见文本把选择器解析成真实元素，打临时属性后
// 返回可被 querySelector 用的 CSS。优先级：叶子节点精确匹配 → 非叶精确 → 叶子包含。
func resolveSelector(c *cdpClient, selector string) (css string, resolved string, err error) {
	if !strings.HasPrefix(selector, "text:") {
		return selector, "", nil
	}
	q := selector[5:]
	expr := fmt.Sprintf(`(() => {
      const q = %s;
      const norm = (e) => (e.textContent || "").trim();
      const all = Array.from(document.querySelectorAll(
        "a,button,span,div,li,h1,h2,h3,h4,h5,p,td,th,label,option,summary"));
      const el = all.find((e) => norm(e) === q && e.children.length === 0)
        ?? all.find((e) => norm(e) === q)
        ?? all.find((e) => norm(e).includes(q) && e.children.length === 0);
      if (!el) return JSON.stringify({ found: false });
      el.setAttribute("data-bctl-text", "1");
      return JSON.stringify({ found: true, tag: el.tagName, text: norm(el).slice(0, 60) });
    })()`, jsQuote(q))
	v, _, err := c.evaluate(expr, false)
	if err != nil {
		return "", "", err
	}
	var info struct {
		Found bool   `json:"found"`
		Tag   string `json:"tag"`
		Text  string `json:"text"`
	}
	if s, ok := v.(string); ok {
		json.Unmarshal([]byte(s), &info)
	}
	if !info.Found {
		return "", "", fmt.Errorf("text: 选择器未匹配到元素: %s", selector)
	}
	return "[data-bctl-text='1']", fmt.Sprintf("<%s> \"%s\"", info.Tag, info.Text), nil
}

func selectorLabel(selector, resolved string) string {
	if resolved != "" {
		return fmt.Sprintf("%s → %s", selector, resolved)
	}
	return selector
}

// ---------- 页面快照 ----------

type pageSnap struct {
	URL     string
	Title   string
	BodyLen int
}

const snapExpr = `JSON.stringify({url: location.href, title: document.title, bodyLen: document.body ? document.body.innerText.length : -1})`

func pageSnapshot(c *cdpClient) (pageSnap, error) {
	var out pageSnap
	v, _, err := c.evaluate(snapExpr, false)
	if err != nil {
		return out, err
	}
	if s, ok := v.(string); ok {
		json.Unmarshal([]byte(s), &out)
	}
	return out, nil
}

// ---------- PNG 尺寸（文件头 16-23 字节，不引图像库） ----------

func pngSize(file string) (int, int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	buf := make([]byte, 24)
	if _, err := io.ReadFull(f, buf); err != nil {
		return 0, 0, err
	}
	if string(buf[1:4]) != "PNG" {
		return 0, 0, fmt.Errorf("不是 PNG 文件: %s", file)
	}
	return int(binary.BigEndian.Uint32(buf[16:20])), int(binary.BigEndian.Uint32(buf[20:24])), nil
}

// ---------- 动作实现 ----------

func doOpen(c *cdpClient, url string) error {
	if _, err := c.send("Page.navigate", map[string]interface{}{"url": url}); err != nil {
		return err
	}
	for i := 0; i < 40 && !c.hasEvent("Page.loadEventFired"); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	info, err := pageSnapshot(c)
	if err != nil {
		return err
	}
	urlOut := info.URL
	if urlOut == "" {
		urlOut = url
	}
	fmt.Printf("打开 %s\n标题: %s\n", urlOut, info.Title)
	return nil
}

func doText(c *cdpClient, selector string) error {
	if selector != "" {
		css, _, err := resolveSelector(c, selector)
		if err != nil {
			return err
		}
		v, present, err := c.evaluate(fmt.Sprintf(
			"(() => { const el = document.querySelector(%s); return el ? el.innerText : null; })()", jsQuote(css)), false)
		if err != nil {
			return err
		}
		if !present || v == nil {
			return fmt.Errorf("selector 未匹配到元素: %s", selector)
		}
		fmt.Fprint(os.Stdout, v.(string))
		return nil
	}
	v, _, err := c.evaluate(`document.body ? document.body.innerText : ''`, false)
	if err != nil {
		return err
	}
	if s, ok := v.(string); ok {
		fmt.Fprint(os.Stdout, s)
	}
	return nil
}

func doInfo(c *cdpClient, selector string) error {
	css, resolved, err := resolveSelector(c, selector)
	if err != nil {
		return err
	}
	v, _, err := c.evaluate(fmt.Sprintf(`(() => {
      const el = document.querySelector(%s);
      if (!el) return JSON.stringify({ found: false });
      const r0 = el.getBoundingClientRect();
      const cs = getComputedStyle(el);
      const info = {
        found: true, tag: el.tagName.toLowerCase(), id: el.id || null,
        class: el.className && typeof el.className === "string" ? el.className : null,
        text: (el.innerText || el.textContent || "").trim().slice(0, 120),
        visible: r0.width > 0 && r0.height > 0 && cs.visibility !== "hidden" && cs.display !== "none",
        rect: { x: Math.round(r0.x), y: Math.round(r0.y), w: Math.round(r0.width), h: Math.round(r0.height) },
      };
      if (el instanceof HTMLInputElement)
        Object.assign(info, { type: el.type, value: el.value, placeholder: el.placeholder || null, disabled: el.disabled });
      else if (el instanceof HTMLSelectElement)
        Object.assign(info, { value: el.value, options: Array.from(el.options).map((o) => o.textContent.trim()).slice(0, 20) });
      else if (el instanceof HTMLAnchorElement) info.href = el.href || null;
      else if (el instanceof HTMLButtonElement) info.disabled = el.disabled;
      return JSON.stringify(info);
    })()`, jsQuote(css)), false)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if s, ok := v.(string); ok {
		raw = []byte(s)
	} else {
		raw = []byte("{}")
	}
	var found bool
	json.Unmarshal(raw, &struct{}{}) // no-op
	json.Unmarshal(raw, &found)      // found=false 兜底
	_ = found
	var probe map[string]interface{}
	json.Unmarshal(raw, &probe)
	if f, _ := probe["found"].(bool); !f {
		return fmt.Errorf("selector 未匹配到元素: %s", selector)
	}
	var indented bytes.Buffer
	json.Indent(&indented, raw, "", "  ")
	fmt.Printf("[info] %s\n", selectorLabel(selector, resolved))
	fmt.Println(indented.String())
	return nil
}

func doScreenshot(c *cdpClient, out string, full bool) error {
	params := map[string]interface{}{"format": "png"}
	if full {
		// 整页截图：captureBeyondViewport 突破视口限制，clip 取内容区完整尺寸
		params["captureBeyondViewport"] = true
		res, err := c.send("Page.getLayoutMetrics", nil)
		if err != nil {
			return err
		}
		var m struct {
			CssContentSize *struct {
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
			} `json:"cssContentSize"`
			ContentSize *struct {
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
			} `json:"contentSize"`
		}
		json.Unmarshal(res, &m)
		size := m.CssContentSize
		if size == nil {
			size = m.ContentSize
		}
		if size != nil {
			params["clip"] = map[string]interface{}{
				"x": 0, "y": 0,
				"width": math.Ceil(size.Width), "height": math.Ceil(size.Height), "scale": 1,
			}
		}
	}
	res, err := c.send("Page.captureScreenshot", params)
	if err != nil {
		return err
	}
	var shot struct {
		Data string `json:"data"`
	}
	json.Unmarshal(res, &shot)
	if shot.Data == "" {
		return errors.New("截图无数据")
	}
	b, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		return fmt.Errorf("截图 base64 解码失败: %v", err)
	}
	if err := os.WriteFile(out, b, 0644); err != nil {
		return err
	}
	w, h, _ := pngSize(out)
	fi, _ := os.Stat(out)
	fmt.Printf("截图已存 %s（%d 字节，%d×%dpx）\n", out, fi.Size(), w, h)
	return nil
}

// clickAtCore 坐标点击核心：CDP 真实鼠标事件 + 800ms 观察 + 新 tab 检测 + 变化报告
func clickAtCore(c *cdpClient, port int, before pageSnap, x, y float64, what string) error {
	res, err := c.send("Target.getTargets", nil)
	if err != nil {
		return err
	}
	var tg struct {
		Infos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	json.Unmarshal(res, &tg)
	targetsBefore := map[string]bool{}
	for _, t := range tg.Infos {
		if t.Type == "page" {
			targetsBefore[t.TargetID] = true
		}
	}

	for _, m := range []map[string]interface{}{
		{"type": "mouseMoved", "x": x, "y": y},
		{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1},
		{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1},
	} {
		if _, err := c.send("Input.dispatchMouseEvent", m); err != nil {
			return err
		}
	}

	// 点击可能有副作用（导航/展开/开新 tab），留 800ms 观察，再报告页面是否变化
	time.Sleep(800 * time.Millisecond)
	after, err := pageSnapshot(c)
	if err != nil {
		return err
	}
	changed := before.URL != after.URL || before.Title != after.Title
	// 轮询最多 2s 检测点击是否开了新 tab（window.open 的 target 注册可能慢于 800ms）
	type newPage struct {
		TargetID, URL, Title string
	}
	var newPages []newPage
	for i := 0; i < 10 && len(newPages) == 0; i++ {
		res, err := c.send("Target.getTargets", nil)
		if err != nil {
			return err
		}
		var tg2 struct {
			Infos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
				Title    string `json:"title"`
			} `json:"targetInfos"`
		}
		json.Unmarshal(res, &tg2)
		for _, t := range tg2.Infos {
			if t.Type == "page" && !targetsBefore[t.TargetID] {
				newPages = append(newPages, newPage{t.TargetID, t.URL, t.Title})
			}
		}
		if len(newPages) == 0 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	bodyChanged := !changed && before.BodyLen != after.BodyLen
	lines := []string{
		fmt.Sprintf("已点击 %s @ (%s,%s)", what, jsFixed(x, 0), jsFixed(y, 0)),
	}
	switch {
	case changed:
		lines = append(lines, fmt.Sprintf("页面变化: %s → %s", before.URL, after.URL))
	case bodyChanged:
		lines = append(lines, fmt.Sprintf("页面内容变化（正文长度 %d → %d，url/标题未变）——可能是页内内容切换/手风琴展开", before.BodyLen, after.BodyLen))
	case len(newPages) > 0:
		lines = append(lines, "当前 tab 未变（无导航/标题变化）")
	default:
		lines = append(lines, "页面无变化（无导航/标题变化）")
	}
	if len(newPages) > 0 {
		pages, err := orderedPages(port)
		if err != nil {
			return err
		}
		for _, np := range newPages {
			idx := -1
			for i, p := range pages {
				if p.ID == np.TargetID {
					idx = i
					break
				}
			}
			suffix := ""
			if np.Title != "" {
				suffix = fmt.Sprintf(" — %s", np.Title)
			}
			lines = append(lines,
				fmt.Sprintf("⚠ 点击开了新 tab [%d]: %s%s（用 tab %d 切过去；当前活跃 tab 不变）", idx, np.URL, suffix, idx))
		}
	}
	fmt.Println(strings.Join(lines, "\n"))
	return nil
}

func doClick(c *cdpClient, port int, selector string) error {
	css, resolved, err := resolveSelector(c, selector)
	if err != nil {
		return err
	}
	before, err := pageSnapshot(c)
	if err != nil {
		return err
	}
	v, _, err := c.evaluate(fmt.Sprintf(`(() => {
      const el = document.querySelector(%s);
      if (!el) return JSON.stringify({ found: false });
      el.scrollIntoView({ block: "center" });
      const r = el.getBoundingClientRect();
      return JSON.stringify({ found: true, x: r.x + r.width / 2, y: r.y + r.height / 2, tag: el.tagName });
    })()`, jsQuote(css)), false)
	if err != nil {
		return err
	}
	var box struct {
		Found bool    `json:"found"`
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Tag   string  `json:"tag"`
	}
	if s, ok := v.(string); ok {
		json.Unmarshal([]byte(s), &box)
	}
	if !box.Found {
		return fmt.Errorf("selector 未匹配到元素: %s", selector)
	}
	if math.IsNaN(box.X) || math.IsNaN(box.Y) {
		return errors.New("元素坐标无效（可能是 display:none）")
	}
	return clickAtCore(c, port, before, box.X, box.Y, fmt.Sprintf("<%s>（%s）", box.Tag, selectorLabel(selector, resolved)))
}

func doClickAt(c *cdpClient, port int, px, py float64, shot string) error {
	if !fileExists(shot) {
		return fmt.Errorf("截图不存在: %s", shot)
	}
	dimW, dimH, err := pngSize(shot)
	if err != nil {
		return err
	}
	// 视口尺寸用同会话实时 innerWidth/innerHeight（getLayoutMetrics 在窗口被 WM
	// 调整后会返回陈旧值，2026-09-12 实测踩坑）。imgW = innerW×dpr，比例映射天然含 dpr 换算。
	v, _, err := c.evaluate(`JSON.stringify({ w: innerWidth, h: innerHeight })`, false)
	if err != nil {
		return err
	}
	var vp struct {
		W, H float64
	}
	if s, ok := v.(string); ok {
		json.Unmarshal([]byte(s), &vp)
	}
	if vp.W == 0 || vp.H == 0 {
		return errors.New("取不到视口尺寸")
	}
	// 截图与当前视口差异过大 → 截图已过时（页面/窗口变过），提醒
	stale := math.Abs(float64(dimW)-vp.W*1.0)/vp.W > 0.05 || math.Abs(float64(dimH)-vp.H)/vp.H > 0.08
	x := px * vp.W / float64(dimW)
	y := py * vp.H / float64(dimH)
	if x < 0 || y < 0 || x > vp.W || y > vp.H {
		return fmt.Errorf(
			"坐标 (%s,%s) 换算后 (%s,%s) 超出视口 (%s×%spx)——检查是否用了整页截图或坐标没从缩略图放大回原图",
			jsNum(px), jsNum(py), jsFixed(x, 1), jsFixed(y, 1), jsNum(vp.W), jsNum(vp.H))
	}
	if stale {
		fmt.Fprintf(os.Stderr,
			"⚠ 截图 %d×%dpx 与当前视口 %s×%spx 差异 >5%%（截图可能过时，元素可能已移动——建议重新截图后再点）\n",
			dimW, dimH, jsNum(vp.W), jsNum(vp.H))
	}
	before, err := pageSnapshot(c)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("截图 %d×%dpx 内 (%s,%s) → CSS (%s,%s)", dimW, dimH, jsNum(px), jsNum(py), jsFixed(x, 1), jsFixed(y, 1))
	if stale {
		what += "（⚠截图过时）"
	}
	return clickAtCore(c, port, before, x, y, what)
}

func doType(c *cdpClient, selector, text string, clear bool) error {
	css, resolved, err := resolveSelector(c, selector)
	if err != nil {
		return err
	}
	v, _, err := c.evaluate(fmt.Sprintf(`(() => {
      const el = document.querySelector(%s);
      if (!el) return JSON.stringify({ found: false });
      el.scrollIntoView({ block: "center" });
      const r = el.getBoundingClientRect();
      return JSON.stringify({ found: true, x: r.x + r.width / 2, y: r.y + r.height / 2, tag: el.tagName });
    })()`, jsQuote(css)), false)
	if err != nil {
		return err
	}
	var box struct {
		Found bool    `json:"found"`
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Tag   string  `json:"tag"`
	}
	if s, ok := v.(string); ok {
		json.Unmarshal([]byte(s), &box)
	}
	if !box.Found {
		return fmt.Errorf("selector 未匹配到元素: %s", selector)
	}
	if math.IsNaN(box.X) || math.IsNaN(box.Y) {
		return errors.New("元素坐标无效（可能是 display:none）")
	}

	// 点一下聚焦（让 input 事件落在该元素上）
	for _, m := range []map[string]interface{}{
		{"type": "mousePressed", "x": box.X, "y": box.Y, "button": "left", "clickCount": 1},
		{"type": "mouseReleased", "x": box.X, "y": box.Y, "button": "left", "clickCount": 1},
	} {
		if _, err := c.send("Input.dispatchMouseEvent", m); err != nil {
			return err
		}
	}

	if clear {
		// Ctrl+A 全选 + Backspace 删除
		for _, m := range []map[string]interface{}{
			{"type": "keyDown", "modifiers": 2, "key": "a", "code": "KeyA", "windowsVirtualKeyCode": 65},
			{"type": "keyUp", "modifiers": 2, "key": "a", "code": "KeyA", "windowsVirtualKeyCode": 65},
			{"type": "keyDown", "key": "Backspace", "code": "Backspace", "windowsVirtualKeyCode": 8},
			{"type": "keyUp", "key": "Backspace", "code": "Backspace", "windowsVirtualKeyCode": 8},
		} {
			if _, err := c.send("Input.dispatchKeyEvent", m); err != nil {
				return err
			}
		}
	}

	// insertText 整段插入并触发 input 事件（对 React/Vue 受控组件也生效）
	if _, err := c.send("Input.insertText", map[string]interface{}{"text": text}); err != nil {
		return err
	}

	v2, present, err := c.evaluate(fmt.Sprintf(
		"(() => { const el = document.querySelector(%s); return el ? (el.value ?? \"\") : \"\"; })()", jsQuote(css)), false)
	if err != nil {
		return err
	}
	val := ""
	if present {
		if s, ok := v2.(string); ok {
			val = s
		}
	}
	clearSuffix := ""
	if clear {
		clearSuffix = "（已先清空）"
	}
	fmt.Printf("已往 <%s>（%s）输入 %s%s\n当前值: %s\n", box.Tag, selectorLabel(selector, resolved), jsString(text), clearSuffix, jsString(val))
	return nil
}

func doPress(c *cdpClient, key string) error {
	// 解析组合键：形如 "Control+a" → modifiers + 主键
	modifiers := 0
	finalKey := key
	parts := strings.Split(key, "+")
	if len(parts) > 1 {
		for _, p := range parts[:len(parts)-1] {
			m := strings.ToLower(strings.TrimSpace(p))
			switch m {
			case "control", "ctrl":
				modifiers |= 2
			case "alt":
				modifiers |= 1
			case "shift":
				modifiers |= 8
			case "meta", "command", "cmd":
				modifiers |= 4
			default:
				return fmt.Errorf("未知修饰键: %s（支持 Control/Alt/Shift/Meta）", p)
			}
		}
		finalKey = parts[len(parts)-1]
	}
	codeMap := map[string]string{
		"Enter": "Enter", "Tab": "Tab", "Escape": "Escape", "Backspace": "Backspace",
		"Delete": "Delete", " ": "Space", "ArrowUp": "ArrowUp", "ArrowDown": "ArrowDown",
		"ArrowLeft": "ArrowLeft", "ArrowRight": "ArrowRight", "Home": "Home", "End": "End",
		"PageUp": "PageUp", "PageDown": "PageDown",
	}
	rk := []rune(finalKey)
	single := len(rk) == 1 && rk[0] < 0x10000
	code, ok := codeMap[finalKey]
	if !ok {
		if single {
			code = "Key" + strings.ToUpper(string(rk[0]))
		} else {
			code = finalKey
		}
	}
	vk := 0
	if single {
		vk = int(rk[0])
	}
	for _, t := range []string{"keyDown", "keyUp"} {
		if _, err := c.send("Input.dispatchKeyEvent", map[string]interface{}{
			"type": t, "key": finalKey, "code": code, "modifiers": modifiers, "windowsVirtualKeyCode": vk,
		}); err != nil {
			return err
		}
	}
	fmt.Printf("已按键 %s（key=%s code=%s modifiers=%d）\n", key, finalKey, code, modifiers)
	return nil
}

func doEval(c *cdpClient, js string) error {
	res, err := c.send("Runtime.evaluate", map[string]interface{}{
		"expression":    js,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(res, &raw)
	if d, ok := raw["exceptionDetails"]; ok {
		var exc struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		}
		json.Unmarshal(d, &exc)
		desc := ""
		if exc.Exception != nil {
			desc = exc.Exception.Description
		}
		if desc == "" {
			desc = exc.Text
		}
		if desc == "" {
			desc = "unknown"
		}
		return fmt.Errorf("eval 抛错: %s", desc)
	}
	var r struct {
		Value json.RawMessage `json:"value"`
	}
	present := false
	if rd, ok := raw["result"]; ok {
		json.Unmarshal(rd, &r)
		if len(r.Value) > 0 {
			present = true
		}
	}
	if !present {
		fmt.Println("(无返回值)")
		return nil
	}
	var v interface{}
	json.Unmarshal(r.Value, &v)
	if s, ok := v.(string); ok {
		fmt.Fprint(os.Stdout, s+"\n")
		return nil
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	e.Encode(v)
	fmt.Print(b.String()) // Encode 自带尾部换行，与 TS console.log 对齐
	return nil
}

func doWait(c *cdpClient, target string, timeoutMs int) error {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	if regexp.MustCompile(`^\d+$`).MatchString(target) {
		ms, _ := strconv.Atoi(target)
		sleepMs := ms
		if timeoutMs < sleepMs {
			sleepMs = timeoutMs
		}
		time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		if sleepMs < ms {
			fmt.Printf("已等待 %dms（要求 %dms，被 timeout 截断）\n", sleepMs, ms)
		} else {
			fmt.Printf("已等待 %dms\n", ms)
		}
		return nil
	}
	if strings.HasPrefix(target, "text:") {
		needle := target[5:]
		for time.Now().Before(deadline) {
			v, _, err := c.evaluate(`document.body ? document.body.innerText : ''`, false)
			if err != nil {
				return err
			}
			if s, ok := v.(string); ok && strings.Contains(s, needle) {
				fmt.Printf("文本已出现: %s\n", jsString(needle))
				return nil
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("超时 %dms：文本未出现 %s", timeoutMs, jsString(needle))
	}
	for time.Now().Before(deadline) {
		v, _, err := c.evaluate(fmt.Sprintf(`document.querySelector(%s) !== null`, jsQuote(target)), false)
		if err != nil {
			return err
		}
		if b, ok := v.(bool); ok && b {
			fmt.Printf("元素已出现: %s\n", target)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("超时 %dms：元素未出现 %s", timeoutMs, target)
}

func doScroll(c *cdpClient, target string) error {
	yBefore, err := scrollY(c)
	if err != nil {
		return err
	}
	switch target {
	case "top":
		c.evaluate(`window.scrollTo(0,0)`, false)
	case "bottom":
		c.evaluate(`window.scrollTo(0, document.documentElement.scrollHeight)`, false)
	case "up":
		c.evaluate(`window.scrollBy(0, -window.innerHeight)`, false)
	case "down":
		c.evaluate(`window.scrollBy(0, window.innerHeight)`, false)
	default:
		v, _, err := c.evaluate(fmt.Sprintf(`(() => {
        const el = document.querySelector(%s);
        if (!el) return null;
        el.scrollIntoView({ block: "center" });
        return true;
      })()`, jsQuote(target)), false)
		if err != nil {
			return err
		}
		if b, ok := v.(bool); !ok || !b {
			return fmt.Errorf("selector 未匹配到元素: %s", target)
		}
	}
	yAfter, err := scrollY(c)
	if err != nil {
		return err
	}
	fmt.Printf("滚动完成: scrollY %s → %s\n", jsNum(yBefore), jsNum(yAfter))
	return nil
}

func scrollY(c *cdpClient) (float64, error) {
	v, _, err := c.evaluate(`window.scrollY`, false)
	if err != nil {
		return 0, err
	}
	if f, ok := v.(float64); ok {
		return f, nil
	}
	return 0, nil
}

func doBack(c *cdpClient) error {
	lenV, _, err := c.evaluate(`history.length`, false)
	if err != nil {
		return err
	}
	length := 0.0
	if f, ok := lenV.(float64); ok {
		length = f
	}
	if length <= 1 {
		fmt.Println("无可回退的历史")
		return nil
	}
	if _, err := c.send("Runtime.evaluate", map[string]interface{}{"expression": "history.back()"}); err != nil {
		return err
	}
	time.Sleep(600 * time.Millisecond)
	v, _, err := c.evaluate(`location.href`, false)
	if err != nil {
		return err
	}
	if s, ok := v.(string); ok {
		fmt.Printf("后退到 %s\n", s)
	}
	return nil
}

func doForward(c *cdpClient) error {
	if _, err := c.send("Runtime.evaluate", map[string]interface{}{"expression": "history.forward()"}); err != nil {
		return err
	}
	time.Sleep(600 * time.Millisecond)
	v, _, err := c.evaluate(`location.href`, false)
	if err != nil {
		return err
	}
	if s, ok := v.(string); ok {
		fmt.Printf("前进到 %s\n", s)
	}
	return nil
}

func doSelect(c *cdpClient, selector, value string) error {
	css, resolved, err := resolveSelector(c, selector)
	if err != nil {
		return err
	}
	v, _, err := c.evaluate(fmt.Sprintf(`(() => {
      const el = document.querySelector(%s);
      if (!el) return JSON.stringify({ found: false });
      if (el.tagName !== 'SELECT') return JSON.stringify({ found: false, reason: 'not a <select>' });
      const want = %s;
      let opt = Array.from(el.options).find(o => o.value === want);
      if (!opt) opt = Array.from(el.options).find(o => o.textContent.trim() === want);
      if (!opt) return JSON.stringify({ found: true, matched: false, options: Array.from(el.options).map(o => o.value) });
      el.value = opt.value;
      el.dispatchEvent(new Event('change', { bubbles: true }));
      return JSON.stringify({ found: true, matched: true, value: opt.value, text: opt.textContent.trim() });
    })()`, jsQuote(css), jsQuote(value)), false)
	if err != nil {
		return err
	}
	var info struct {
		Found   bool     `json:"found"`
		Matched bool     `json:"matched"`
		Value   string   `json:"value"`
		Text    string   `json:"text"`
		Options []string `json:"options"`
	}
	if s, ok := v.(string); ok {
		json.Unmarshal([]byte(s), &info)
	}
	if !info.Found {
		return fmt.Errorf("selector 未匹配到 <select> 元素: %s", selector)
	}
	if !info.Matched {
		return fmt.Errorf("选项未匹配到 %s，可选值: %s", jsString(value), jsStringSlice(info.Options))
	}
	fmt.Printf("已选 <select>（%s）= %s（%s）\n", selectorLabel(selector, resolved), jsString(info.Value), info.Text)
	return nil
}

func jsStringSlice(s []string) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func doHover(c *cdpClient, selector string) error {
	css, resolved, err := resolveSelector(c, selector)
	if err != nil {
		return err
	}
	v, _, err := c.evaluate(fmt.Sprintf(`(() => {
      const el = document.querySelector(%s);
      if (!el) return JSON.stringify({ found: false });
      el.scrollIntoView({ block: "center" });
      const r = el.getBoundingClientRect();
      return JSON.stringify({ found: true, x: r.x + r.width / 2, y: r.y + r.height / 2, tag: el.tagName });
    })()`, jsQuote(css)), false)
	if err != nil {
		return err
	}
	var box struct {
		Found bool    `json:"found"`
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Tag   string  `json:"tag"`
	}
	if s, ok := v.(string); ok {
		json.Unmarshal([]byte(s), &box)
	}
	if !box.Found {
		return fmt.Errorf("selector 未匹配到元素: %s", selector)
	}
	if _, err := c.send("Input.dispatchMouseEvent", map[string]interface{}{
		"type": "mouseMoved", "x": box.X, "y": box.Y, "button": "none",
	}); err != nil {
		return err
	}
	fmt.Printf("已悬停 <%s>（%s）@ (%s,%s)\n", box.Tag, selectorLabel(selector, resolved), jsFixed(box.X, 0), jsFixed(box.Y, 0))
	return nil
}

func doFileUpload(c *cdpClient, selector, path string) error {
	if !fileExists(path) {
		return fmt.Errorf("文件不存在: %s", path)
	}
	if _, err := c.send("DOM.enable", nil); err != nil {
		return err
	}
	res, err := c.send("DOM.getDocument", nil)
	if err != nil {
		return err
	}
	var doc struct {
		Root struct {
			NodeID uint64 `json:"nodeId"`
		} `json:"root"`
	}
	json.Unmarshal(res, &doc)
	res, err = c.send("DOM.querySelector", map[string]interface{}{"nodeId": doc.Root.NodeID, "selector": selector})
	if err != nil {
		return err
	}
	var n struct {
		NodeID uint64 `json:"nodeId"`
	}
	json.Unmarshal(res, &n)
	if n.NodeID == 0 {
		return fmt.Errorf("selector 未匹配到 <input type=file> 元素: %s", selector)
	}
	if _, err := c.send("DOM.setFileInputFiles", map[string]interface{}{"files": []string{path}, "nodeId": n.NodeID}); err != nil {
		return err
	}
	fmt.Printf("已上传文件 %s → <input>（%s）\n", path, selector)
	return nil
}

// ---------- 报告 ----------

func tabListReport(port int) (string, error) {
	pages, err := orderedPages(port)
	if err != nil {
		return "", err
	}
	if len(pages) == 0 {
		return "【tab】（无）", nil
	}
	active := ""
	if id, err := activeTargetId(port); err == nil {
		active = id
	}
	var b strings.Builder
	fmt.Fprintf(&b, "【tab】共 %d 个：\n", len(pages))
	for i, p := range pages {
		mark := ""
		if p.ID == active {
			mark = " *活跃"
		}
		title := p.Title
		if title == "" {
			title = "(无标题)"
		}
		fmt.Fprintf(&b, "  [%d]%s %s  %s\n", i, mark, title, p.URL)
	}
	return b.String(), nil
}

func screenReport(c *cdpClient, port int) (string, error) {
	v, _, err := c.evaluate(`JSON.stringify({url: location.href, title: document.title, body: document.body ? document.body.innerText : ''})`, false)
	if err != nil {
		return "", err
	}
	var p struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if s, ok := v.(string); ok {
		json.Unmarshal([]byte(s), &p)
	}
	tabs, err := tabListReport(port)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("\n【屏幕】%s\n当前页: %s\n标题: %s\n正文:\n%s", tabs, p.URL, p.Title, p.Body), nil
}

// ---------- 动作实现（tab 管理） ----------

func doTabs(port int) error {
	pages, err := orderedPages(port)
	if err != nil {
		return err
	}
	if len(pages) == 0 {
		fmt.Println("（无 tab）")
		return nil
	}
	active, err := activeTargetId(port)
	if err != nil {
		return err
	}
	fmt.Printf("共 %d 个 tab：\n", len(pages))
	for i, p := range pages {
		mark := ""
		if p.ID == active {
			mark = " *活跃"
		}
		title := p.Title
		if title == "" {
			title = "(无标题)"
		}
		fmt.Printf("  [%d]%s %s  %s\n", i, mark, title, p.URL)
	}
	return nil
}

func doNew(port int, url string) error {
	t, err := newTab(port, "about:blank")
	if err != nil {
		return err
	}
	if t.ID == "" {
		return errors.New("开新 tab 未返回 targetId")
	}
	setActiveTab(port, t.ID)
	c, err := connectCdP(port, t.ID)
	if err != nil {
		return err
	}
	if _, err := c.send("Page.navigate", map[string]interface{}{"url": url}); err != nil {
		c.conn.Close()
		return err
	}
	for i := 0; i < 40 && !c.hasEvent("Page.loadEventFired"); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	v, _, err := c.evaluate(snapExpr, false)
	c.conn.Close()
	if err != nil {
		return err
	}
	var p struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if s, ok := v.(string); ok {
		json.Unmarshal([]byte(s), &p)
	}
	urlOut := p.URL
	if urlOut == "" {
		urlOut = url
	}
	id8 := t.ID
	if len(id8) > 8 {
		id8 = id8[:8]
	}
	titleLine := ""
	if p.Title != "" {
		titleLine = fmt.Sprintf("\n标题: %s", p.Title)
	}
	fmt.Printf("已开新 tab [%s…] → %s（现为活跃 tab）%s\n", id8, urlOut, titleLine)
	return nil
}

func doTabSwitch(port int, index int) error {
	pages, err := orderedPages(port)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(pages) {
		return fmt.Errorf("tab 索引 %d 越界（共 %d 个，0-%d）", index, len(pages), len(pages)-1)
	}
	t := pages[index]
	setActiveTab(port, t.ID)
	title := t.Title
	if title == "" {
		title = "(无标题)"
	}
	fmt.Printf("已切换到 tab [%d]：%s  %s\n", index, title, t.URL)
	return nil
}

func doCloseTab(port int, index int) error {
	pages, err := orderedPages(port)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(pages) {
		return fmt.Errorf("tab 索引 %d 越界（共 %d 个，0-%d）", index, len(pages), len(pages)-1)
	}
	t := pages[index]
	prevActive, err := activeTargetId(port)
	if err != nil {
		return err
	}
	wasActive := t.ID == prevActive
	if err := closeTab(port, t.ID); err != nil {
		return err
	}
	// /json/close 是异步的：轮询直到该 tab 真正从列表消失，否则计数会把它算进去
	for i := 0; i < 40; i++ {
		cur, err := listPages(port)
		if err != nil {
			return err
		}
		gone := true
		for _, p := range cur {
			if p.ID == t.ID {
				gone = false
				break
			}
		}
		if gone {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	rest, err := orderedPages(port)
	if err != nil {
		return err
	}
	if wasActive {
		if len(rest) > 0 {
			setActiveTab(port, rest[0].ID)
		} else {
			clearActiveTab(port)
		}
	}
	suffix := ""
	if wasActive {
		suffix = "（活跃已落到剩余第一个）"
	}
	fmt.Printf("已关闭 tab [%d]（%s），剩余 %d 个%s\n", index, t.URL, len(rest), suffix)
	return nil
}

// ---------- 编排 ----------

var fullScreenActions = map[string]bool{
	"open": true, "navigate": true, "click": true, "type": true, "press": true,
	"select": true, "hover": true, "back": true, "forward": true,
}

// withInstance 实例检查 + CDP 连接 + 动作执行 + 事后报告 + 心跳刷新
func withInstance(port int, action string, fn func(*cdpClient) error, needsPage bool) {
	hb := hbFileOf(port)
	st := stateFileOf(port)
	stop := stopFileOf(port)
	if !fileExists(hb) || !fileExists(st) {
		fmt.Fprintf(os.Stderr, "browserctl: 实例 :%d 未起（先 nohup browserd %d ... 起一个）\n", port, port)
		os.Exit(2)
	}
	if fileExists(stop) {
		fmt.Fprintf(os.Stderr, "browserctl: 实例 :%d 正在关闭\n", port)
		os.Exit(2)
	}
	var c *cdpClient
	if needsPage {
		tid, err := activeTargetId(port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "browserctl: CDP 连接 :%d 失败（%s）——实例可能已退出\n", port, err.Error())
			os.Exit(2)
		}
		c, err = connectCdP(port, tid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "browserctl: CDP 连接 :%d 失败（%s）——实例可能已退出\n", port, err.Error())
			os.Exit(2)
		}
	}
	// 刷新心跳（除 close 外所有动作）
	if action != "close" {
		os.WriteFile(hb, []byte(strconv.FormatInt(time.Now().UnixMilli(), 10)), 0644)
	}
	defer func() {
		if c != nil {
			c.clearTextMarker()
			c.conn.Close()
		}
	}()
	if err := fn(c); err != nil {
		fmt.Fprintf(os.Stderr, "browserctl: %s\n", err.Error())
		os.Exit(3)
	}
	if action != "close" && c != nil {
		if fullScreenActions[action] {
			rep, err := screenReport(c, port)
			if err != nil {
				fmt.Fprintf(os.Stderr, "browserctl: %s\n", err.Error())
				os.Exit(3)
			}
			fmt.Println(rep)
		} else if action == "scroll" || action == "file_upload" {
			rep, err := tabListReport(port)
			if err != nil {
				fmt.Fprintf(os.Stderr, "browserctl: %s\n", err.Error())
				os.Exit(3)
			}
			fmt.Println(rep)
		}
	}
	if action != "close" {
		n, err := listPages(port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "browserctl: %s\n", err.Error())
			os.Exit(3)
		}
		if len(n) > 5 {
			fmt.Printf("\n⚠ 标签页已 %d 个（超过 5 个），要不要关闭多余的标签页？可用 close_tab <index> 关掉不需要的。\n", len(n))
		}
	}
}

// ---------- CLI ----------

type ctlArgs struct {
	port   int
	action string
	args   []string
	out    *string
	clear  bool
	full   bool
	shot   *string
}

func parseCtlArgs(argv []string) ctlArgs {
	if len(argv) < 2 {
		ctlFail("usage: browserctl <port> <action> [args...] [--out <path>] [--clear]")
	}
	f, err := strconv.ParseFloat(argv[0], 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f || f < 1 || f > 65535 {
		ctlFail(fmt.Sprintf("port 须为 1-65535 整数，收到: %s", argv[0]))
	}
	action := argv[1]
	a := ctlArgs{port: int(f), action: action}
	afterDashDash := false
	for i := 2; i < len(argv); i++ {
		tok := argv[i]
		if !afterDashDash && tok == "--" {
			afterDashDash = true
			continue
		}
		if !afterDashDash && tok == "--out" {
			if i+1 >= len(argv) {
				ctlFail("--out 需要路径")
			}
			v := argv[i+1]
			a.out = &v
			i++
			continue
		}
		if !afterDashDash && tok == "--clear" {
			a.clear = true
			continue
		}
		if !afterDashDash && tok == "--full" {
			a.full = true
			continue
		}
		if !afterDashDash && tok == "--shot" {
			if i+1 >= len(argv) {
				ctlFail("--shot 需要截图路径")
			}
			v := argv[i+1]
			a.shot = &v
			i++
			continue
		}
		a.args = append(a.args, tok)
	}
	return a
}

func ctlFail(msg string) {
	fmt.Fprintln(os.Stderr, "browserctl: "+msg)
	os.Exit(3)
}

func main() {
	a := parseCtlArgs(os.Args[1:])
	var err error
	switch a.action {
	case "open", "navigate":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail(fmt.Sprintf("%s 需要 <url>", a.action))
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doOpen(c, a.args[0]) }, true)
	case "text":
		withInstance(a.port, a.action, func(c *cdpClient) error { return doText(c, sel0(a.args)) }, true)
	case "info":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("info 需要 <css-selector|text:txt>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doInfo(c, a.args[0]) }, true)
	case "click":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("click 需要 <css-selector>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doClick(c, a.port, a.args[0]) }, true)
	case "click_at":
		px, ok1 := jsNumber(a.args, 0)
		py, ok2 := jsNumber(a.args, 1)
		if !ok1 || !ok2 {
			ctlFail("click_at 需要 <x> <y>（截图内像素坐标）")
		}
		if a.shot == nil {
			ctlFail("click_at 需要 --shot <视口截图.png>（非整页截图）")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doClickAt(c, a.port, px, py, *a.shot) }, true)
	case "type":
		if len(a.args) < 2 || a.args[0] == "" || a.args[1] == "" {
			ctlFail("type 需要 <css-selector> <text>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doType(c, a.args[0], a.args[1], a.clear) }, true)
	case "press":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("press 需要 <key>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doPress(c, a.args[0]) }, true)
	case "eval":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("eval 需要 <js>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doEval(c, a.args[0]) }, true)
	case "wait":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("wait 需要 <selector|text:…|<ms>>")
		}
		timeoutMs := 10000
		if len(a.args) > 1 && a.args[1] != "" {
			tf, terr := strconv.ParseFloat(a.args[1], 64)
			if terr != nil || math.IsNaN(tf) || math.IsInf(tf, 0) || tf <= 0 {
				ctlFail(fmt.Sprintf("wait 的 timeout 须为正整数毫秒，收到: %s", a.args[1]))
			}
			timeoutMs = int(tf)
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doWait(c, a.args[0], timeoutMs) }, true)
	case "scroll":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("scroll 需要 <up|down|top|bottom|<css>>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doScroll(c, a.args[0]) }, true)
	case "back":
		withInstance(a.port, a.action, doBack, true)
	case "forward":
		withInstance(a.port, a.action, doForward, true)
	case "select":
		if len(a.args) < 2 || a.args[0] == "" || a.args[1] == "" {
			ctlFail("select 需要 <css-selector> <value>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doSelect(c, a.args[0], a.args[1]) }, true)
	case "hover":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("hover 需要 <css-selector>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doHover(c, a.args[0]) }, true)
	case "file_upload":
		if len(a.args) < 2 || a.args[0] == "" || a.args[1] == "" {
			ctlFail("file_upload 需要 <css-selector> <path>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doFileUpload(c, a.args[0], a.args[1]) }, true)
	case "tabs":
		withInstance(a.port, a.action, func(c *cdpClient) error { return doTabs(a.port) }, false)
	case "new":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("new 需要 <url>")
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doNew(a.port, a.args[0]) }, false)
	case "tab":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("tab 需要 <index>")
		}
		idx, ok := nonNegInt(a.args[0])
		if !ok {
			ctlFail(fmt.Sprintf("tab 索引须为非负整数，收到: %s", a.args[0]))
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doTabSwitch(a.port, idx) }, true)
	case "close_tab":
		if len(a.args) < 1 || a.args[0] == "" {
			ctlFail("close_tab 需要 <index>")
		}
		cidx, ok := nonNegInt(a.args[0])
		if !ok {
			ctlFail(fmt.Sprintf("close_tab 索引须为非负整数，收到: %s", a.args[0]))
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doCloseTab(a.port, cidx) }, false)
	case "cookies":
		// CDP Network.getAllCookies——全浏览器 cookie（含 httpOnly，如 B 站 SESSDATA）
		// 用法: cookies [域名子串过滤] [--json]；默认输出 Cookie 头格式（name=value; ...）一行
		var filter string
		asJson := false
		for _, x := range a.args {
			if x == "--json" {
				asJson = true
			} else if filter == "" && !strings.HasPrefix(x, "--") {
				filter = x
			}
		}
		withInstance(a.port, a.action, func(c *cdpClient) error {
			res, err := c.send("Network.getAllCookies", nil)
			if err != nil {
				return err
			}
			var j struct {
				Cookies []struct {
					Name   string `json:"name"`
					Value  string `json:"value"`
					Domain string `json:"domain"`
				} `json:"cookies"`
			}
			if err := json.Unmarshal(res, &j); err != nil {
				return err
			}
			var kept []struct {
				Name   string `json:"name"`
				Value  string `json:"value"`
				Domain string `json:"domain"`
			}
			var header []string
			for _, ck := range j.Cookies {
				if filter == "" || strings.Contains(ck.Domain, filter) {
					kept = append(kept, ck)
					header = append(header, ck.Name+"="+ck.Value)
				}
			}
			if asJson {
				if len(kept) == 0 {
					fmt.Println("[]")
				} else {
					b, _ := json.MarshalIndent(kept, "", "  ")
					fmt.Println(string(b))
				}
			} else {
				fmt.Println(strings.Join(header, "; "))
			}
			return nil
		}, true)
	case "screenshot":
		target := fmt.Sprintf("/tmp/browser-%d-%s.png", a.port, strconv.FormatInt(time.Now().UnixMilli(), 36))
		if a.out != nil {
			target = *a.out
		}
		withInstance(a.port, a.action, func(c *cdpClient) error { return doScreenshot(c, target, a.full) }, true)
	case "status":
		withInstance(a.port, a.action, func(c *cdpClient) error {
			b, err := os.ReadFile(stateFileOf(a.port))
			if err != nil {
				return err
			}
			var st struct {
				ChromePID int  `json:"chrome_pid"`
				Keep      bool `json:"keep"`
				Shared    bool `json:"shared"`
				Headed    bool `json:"headed"`
			}
			json.Unmarshal(b, &st)
			out := struct {
				Port      int    `json:"port"`
				ChromePID int    `json:"chrome_pid"`
				Keep      bool   `json:"keep"`
				Shared    bool   `json:"shared"`
				Headed    bool   `json:"headed"`
				URL       string `json:"url"`
				Title     string `json:"title"`
			}{a.port, st.ChromePID, st.Keep, st.Shared, st.Headed, c.page.URL, c.page.Title}
			var buf bytes.Buffer
			e := json.NewEncoder(&buf)
			e.SetEscapeHTML(false)
			e.SetIndent("", "  ")
			e.Encode(out)
			fmt.Print(buf.String()) // Encode 自带尾部换行，与 TS console.log 对齐
			return nil
		}, true)
	case "close":
		if !fileExists(hbFileOf(a.port)) {
			fmt.Fprintf(os.Stderr, "browserctl: 实例 :%d 不存在\n", a.port)
			os.Exit(2)
		}
		b, err := os.ReadFile(stateFileOf(a.port))
		if err != nil {
			ctlFail(err.Error())
		}
		var st struct {
			DaemonPID int  `json:"daemon_pid"`
			Shared    bool `json:"shared"`
		}
		json.Unmarshal(b, &st)
		if st.Shared {
			// 共享实例：软关闭——保留一个页面（页面归零会触发 chromium 清 session cookie），
			// 关掉其余；进程继续活、登录态保留（杀进程才灭）
			pages, err := listPages(a.port)
			if err != nil {
				ctlFail(err.Error())
			}
			if len(pages) == 0 {
				t0, err := newTab(a.port, "about:blank")
				if err == nil && t0.ID != "" {
					setActiveTab(a.port, t0.ID)
				}
			} else {
				for _, p := range pages[1:] {
					doMethod("GET", fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", a.port, p.ID), nil)
				}
			}
			clearActiveTab(a.port)
			closed := len(pages) - 1
			if closed < 0 {
				closed = 0
			}
			fmt.Printf("共享实例 :%d：已关 %d 个 tab（保留 1 个页面维持登录态），进程继续运行；kill %d 清除全部状态\n", a.port, closed, st.DaemonPID)
		} else {
			os.WriteFile(stopFileOf(a.port), []byte(strconv.FormatInt(time.Now().UnixMilli(), 10)), 0644)
			fmt.Printf("已通知实例 :%d 关闭（browserd 5s 内退出并清理）\n", a.port)
		}
	default:
		ctlFail(fmt.Sprintf("未知动作: %s（支持 open/navigate/text/click/type/press/eval/wait/scroll/back/forward/select/hover/file_upload/tabs/new/tab/close_tab/screenshot/status/close）", a.action))
	}
	_ = err
}

func sel0(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// jsNumber 对齐 JS Number(str)：空串 → 0，非数字 → NaN(ok=false)
func jsNumber(args []string, i int) (float64, bool) {
	if i >= len(args) {
		return 0, false
	}
	s := args[i]
	if s == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	if math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func nonNegInt(s string) (int, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f || f < 0 {
		return 0, false
	}
	return int(f), true
}
