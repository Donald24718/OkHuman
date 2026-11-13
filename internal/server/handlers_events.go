package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/inject"
	"okhuman/internal/persist"
	"okhuman/internal/types"
)

func (ap *App) handleEventsPoll(w http.ResponseWriter, r *http.Request) {
	s := -1
	if q := r.URL.Query().Get("since"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			s = n
		}
	}
	bs, latest, gap, _ := ap.state.Evt.takeSince(s, maxPollBatches)
	if bs == nil {
		bs = []eventBatch{}
	}
	ap.state.mu.Lock()
	running := ap.state.Agent.Lock.Running() || ap.state.ActiveDrain != nil
	ap.state.mu.Unlock()
	writeJSON(w, 200, map[string]interface{}{
		"batches": bs, "latest": latest, "gap": gap, "running": running,
	})
}

func (ap *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	conn := newSSE(w)
	if !conn.flushable {
		ap.state.logf("[sse] 响应不支持 Flush（中间件未转发？）：事件可能延迟送达")
	}
	ap.state.mu.Lock()
	queued := queuedTexts(ap.state.MessageQueue)
	qItems := copyQueue(ap.state.MessageQueue)
	running := a.Lock.Running() || ap.state.ActiveDrain != nil
	session := int(a.SessionNo.Load())
	bgPending := a.BG().Count()
	var roundFrames []string
	if running && ap.state.RoundBuf != nil && ap.state.RoundBuf.Round == session {
		roundFrames = append([]string(nil), ap.state.RoundBuf.Frames...)
	}
	ap.state.mu.Unlock()
	conn.write(fmt.Sprintf("event: hello\ndata: %s\n\n", jsonStr(map[string]interface{}{
		"session":      session,
		"running":      running,
		"queued":       queued,
		"queued_items": qItems,
		"bg_pending":   bgPending,
	})))
	// 重放：优先走事件缓冲（原子、含尚未发车的帧）；本轮开头已被窗口挤掉时
	// 回退 RoundBuf 整轮重放（保住"长轮中途重订也能补全本轮"的旧语义）
	bs, _, _, fromRoundStart := ap.state.Evt.takeSince(-1, 0)
	if len(bs) > 0 && fromRoundStart {
		conn.write(joinFrames(bs))
	} else {
		for _, f := range roundFrames {
			conn.write(f)
		}
	}
	cur := ap.state.Evt.latestSeq()
	tick := time.NewTicker(eventPollTick)
	defer tick.Stop()
	ping := time.NewTicker(5 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			conn.close()
			return
		case <-ping.C:
			conn.write(": ping\n\n") // 保活：外部消费者靠周期性数据判活
			continue
		case <-tick.C:
		}
		got, _, _, _ := ap.state.Evt.takeSince(cur, 0)
		if len(got) == 0 {
			continue
		}
		conn.write(joinFrames(got))
		cur = got[len(got)-1].Seq
	}
}

func (ap *App) handleStripInjects(w http.ResponseWriter, r *http.Request) {
	dir := ap.injectDirOf()
	files, _ := filepath.Glob(filepath.Join(dir, "inj-*.json"))
	stripped := make([]string, 0, len(files))
	failed := make([]string, 0)
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		if !injIDRe.MatchString(id) {
			continue
		}
		ok, err := inject.StripSidecar(dir, id)
		if err != nil {
			failed = append(failed, id+": "+err.Error())
		} else if ok {
			stripped = append(stripped, id)
		}
		ctxmgr.ClearResolverCacheEntry(id)
	}
	// 侧车字节变了，附件列表同步（否则侧栏显示的还是原图大小）
	ap.state.mu.Lock()
	for i := range ap.state.Injects {
		if ap.state.Injects[i].Bytes > 0 {
			ap.state.Injects[i].Bytes = inject.InjectFileSize(dir, ap.state.Injects[i].ID)
		}
	}
	n := len(ap.state.Injects)
	ap.state.mu.Unlock()
	if len(failed) > 0 {
		ap.state.logf("[inject] 手动剥离附件：成功 %d 个，失败 %d 个：%s", len(stripped), len(failed), strings.Join(failed, "; "))
	} else {
		ap.state.logf("[inject] 手动剥离附件 %d 个（会话登记 %d 个）→ 侧车回写文本提示，后续调用不再发原内容", len(stripped), n)
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "stripped": stripped, "count": len(stripped), "failed": failed})
}

