package server

// 手动压缩上下文 / 手动剥离全部注入附件（2026-10-18）。
// 这两个端点取代旧的"LLM 4xx 三级回退阶梯"：机器不再靠硬编码状态码猜附件坏了，
// 处置由人按 WebUI 按钮（重置会话右边两个）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"okhuman/internal/config"
	"okhuman/internal/llm"
	"okhuman/internal/types"
)

func TestStripInjectsAndManualCompress(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	cfg.Context.KeepRecentChars = 40 // 保留预算压到极小 → 老消息可压，手动压缩有活干
	fake := llm.NewFakeLLM([]llm.FakeStep{
		{Type: "text", Text: "收到附件"},
		{Type: "compress", Text: "手动压缩的总结", Deltas: []types.Delta{
			{Thinking: "先扫最旧一批"},
			{Text: "手动压缩的总结"},
		}},
	})
	a := CreateAgentState(cfg, fake, "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	ap := New(st, nil)
	srv := httptest.NewServer(ap)
	defer srv.Close()

	do := func(method, path string, body interface{}, out interface{}) int {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("%s %s 解码: %v", method, path, err)
			}
		}
		return resp.StatusCode
	}

	// 1) 注入一个假图片附件（侧车落盘 inj-1.json）
	var inj map[string]interface{}
	if code := do(http.MethodPost, "/inject", map[string]interface{}{"content": []map[string]interface{}{
		{"type": "text", "text": "看图"},
		{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64,dGVzdAo="}},
	}}, &inj); code != 200 {
		t.Fatalf("/inject code=%d body=%v", code, inj)
	}
	id, _ := inj["id"].(string)
	if id == "" {
		t.Fatalf("/inject 没返回 id: %v", inj)
	}
	side := filepath.Join(cfg.Data.Dir, "inject", id+".json")
	raw, err := os.ReadFile(side)
	if err != nil || !strings.Contains(string(raw), "image_url") {
		t.Fatalf("侧车应含 image_url，实际 %s err=%v", raw, err)
	}

	// 2) 手动剥离全部附件 → 侧车永久回写文本提示，compose 不再发原内容
	var strip map[string]interface{}
	if code := do(http.MethodPost, "/strip-injects", map[string]interface{}{}, &strip); code != 200 {
		t.Fatalf("/strip-injects code=%d body=%v", code, strip)
	}
	if cnt, _ := strip["count"].(float64); cnt < 1 {
		t.Fatalf("剥离数 = %v，期望 ≥1", strip["count"])
	}
	raw, err = os.ReadFile(side)
	if err != nil {
		t.Fatalf("侧车读取失败: %v", err)
	}
	if strings.Contains(string(raw), "image_url") {
		t.Errorf("侧车应已被回写为文本提示: %s", raw)
	}
	if !strings.Contains(string(raw), "已按用户手动指令剥离") {
		t.Errorf("侧车应含手动剥离提示文案: %s", raw)
	}
	parts := composeText(a.CM().Compose())
	if strings.Contains(parts, "image_url") {
		t.Errorf("compose 仍在发原附件: %s", parts)
	}
	if !strings.Contains(parts, "已按用户手动指令剥离") {
		t.Errorf("compose 应带手动剥离提示: %s", parts)
	}

	// 3) 攒几条消息（否则全在保留预算内，手动压缩无事可做）
	var chat map[string]interface{}
	if code := do(http.MethodPost, "/chat", map[string]interface{}{"message": "压缩前先说一句话，让会话里有足够多的老消息可供压缩"}, &chat); code != 200 {
		t.Fatalf("/chat code=%d body=%v", code, chat)
	}

	// 4) 手动压缩：不等阈值，立刻压最旧一批。
	//    同时订阅 /events —— 点按钮后聊天区必须像自动压缩那样流式出现压缩思考/正文
	//    （此前手动压缩没人订阅压缩事件，界面全程无输出）。
	evConn, err := srv.Client().Get(srv.URL + "/events")
	if err != nil {
		t.Fatalf("订阅 /events: %v", err)
	}
	defer evConn.Body.Close()
	br := bufio.NewReader(evConn.Body)
	seenDeltaThinking, sawCompressFrame := "", false
	// 兜底：读不到最终 compress 帧时别把测试挂死
	time.AfterFunc(20*time.Second, func() { _ = evConn.Body.Close() })

	// 压缩在另一个 goroutine 打（主线程读事件流），结果经 channel 取回——
	// 不再补打第二次 /compress（第二次可能因"已压完无可压"返回 did=false → 测试不稳）
	type compressRes struct {
		code int
		body map[string]interface{}
	}
	resCh := make(chan compressRes, 1)
	go func() {
		var c map[string]interface{}
		resCh <- compressRes{do(http.MethodPost, "/compress", map[string]interface{}{}, &c), c}
	}()

	var cmp map[string]interface{}
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			t.Fatalf("读 /events 帧失败（未看到 compress 帧）：%v；已见 delta 思考=%q", rerr, seenDeltaThinking)
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "event: ") {
			continue
		}
		evName := strings.TrimPrefix(line, "event: ")
		data, _ := br.ReadString('\n')
		if evName == "compress_delta" && strings.Contains(data, "先扫最旧一批") {
			seenDeltaThinking = strings.TrimSpace(data)
		}
		if evName == "compress" {
			sawCompressFrame = true
			if seenDeltaThinking == "" {
				t.Errorf("手动压缩期间没收到 compress_delta 思考流帧")
			}
			if !strings.Contains(data, "手动压缩的总结") {
				t.Errorf("compress 帧应带压缩后正文：%s", data)
			}
			break
		}
	}
	if !sawCompressFrame {
		t.Fatalf("未在 /events 上看到 compress 帧")
	}
	res := <-resCh
	if res.code != 200 {
		t.Fatalf("/compress code=%d body=%v", res.code, res.body)
	}
	cmp = res.body
	if did, _ := cmp["did"].(bool); !did {
		t.Fatalf("/compress 应真的压了: %v", cmp)
	}
	if got := a.CM().Session().Summary; got == nil || !strings.Contains(got.Content, "手动压缩的总结") {
		t.Errorf("summary 应为手动压缩的总结，实际 %+v", got)
	}
	// 手动压缩与自动压缩一样落两个文件：全文存档 + 总结文本（detail 里须报出路径）
	detail, _ := cmp["detail"].(string)
	rec, sum := "", ""
	for _, ln := range strings.Split(detail, "\n") {
		if strings.HasPrefix(ln, "全文存档：") {
			rec = strings.TrimPrefix(ln, "全文存档：")
		}
		if strings.HasPrefix(ln, "总结文本：") {
			sum = strings.TrimPrefix(ln, "总结文本：")
		}
	}
	for name, pth := range map[string]string{"全文存档": rec, "总结文本": sum} {
		if pth == "" {
			t.Fatalf("detail 未报出%s路径：%s", name, detail)
		}
		if _, err := os.Stat(pth); err != nil {
			t.Errorf("%s 文件不存在：%s", name, pth)
		}
	}
	if b, err := os.ReadFile(rec); err != nil || !strings.Contains(string(b), "触发=压缩") {
		t.Errorf("全文存档头应标 触发=压缩：%v %s", err, b)
	}

	// 5) 重置会话：旧会话全文自动落盘（重置会整体换掉 Manager，不存就无处可寻）
	var rst map[string]interface{}
	if code := do(http.MethodPost, "/reset", map[string]interface{}{}, &rst); code != 200 {
		t.Fatalf("/reset code=%d body=%v", code, rst)
	}
	rp, _ := rst["record"].(string)
	if rp == "" {
		t.Fatalf("/reset 应返回旧会话全文路径：%v", rst)
	}
	b, err := os.ReadFile(rp)
	if err != nil {
		t.Fatalf("读重置全文失败: %v", err)
	}
	if !strings.Contains(string(b), "触发=重置会话") || !strings.Contains(string(b), "压缩前先说一句话") {
		t.Errorf("重置全文应含触发标记与旧消息原文：%s", b)
	}
	if n, _ := rst["record_msgs"].(float64); n < 1 {
		t.Errorf("record_msgs = %v", rst["record_msgs"])
	}
}

// composeText 把 compose 结果的全部消息内容摊平成可断言的字符串
func composeText(msgs []types.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if ps := types.AsParts(m.Content); ps != nil {
			for _, p := range ps {
				b.WriteString(p.Type + " " + p.Text + " " + p.Ref + " " + string(p.Raw) + " ")
			}
		} else {
			b.WriteString(types.AsString(m.Content) + " ")
		}
	}
	return b.String()
}
