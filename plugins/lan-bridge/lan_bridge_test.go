package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-not-a-secret"

// 两个假实例，各自能认出自己是谁
func fakeInst(name string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/slow-sse") { // 不在白名单，桥会先挡掉；这里给直连测试用
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			fmt.Fprintf(w, "event: a\n\ndata:1\n\n")
			f.Flush()
			time.Sleep(400 * time.Millisecond)
			fmt.Fprintf(w, "data:2\n\n")
			f.Flush()
			return
		}
		fmt.Fprintf(w, "%s:%s", name, r.URL.Path)
	}))
}

func newTestBridge(t *testing.T, a, b *httptest.Server, allowCfgWrite bool) *httptest.Server {
	t.Helper()
	cfg := &Cfg{
		Listen:        ListenCfg{Host: "127.0.0.1", Port: 0},
		Token:         testToken,
		Instances:     []Instance{{ID: "A", URL: a.URL}, {ID: "B", URL: b.URL}},
		DefaultInst:   "A",
		AllowCfgWrite: allowCfgWrite,
	}
	return httptest.NewServer(newBridge(cfg))
}

func authedClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	// 不自动跟 302：要断言登录本身返回的是 302，而不是跟到 / 之后的 200
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Post(ts.URL+"/login", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("空 token 登录应 401，实得 %d", resp.StatusCode)
	}
	form := strings.NewReader("t=" + testToken)
	resp, err = c.Post(ts.URL+"/login", "application/x-www-form-urlencoded", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("正确 token 登录应 302，实得 %d", resp.StatusCode)
	}
	return c
}

