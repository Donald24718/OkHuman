package main

// 测试支持：把页面里的 JS 异常与 console 报错持续记到
// /tmp/okhuman-browser-<port>.errors（JSONL），供 browserctl errors 读。
//
// 为什么由 daemon 记而不是 ctl 记：ctl 是短连接，命令与命令之间
// 没人接事件；页面在两次动作之间抛的异常就永远丢了。
// 而"这页面前端有没有报错"恰恰是网页测试最要紧的一条断言。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/websocket"
)

func errorsFile(port int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("okhuman-browser-%d.errors", port))
}

type cdpTarget struct {
	Type                 string `json:"type"`
	ID                   string `json:"id"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func watchErrors(port int) {
	seen := map[string]bool{}
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
		if err == nil {
			var tabs []cdpTarget
			if json.NewDecoder(resp.Body).Decode(&tabs) == nil {
				for _, t := range tabs {
					if t.Type != "page" || t.WebSocketDebuggerURL == "" || seen[t.ID] {
						continue
					}
					seen[t.ID] = true
					go watchTab(t.WebSocketDebuggerURL, port)
				}
			}
			resp.Body.Close()
		}
		time.Sleep(time.Second)
	}
}

func watchTab(wsURL string, port int) {
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return
	}
	defer c.Close()
	for i, m := range []string{"Runtime.enable", "Log.enable", "Page.enable"} {
		c.WriteJSON(map[string]interface{}{"id": i + 1, "method": m})
	}
	f, err := os.OpenFile(errorsFile(port), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	seen := map[string]bool{}
	for {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Type             string         `json:"type"`
				Level            string         `json:"level"`
				Entry            map[string]any `json:"entry"`
				ExceptionDetails map[string]any `json:"exceptionDetails"`
				Args             []struct {
					Description string      `json:"description"`
					Value       interface{} `json:"value"`
					Type        string      `json:"type"`
				} `json:"args"`
			} `json:"params"`
		}
		if err := c.ReadJSON(&msg); err != nil {
			return
		}
		rec := map[string]string{"at": time.Now().Format(time.RFC3339), "tab": wsURL}
		_ = rec
		switch msg.Method {
		case "Runtime.exceptionThrown":
			d := msg.Params.ExceptionDetails
			rec["kind"] = "exception"
			rec["text"] = firstStr(d, "text") + " " + descOf(d["exception"])
			rec["where"] = fmt.Sprint(d["url"], ":", d["lineNumber"])
		case "Runtime.consoleAPICalled":
			if msg.Params.Type != "error" && msg.Params.Type != "warning" {
				continue
			}
			rec["kind"] = "console." + msg.Params.Type
			for _, a := range msg.Params.Args {
				rec["text"] += a.Description + fmt.Sprint(a.Value) + " "
			}
		case "Log.entryAdded":
			e := msg.Params.Entry
			if fmt.Sprint(e["level"]) != "error" {
				continue
			}
			rec["kind"] = "log.error"
			rec["text"] = fmt.Sprint(e["text"])
			rec["where"] = fmt.Sprint(e["url"], ":", e["lineNumber"])
		default:
			continue
		}
		// 同一个异常 chromium 会重放（重新 enable、切 tab 都会再来一遍）。
		// 写入侧就去重：否则 errors 里全是重复条目，真问题被埋掉。
		key := rec["kind"] + "|" + rec["text"]
		if seen[key] {
			continue
		}
		seen[key] = true
		b, _ := json.Marshal(rec)
		f.Write(append(b, '\n'))
	}
}

func firstStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func descOf(v any) string {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["description"].(string); ok {
			return s
		}
		if s, ok := m["value"].(string); ok {
			return s
		}
	}
	return ""
}
