package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 测试上行只走生产那条路：POST /enqueue → 等这一轮收口 → 从 /history 取正文。
//（同步一问一答的 POST /chat 已删除：生产里没有调用方，测试也不该守着一条
// 没人走的路径。2026-11-12）

// runTurnErr 发消息并等收口，返回本轮最后一条 assistant 正文；错误经返回值传出，
// 因此在 goroutine 里也能用（t.Fatal 只能主 goroutine 调）。
func runTurnErr(srv *httptest.Server, msg string) (string, error) {
	b, _ := json.Marshal(map[string]string{"message": msg})
	resp, err := srv.Client().Post(srv.URL+"/enqueue", "application/json", bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("/enqueue: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("/enqueue code=%d", resp.StatusCode)
	}
	if err := waitTurn(srv); err != nil {
		return "", err
	}
	return lastAssistantReply(srv)
}

// runTurn 测试主 goroutine 用的版本（失败即 t.Fatal）。
func runTurn(t *testing.T, srv *httptest.Server, msg string) string {
	t.Helper()
	got, err := runTurnErr(srv, msg)
	if err != nil {
		t.Fatalf("runTurn(%q): %v", msg, err)
	}
	return got
}

// waitTurn 等 /status.running 落回 false。
func waitTurn(srv *httptest.Server) error {
	deadline := time.Now().Add(90 * time.Second) // -race 下慢几倍，别把慢当卡死
	for {
		var st map[string]interface{}
		if err := getJSONURL(srv, "/status", &st); err != nil {
			return err
		}
		if running, _ := st["running"].(bool); !running {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("等 run 收口超时")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitTurnDone 主 goroutine 版本。
func waitTurnDone(t *testing.T, srv *httptest.Server) {
	t.Helper()
	if err := waitTurn(srv); err != nil {
		t.Fatal(err)
	}
}

// lastAssistantReply 从 /history 取最后一条 assistant 正文（兼容 content 两种形状）。
func lastAssistantReply(srv *httptest.Server) (string, error) {
	var h struct {
		Entries []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"entries"`
	}
	if err := getJSONURL(srv, "/history", &h); err != nil {
		return "", err
	}
	for i := len(h.Entries) - 1; i >= 0; i-- {
		e := h.Entries[i]
		if e.Role != "assistant" {
			continue
		}
		if s := textOfContent(e.Content); s != "" {
			return s, nil
		}
	}
	return "", errors.New("/history 里没有 assistant 正文")
}

// textOfContent 兼容 content 是字符串或多模态数组。
func textOfContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// short 报错用：截到 300 字节。
func short(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300]
}

func getJSONURL(srv *httptest.Server, path string, out interface{}) error {
	r, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("GET %s 解码: %w（body=%s）", path, err, b)
	}
	return nil
}