func TestUnauthenticatedIsRedirectedAndUpstreamUntouched(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := c.Get(ts.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/login" {
		t.Fatalf("未登录应 302 到 /login，实得 %d / %q", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestLoginThenProxy(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()

	c := authedClient(t, ts)
	// 多上游时人也要先"落地"：GET / 给默认实例并把选择钉进 cookie，之后页面的数据请求才通
	r, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(r); got != "A:/" {
		t.Fatalf("落地页应给默认实例 A，实得 %q (code %d)", got, r.StatusCode)
	}
	r.Body.Close()
	r2, err := c.Get(ts.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if got := readAll(r2); got != "A:/history" {
		t.Fatalf("落地后应转发到默认实例 A，实得 %q (code %d)", got, r2.StatusCode)
	}
}

func TestWrongTokenNoCookie(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()

	r, err := ts.Client().Post(ts.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader("t=nope"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401，实得 %d", r.StatusCode)
	}
	for _, ck := range r.Cookies() {
		if ck.Name == "okh_bridge" {
			t.Fatal("错 token 却发了鉴权 cookie")
		}
	}
}

func TestBearerHeaderAuth(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.AddCookie(&http.Cookie{Name: instCookie, Value: "A"}) // 多上游必须点名；这条测的是鉴权
	r, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if got := readAll(r); got != "A:/status" {
		t.Fatalf("Bearer 鉴权应可用，实得 %q (code %d)", got, r.StatusCode)
	}
}

func TestWhitelistDeniesUnknownPathAndUnlistedMethod(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()
	c := authedClient(t, ts)

	for _, p := range []string{"/admin", "/proc", "/config/../../x", "/strip-injects"} {
		r, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		code := r.StatusCode
		r.Body.Close()
		if p == "/strip-injects" {
			// 在白名单但只允许 POST → GET 应 405/403，不能放行
			if code == http.StatusOK {
				t.Fatalf("GET /strip-injects 不该放行")
			}
			continue
		}
		if code != http.StatusForbidden {
			t.Fatalf("%s 应 403，实得 %d", p, code)
		}
	}

	// POST /config 默认拒绝（手机上不能改实例配置）
	r, err := c.Post(ts.URL+"/config", "application/json", strings.NewReader(`{"patch":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	code := r.StatusCode
	r.Body.Close()
	if code != http.StatusForbidden {
		t.Fatalf("POST /config 默认应 403，实得 %d", code)
	}
}

// 白名单必须覆盖 webui 真正发的每一个端点：漏一个就是那个页面在手机上坏掉
// （实测漏 /prompts/file 导致提示词页 403）
func TestWebuiEndpointsAllWhitelisted(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()
	c := authedClient(t, ts)
	// 真实浏览器顺序：登录 → /pick 点一次实例（之后 cookie 带着走）→ 各页面端点
	if r, err := c.Get(ts.URL + "/pick?inst=A"); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}
	gets := []string{"/", "/marked.min.js", "/sanitize.js", "/health", "/status", "/queue",
		"/history", "/config", "/prompts", "/prompts/file", "/attachments", "/events/poll"}
	posts := []string{"/enqueue", "/stop", "/reset", "/compress", "/strip-injects", "/inject",
		"/prompts/file", "/prompts/reload"}
	for _, p := range gets {
		r, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		code := r.StatusCode
		r.Body.Close()
		if code == http.StatusForbidden {
			t.Fatalf("webui 会用 GET %s，不该被白名单挡", p)
		}
		if code == http.StatusBadRequest {
			t.Fatalf("GET %s 被要求点名实例（400）：页面选过一次之后不该再遇到，说明浏览器流程断了", p)
		}
	}
	for _, p := range posts {
		r, err := c.Post(ts.URL+p, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		code := r.StatusCode
		r.Body.Close()
		if code == http.StatusForbidden {
			t.Fatalf("webui 会用 POST %s，不该被白名单挡", p)
		}
	}
}

func TestAllowConfigWriteOptIn(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, true)
	defer ts.Close()
	c := authedClient(t, ts)
	c.Jar.SetCookies(mustURL(t, ts.URL), []*http.Cookie{{Name: instCookie, Value: "A"}}) // 多上游：点名要改谁

	r, err := c.Post(ts.URL+"/config", "application/json", strings.NewReader(`{"patch":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("开了 allow_config_write 应放行，实得 %d", r.StatusCode)
	}
}

func TestInstanceCookieSwitchesUpstream(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()
	c := authedClient(t, ts)

	// 先切到 B
	r, err := c.Get(ts.URL + "/pick?inst=B")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()

	r, err = c.Get(ts.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if got := readAll(r); got != "B:/history" {
		t.Fatalf("切实例后应打到 B，实得 %q", got)
	}
}

// 认不出的 inst：2026-11-11 起不再静默退回默认（错的形态是"回信来自另一个 agent"，看不见），改为 400。
func TestUnknownInstCookieIsRejected(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()
	c := authedClient(t, ts)
	c.Jar.SetCookies(mustURL(t, ts.URL), []*http.Cookie{{Name: instCookie, Value: "不存在的实例"}})

	r, err := c.Get(ts.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("认不出的实例 cookie 应 400（不静默落默认），实得 %q (code %d)", readAll(r), r.StatusCode)
	}
}

// 关键：SSE 必须逐块 flush，不能被桥攒完再给——否则手机上流式变成"最后一次性出"
func TestSSEIsFlushedIncrementally(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	// 把 A 的 URL 指到一个会慢速推流的假实例
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "data:1\n\n")
		f.Flush()
		time.Sleep(500 * time.Millisecond)
		fmt.Fprint(w, "data:2\n\n")
		f.Flush()
	}))
	defer slow.Close()

	cfg := &Cfg{
		Listen:    ListenCfg{Host: "127.0.0.1"},
		Token:     testToken,
		Instances: []Instance{{ID: "S", URL: slow.URL}},
	}
	ts := httptest.NewServer(newBridge(cfg))
	defer ts.Close()
	c := authedClient(t, ts)

	r, err := c.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	br := bufio.NewReader(r.Body)
	start := time.Now()
	first, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	d1 := time.Since(start)
	if !strings.Contains(first, "data:1") {
		t.Fatalf("第一帧应为 data:1，实得 %q", first)
	}
	if d1 > 300*time.Millisecond {
		t.Fatalf("第一帧等了 %v —— 说明桥在缓冲，没逐块 flush", d1)
	}
	br.ReadString('\n')
	rest, _ := br.ReadString('\n')
	if !strings.Contains(rest, "data:2") {
		t.Fatalf("第二帧应为 data:2，实得 %q", rest)
	}
}

func TestUpstreamUnreachableIs502(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	cfg := &Cfg{
		Token:     testToken,
		Instances: []Instance{{ID: "dead", URL: "http://127.0.0.1:1"}}, // 必然连不上
	}
	ts := httptest.NewServer(newBridge(cfg))
	defer ts.Close()
	c := authedClient(t, ts)

	r, err := c.Get(ts.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游不可达应 502，实得 %d", r.StatusCode)
	}
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()
	r, err := (&http.Client{}).Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK || !strings.Contains(readAll(r), `"ok":true`) {
		t.Fatalf("/healthz 应无鉴权返回 ok")
	}
}

// token 为空 → 生成随机值、写回 config.json、权限 600、日志里不出现
func TestTokenGeneratedAndPersisted(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "config.json")
	cfg, gen, err := loadCfg(cp)
	if err != nil {
		t.Fatal(err)
	}
	if !gen || len(cfg.Token) < 16 {
		t.Fatalf("应生成 token，实得 gen=%v len=%d", gen, len(cfg.Token))
	}
	if err := saveCfg(cp, cfg); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(cp)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config.json 权限应 600，实得 %o", st.Mode().Perm())
	}
	// 再读一次：token 必须稳定（不能每次重启都换，否则手机 cookie 全废）
	cfg2, gen2, err := loadCfg(cp)
	if err != nil {
		t.Fatal(err)
	}
	if gen2 || cfg2.Token != cfg.Token {
		t.Fatalf("第二次读不该重新生成 token")
	}
}

// 主程序只认本机同源 Origin；桥必须把浏览器的 Origin 换成上游自己的 origin，
// 否则手机上所有写操作（发消息）都会 403「不是本机同源」。
func TestOriginRewrittenToUpstream(t *testing.T) {
	var gotOrigin, gotHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrigin, gotHost = r.Header.Get("Origin"), r.Host
		fmt.Fprint(w, "ok")
	}))
	defer up.Close()
	cfg := &Cfg{Token: testToken, Instances: []Instance{{ID: "U", URL: up.URL}}}
	ts := httptest.NewServer(newBridge(cfg))
	defer ts.Close()
	c := authedClient(t, ts)

	req, _ := http.NewRequest("POST", ts.URL+"/enqueue", strings.NewReader(`{"message":"x"}`))
	// 手机浏览器真实发的 Origin（用 RFC 5737 TEST-NET 地址，不写真实局域网地址）
	req.Header.Set("Origin", "http://192.0.2.10:8602")
	req.Header.Set("Content-Type", "application/json")
	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if gotOrigin != up.URL {
		t.Fatalf("上游看到的 Origin 应为 %s，实得 %q", up.URL, gotOrigin)
	}
	if !strings.HasPrefix(gotHost, "127.0.0.1:") {
		t.Fatalf("Host 应是上游地址（否则主程序判 DNS rebinding），实得 %q", gotHost)
	}

	// 不带 Origin 的客户端（curl/脚本/插件）不该被凭空加上 Origin
	req2, _ := http.NewRequest("GET", ts.URL+"/history", nil)
	r2, err := c.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if gotOrigin != "" {
		t.Fatalf("客户端没发 Origin 时不该伪造，实得 %q", gotOrigin)
	}
}

