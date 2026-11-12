package main

// 跨机走桥时的两把钥匙：Bearer 认证、okh_inst 选上游。
// 多上游的桥现在要求显式点名（缺了就 400），所以这里要钉住"写了 inst 就一定带上 cookie"。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func echoHeaders(t *testing.T, p *Peer) (auth, cookie string, code int) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
		w.Header().Set("X-Echo-Cookie", r.Header.Get("Cookie"))
		io.WriteString(w, "ok")
	}))
	defer ts.Close()
	req, err := http.NewRequest("GET", ts.URL+"/status", nil)
	if err != nil {
		t.Fatalf("建请求失败：%v", err)
	}
	p.auth(req)
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.Header.Get("X-Echo-Auth"), res.Header.Get("X-Echo-Cookie"), res.StatusCode
}

func TestPeerAuthSendsBothKeysWhenInstSet(t *testing.T) {
	p := Peer{Name: "x", Token: "tok-1", Inst: "8452"}
	auth, cookie, _ := echoHeaders(t, &p)
	if auth != "Bearer tok-1" {
		t.Errorf("Bearer 不对：%q", auth)
	}
	if cookie != "okh_inst=8452" {
		t.Errorf("缺 okh_inst cookie：%q", cookie)
	}
}

func TestPeerAuthOmitsCookieWhenNoInst(t *testing.T) {
	p := Peer{Name: "x", Token: "tok-1"}
	auth, cookie, _ := echoHeaders(t, &p)
	if auth != "Bearer tok-1" {
		t.Errorf("Bearer 不对：%q", auth)
	}
	if cookie != "" {
		t.Errorf("没写 inst 就不该带 cookie：%q", cookie)
	}
}

// 直连（不经桥）的 peer：两把钥匙都不该出现
func TestPeerAuthDirectPeerSendsNothing(t *testing.T) {
	p := Peer{Name: "x"}
	auth, cookie, _ := echoHeaders(t, &p)
	if auth != "" || cookie != "" {
		t.Errorf("直连 peer 却带了凭据：auth=%q cookie=%q", auth, cookie)
	}
}
