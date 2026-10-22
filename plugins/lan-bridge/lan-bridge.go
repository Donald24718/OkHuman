// okhuman-lan-bridge：把本机 OkHuman 实例的 WebUI 安全地暴露到局域网（独立进程）。
//
// 为什么是"桥"而不是改主程序：主程序只监听 127.0.0.1 且**零鉴权**，把 host 改成
// 0.0.0.0 等于把 /history（会话全文）、/enqueue（指挥 agent）、/config（改配置）
// 白送给同网段任何人。本插件是**唯一**监听局域网的东西：主程序继续只听 localhost，
// 鉴权只在这一处，能走哪些端点由白名单决定。
//
// 手机侧不需要装任何东西：浏览器打开 http://<PC>:8602/login 输 token 即可，
// 之后加载的就是实例自己的 webui（webui 全用相对路径 fetch，所以原样转发就能跑）。
//
// 解耦契约：不 import OkHuman 任何模块，只走公开 HTTP 接口；config.json / data/ 自管。
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- 配置 ----------

type Instance struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type ListenCfg struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type Cfg struct {
	Listen        ListenCfg  `json:"listen"`
	Token         string     `json:"token"` // 空 → 首次启动生成随机值并写回本文件（不打进日志）
	Instances     []Instance `json:"instances"`
	DefaultInst   string     `json:"default_instance"`
	AllowCfgWrite bool       `json:"allow_config_write"` // 默认 false：手机上不能改实例配置
}

func defaultCfg() Cfg {
	return Cfg{
		Listen:      ListenCfg{Host: "0.0.0.0", Port: 8602},
		Instances:   []Instance{{ID: "8451", URL: "http://127.0.0.1:8451"}},
		DefaultInst: "8451",
	}
}

func loadCfg(path string) (*Cfg, bool, error) {
	cfg := defaultCfg()
	generated := false
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, false, fmt.Errorf("config.json 解析失败: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}
	if cfg.Listen.Host == "" {
		cfg.Listen.Host = "0.0.0.0"
	}
	if cfg.Listen.Port <= 0 {
		cfg.Listen.Port = 8602
	}
	if len(cfg.Instances) == 0 {
		return nil, false, fmt.Errorf("instances 不能为空")
	}
	for i := range cfg.Instances {
		cfg.Instances[i].URL = strings.TrimRight(cfg.Instances[i].URL, "/")
		if cfg.Instances[i].ID == "" {
			cfg.Instances[i].ID = cfg.Instances[i].URL
		}
	}
	if cfg.DefaultInst == "" {
		cfg.DefaultInst = cfg.Instances[0].ID
	}
	if cfg.Token == "" {
		nb := make([]byte, 16)
		if _, err := rand.Read(nb); err != nil {
			return nil, false, fmt.Errorf("生成 token 失败: %w", err)
		}
		cfg.Token = hex.EncodeToString(nb)
		generated = true
	}
	return &cfg, generated, nil
}

func saveCfg(path string, cfg *Cfg) error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- 端点白名单（默认拒绝；上游地址永远来自配置，绝不从请求里取）----------

func allowList(cfg *Cfg) map[string][]string {
	m := map[string][]string{
		"/":               {"GET"},
		"/favicon.ico":    {"GET"},
		"/marked.min.js":  {"GET"},
		"/sanitize.js":    {"GET"},
		"/health":         {"GET"},
		"/status":         {"GET"},
		"/queue":          {"GET"},
		"/history":        {"GET"},
		"/config":         {"GET"},
		"/prompts":        {"GET"},
		"/prompts/file":   {"GET", "POST"}, // 提示词页读/写单个文件（漏了它手机上进提示词页就 403）
		"/prompts/reload": {"POST"},
		"/attachments":    {"GET"},
		"/events":         {"GET"},
		"/events/poll":    {"GET"},
		"/enqueue":        {"POST"},
		"/stop":           {"POST"},
		"/reset":          {"POST"},
		"/compress":       {"POST"},
		"/strip-injects":  {"POST"},
		"/inject":         {"POST"},
	}
	if cfg.AllowCfgWrite {
		m["/config"] = append(m["/config"], "POST")
	}
	return m
}

// ---------- 桥 ----------

type bridge struct {
	cfg    Cfg
	allow  map[string][]string
	client *http.Client
	cookie string
}

const instCookie = "okh_inst"

func newBridge(cfg *Cfg) *bridge {
	return &bridge{
		cfg:   *cfg,
		allow: allowList(cfg),
		// 不设 Timeout：/events 是常驻 SSE，超时会把流掐掉
		client: &http.Client{},
		cookie: "okh_bridge",
	}
}

func (b *bridge) findInst(id string) *Instance {
	for i := range b.cfg.Instances {
		if b.cfg.Instances[i].ID == id {
			return &b.cfg.Instances[i]
		}
	}
	return nil
}

// pickInst 选上游：cookie 指定 → 配置默认 → 第一个。认不出的 cookie 退回默认，不报错。
func (b *bridge) pickInst(r *http.Request) *Instance {
	if c, err := r.Cookie(instCookie); err == nil {
		if in := b.findInst(c.Value); in != nil {
			return in
		}
	}
	if in := b.findInst(b.cfg.DefaultInst); in != nil {
		return in
	}
	return &b.cfg.Instances[0]
}

func eq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (b *bridge) authed(r *http.Request) bool {
	if c, err := r.Cookie(b.cookie); err == nil && eq(c.Value, b.cfg.Token) {
		return true
	}
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		return eq(strings.TrimSpace(ah[7:]), b.cfg.Token)
	}
	return false
}