// ---------- /reset：新会话 ----------

func (ap *App) injectInFlight(id string) bool {
	ap.state.mu.Lock()
	defer ap.state.mu.Unlock()
	return ap.state.injectInFlight[id]
}

func (ap *App) handleInject(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	b := tryReadJSON(r)
	var parts []types.ContentPart
	if b != nil {
		parts = validateContentParts(b["content"])
	}
	if parts == nil {
		writeJSON(w, 400, map[string]interface{}{"error": "content 须为非空 content parts 数组（OpenAI 格式：每个 part 为带非空 type 字段的 JSON 对象，如 {type:text,text} / {type:image_url,image_url:{url}} / {type:input_video,input_video:{data}}）"})
		return
	}
	dir := ap.injectDirOf()
	// 先无锁扫描（2026-09-14 审计修：旧实现在 state.mu 内调 sessionHasInjectRef
	// （取 cm.mu）→ 锁序 state.mu→cm.mu；而 compose 侧 resolve 失败回调是
	// cm.mu→state.mu → 反向锁序，并发 /inject 与在飞 run 可死锁。现在扫描
	// 全程不持 state.mu，两个方向都断）
	ap.state.mu.Lock()
	pending := append([]InjectInfo(nil), ap.state.Injects...)
	ap.state.mu.Unlock()
	// 只记下要摘除的 ID，不回写快照：并发的另一个 /inject 可能在这中间加过附件，
	// 回写快照会把别人刚加的那条抹掉（2026-11-13 第四轮 A2：16 路并发只剩 8 条）。
	dropped := make(map[string]bool)
	for _, x := range pending {
		if ap.injectInFlight(x.ID) || sessionHasInjectRef(a.CM(), x.ID) {
			continue
		}
		// 预算修复（2026-09-12 破案）：只清"会话里已无引用"的侧车（被压缩吃掉的附件留着就是永久占用）
		inject.DropInjectFile(dir, x.ID)
		dropped[x.ID] = true
	}
	ap.state.mu.Lock()
	ap.state.InjectSeq++ // 序号只增；列表的摘除按 ID 在同一次锁里做（A2）
	id := fmt.Sprintf("inj-%d", ap.state.InjectSeq)
	if ap.state.injectInFlight == nil {
		ap.state.injectInFlight = map[string]bool{}
	}
	ap.state.injectInFlight[id] = true // 在飞：别的并发请求这一轮不许扫掉它
	n := len(ap.state.Injects)
	overLimit := n > INJECT_MAX
	ap.state.mu.Unlock()
	if overLimit {
		fmt.Fprintf(os.Stderr, "[inject] 活跃附件 %d 超软上限 %d（引用仍在会话，保留不清）\n", n, INJECT_MAX)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": "inject 目录创建失败：" + err.Error()})
		return
	}
	// 侧车保持紧凑 JSON（compose 会读它，且下面要记字节数），但序列化错误不再吞掉。
	payload, err := json.Marshal(parts)
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": "侧车序列化失败：" + err.Error()})
		return
	}
	if err := persist.WritePath(filepath.Join(dir, id+".json"), string(payload)); err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": "侧车写入失败：" + err.Error()})
		return
	}
	var notePtr *string
	if b != nil {
		if note, ok := b["note"].(string); ok {
			notePtr = &note
		}
	}
	ap.state.mu.Lock()
	cur := ap.state.Injects
	if len(dropped) > 0 {
		next := make([]InjectInfo, 0, len(cur))
		for _, x := range cur {
			if !dropped[x.ID] {
				next = append(next, x)
			}
		}
		cur = next
	}
	ap.state.Injects = append(cur, InjectInfo{ID: id, Bytes: len(payload), CreatedAt: time.Now().UnixMilli(), Note: notePtr})
	ap.state.mu.Unlock()
	label := "附件 " + id
	if notePtr != nil && *notePtr != "" {
		label = *notePtr
	}
	a.CM().AddMessage(&types.RawEntry{
		Role: "user",
		Content: []types.ContentPart{
			types.TextPart("[okattach] " + label),
			types.RefPart(id),
		},
	})
	ap.state.mu.Lock()
	delete(ap.state.injectInFlight, id) // 已进会话，之后按引用判据清扫
	ap.state.mu.Unlock()
	writeJSON(w, 200, map[string]interface{}{"ok": true, "id": id})
}