// 多上游桥：没点名的机器请求必须响（400），点错也响，点名才转给那一个。
// 这条是为了堵掉"消息发出去、回信来自另一个 agent"这种看不见的错。
func TestMultiUpstreamRequiresExplicitInstance(t *testing.T) {
	a, b := fakeInst("A"), fakeInst("B")
	defer a.Close()
	defer b.Close()
	ts := newTestBridge(t, a, b, false)
	defer ts.Close()
	c := authedClient(t, ts)

	for _, p := range []string{"/status", "/history", "/queue"} {
		r, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		code := r.StatusCode
		r.Body.Close()
		if code != http.StatusBadRequest {
			t.Fatalf("%s 未点名实例应 400，实得 %d", p, code)
		}
	}

	req, _ := http.NewRequest("GET", ts.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.AddCookie(&http.Cookie{Name: instCookie, Value: "B"})
	r, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(r); got != "B:/status" {
		t.Fatalf("点名 B 应打到 B，实得 %q", got)
	}
	r.Body.Close()

}

// 只挂一个上游时不必点名（没得可选），照旧转过去。
func TestSingleUpstreamNeedsNoCookie(t *testing.T) {
	up := fakeInst("U")
	defer up.Close()
	cfg := &Cfg{Token: testToken, Instances: []Instance{{ID: "U", URL: up.URL}}}
	ts := httptest.NewServer(newBridge(cfg))
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	r, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(r); got != "U:/status" {
		t.Fatalf("单上游不该要求点名，实得 %q (code %d)", got, r.StatusCode)
	}
	r.Body.Close()
}

func readAll(r *http.Response) string {
	x, _ := io.ReadAll(r.Body)
	return string(x)
}

func mustURL(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