func (b *bridge) setAuth(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: b.cookie, Value: b.cfg.Token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600,
	})
}

var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true, "Trailers": true,
	"Transfer-Encoding": true, "Upgrade": true, "Host": true, "Content-Length": true,
}

func copyHdr(dst http.Header, src http.Header) {
	for k, vs := range src {
		if hopByHop[k] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// proxy 原样转发。SSE 逐块 flush（不缓冲，否则手机上流式输出会变成"最后一次性出来"）。
func (b *bridge) proxy(w http.ResponseWriter, r *http.Request, in *Instance) {
	target := in.URL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "桥内部错误", http.StatusInternalServerError)
		return
	}
	copyHdr(req.Header, r.Header)
	req.Header.Set("X-Forwarded-For", r.RemoteAddr)
	req.Host = ""
	// 主程序的写请求守卫要求 Origin 的 host 是本机地址（挡 DNS rebinding / 跨站页面）。
	// 浏览器发给桥的是 http://<局域网IP>:8602，原样转过去会被判"不是本机同源"。
	// 桥是这台机器上该实例的唯一代理、且自己已经做完 token 鉴权，所以这里把 Origin
	// 换成上游自己的 origin —— 守卫的威胁模型没被削弱：恶意跨站页面连桥都进不来
	// （鉴权 cookie 是 SameSite=Lax，跨站 POST 不会带上），进不来就轮不到它写实例。
	if req.Header.Get("Origin") != "" {
		req.Header.Set("Origin", in.URL)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		log.Printf("[%s] 上游失败 %s %s: %v", in.ID, r.Method, r.URL.Path, err)
		http.Error(w, "上游不可达（"+in.ID+"）", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHdr(w.Header(), resp.Header)
	w.Header().Set("X-Frame-Options", "") // 桥同源嵌入，不需要；留着会让某些浏览器拒绝
	w.WriteHeader(resp.StatusCode)

	fl, canFlush := w.(http.Flusher)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") && canFlush {
		w.Header().Set("X-Accel-Buffering", "no")
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return // 手机断了：只断这条连接，上游 run 不受影响（实例侧已解耦）
				}
				fl.Flush()
			}
			if err != nil {
				return
			}
		}
	}
	_, _ = io.Copy(w, resp.Body)
}

const loginPage = `<!doctype html><meta charset=utf-8><meta name="viewport" content="width=device-width,initial-scale=1">
<title>OkHuman 局域网桥</title><style>body{font:16px system-ui;background:#14161a;color:#e6e6e6;padding:2rem;max-width:26rem;margin:auto}
input{width:100%%;padding:.7rem;font-size:1rem;background:#1e2228;color:#e6e6e6;border:1px solid #3a4048;border-radius:6px}
button{margin-top:.8rem;padding:.7rem 1.2rem;font-size:1rem;background:#2b6cb0;color:#fff;border:0;border-radius:6px}</style>
<h3>OkHuman 局域网桥</h3>%s
<form method="POST" action="/login"><input type="password" name="t" placeholder="token" autocomplete="off" autofocus>
<button type="submit">进入</button></form>`

func (b *bridge) handleLogin(w http.ResponseWriter, r *http.Request) {
	tok := ""
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		tok = r.FormValue("t")
	} else {
		tok = r.URL.Query().Get("t")
	}
	if tok == "" {
		// GET 是"来要表单"，200；POST 交空值是一次失败尝试，401（别把空提交当首次访问）
		code := http.StatusOK
		note := ""
		if r.Method == http.MethodPost {
			code = http.StatusUnauthorized
			note = "<p style='color:#e57373'>token 不能为空</p>"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		fmt.Fprintf(w, loginPage, note)
		return
	}
	if !eq(tok, b.cfg.Token) {
		log.Printf("登录失败 from %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, loginPage, "<p style='color:#e57373'>token 不对</p>")
		return
	}
	b.setAuth(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (b *bridge) handlePick(w http.ResponseWriter, r *http.Request) {
	if id := r.URL.Query().Get("inst"); id != "" {
		if in := b.findInst(id); in != nil {
			http.SetCookie(w, &http.Cookie{Name: instCookie, Value: in.ID, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 365 * 24 * 3600})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}
	cur := b.pickInst(r).ID
	var s strings.Builder
	s.WriteString(`<!doctype html><meta charset=utf-8><meta name="viewport" content="width=device-width,initial-scale=1">
<title>切换实例</title><style>body{font:16px system-ui;background:#14161a;color:#e6e6e6;padding:2rem;max-width:26rem;margin:auto}
a{display:block;padding:.8rem;margin:.5rem 0;background:#1e2228;border-radius:6px;color:#e6e6e6;text-decoration:none}
.on{background:#2b6cb0}</style><h3>切换实例</h3>`)
	for _, in := range b.cfg.Instances {
		on := ""
		if in.ID == cur {
			on = " class=on"
		}
		s.WriteString(fmt.Sprintf("<a%s href=\"/pick?inst=%s\">%s <span style='opacity:.6'>%s</span></a>", on, in.ID, in.ID, in.URL))
	}
	s.WriteString(`<a href="/">返回</a>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, s.String())
}

func (b *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/healthz": // 给 okmon / 排障用，不鉴权，不含任何会话内容
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"instances":%d}`, len(b.cfg.Instances))
		return
	case p == "/login":
		b.handleLogin(w, r)
		return
	case p == "/pick":
		if !b.authed(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		b.handlePick(w, r)
		return
	}

	if !b.authed(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	rl, ok := b.allow[p]
	if !ok {
		log.Printf("拒绝（不在白名单）%s %s", r.Method, p)
		http.Error(w, "不在白名单", http.StatusForbidden)
		return
	}
	okMethod := false
	for _, m := range rl {
		if m == r.Method {
			okMethod = true
			break
		}
	}
	if !okMethod {
		log.Printf("拒绝（方法）%s %s", r.Method, p)
		http.Error(w, "方法不允许", http.StatusForbidden)
		return
	}
	b.proxy(w, r, b.pickInst(r))
}

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	cp := filepath.Join(dir, "config.json")
	cfg, generated, err := loadCfg(cp)
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}
	if generated {
		if err := saveCfg(cp, cfg); err != nil {
			log.Fatalf("token 已生成但写不回 config.json（需要可写目录）: %v", err)
		}
		// 故意不把 token 打进日志：日志会外流，token 只落插件自己的文件
		log.Printf("已生成随机 token 并写入 %s（权限 600，日志里不重复）", cp)
	}
	b := newBridge(cfg)
	addr := fmt.Sprintf("%s:%d", cfg.Listen.Host, cfg.Listen.Port)
	if cfg.Listen.Host == "0.0.0.0" || cfg.Listen.Host == "" {
		log.Printf("⚠ 监听 %s = 整个局域网可达；只有本进程对外，实例仍只听 localhost", addr)
	}
	log.Printf("lan-bridge 启动 %s → 实例 %v（默认 %s）", addr,
		strings.Join(ids(cfg.Instances), ","), cfg.DefaultInst)
	srv := &http.Server{Addr: addr, Handler: b, ReadHeaderTimeout: 15 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func ids(in []Instance) []string {
	s := make([]string, 0, len(in))
	for _, x := range in {
		s = append(s, x.ID)
	}
	return s
}
