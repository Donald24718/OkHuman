package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 修复验证 1：过期一次性任务启动补跑只触发 1 次（旧代码 2 次：BootCatchup + tick 各一次）
func TestBootCatchupOneShotSingleFire(t *testing.T) {
	now := time.Now().UnixMilli()
	st := newCronStore(t.TempDir(), 3)
	at := now - 60_000
	st.jobs = append(st.jobs, CronJob{ID: "t1", Name: "t1", Prompt: "p", At: &at, NextRunAt: at})
	var hits int32
	sc := NewScheduler(50, func(string) {})
	sc.Register([]CronInstance{{ID: "i1", URL: "http://127.0.0.1:1", Store: st,
		Handle: func(job CronJob) (int, error) {
			atomic.AddInt32(&hits, 1)
			time.Sleep(300 * time.Millisecond) // 模拟实例那一轮很久（入队不该等它）
			return 1, nil
		}}})
	sc.BootCatchup()
	tk := sc.start()
	time.Sleep(1500 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("过期一次性任务被触发 %d 次，应为 1（BootCatchup 双触发）", got)
	}
	tk.Stop()
}

// 修复验证 2：长时间暂停后 resume → next_run_at 落在未来（旧代码落在过去 → 立即触发）
func TestResumeStaleAnchor(t *testing.T) {
	st := newCronStore(t.TempDir(), 3)
	now := time.Now().UnixMilli()
	ev := int64(200)
	st.jobs = append(st.jobs, CronJob{ID: "t2", Name: "t2", Prompt: "p", EveryMs: &ev, LastRunAt: now - 650, NextRunAt: 0})
	if !st.resume("t2") {
		t.Fatal("resume 失败")
	}
	j := st.get("t2")
	now2 := time.Now().UnixMilli()
	if j.NextRunAt <= now2 {
		t.Fatalf("next_run_at=%d <= now=%d（resume 后会立即触发）", j.NextRunAt, now2)
	}
	if j.NextRunAt > now2+ev {
		t.Fatalf("next_run_at=%d 过远，应为 now+period 附近（%d）", j.NextRunAt, now2+ev)
	}
}

// 修复验证 3：并发 RunNow 同实例串行（CAS 占位）
// 修复验证 3（2026-10-22 改版）：只管发——入队不阻塞、不串行、不占锁。
// 旧实现用 CAS 忙等把同实例强制串行（maxActive 必须 ≤1）；改成 POST /enqueue 后
// 入队是瞬时动作，实例自己排队，所以这里断言的是**新**不变量：
// 并发触发不死锁、不丢任务，且返回的是入队回执而不是等待后的回复。
func TestRunNowFireAndForget(t *testing.T) {
	st := newCronStore(t.TempDir(), 3)
	now := time.Now().UnixMilli()
	ev := int64(60_000)
	st.jobs = append(st.jobs, CronJob{ID: "t3", Name: "t3", Prompt: "p", EveryMs: &ev, LastRunAt: now, NextRunAt: now + 60_000})
	var hits int32
	sc := NewScheduler(50, func(string) {})
	sc.Register([]CronInstance{{ID: "i3", URL: "http://127.0.0.1:1", Store: st,
		Handle: func(job CronJob) (int, error) {
			atomic.AddInt32(&hits, 1)
			return 7, nil // 入队立即返回位置，不模拟"等一轮"
		}}})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 30; i++ {
			sc.RunNow("i3", "t3")
		}
		close(done)
	}()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); sc.RunNow("i3", "t3") }()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("并发入队死锁（忙等锁删掉后不该再出现）")
	}
	wg.Wait() // 不 join 就会读到半截计数（第一版就是这假失败）
	if h := atomic.LoadInt32(&hits); h != 40 {
		t.Fatalf("触发次数=%d，期望 40（入队不该被串行吞掉）", h)
	}
	ok, msg, e := sc.RunNow("i3", "t3")
	if !ok || e != "" {
		t.Fatalf("RunNow 失败：%s", e)
	}
	if msg == "" || msg[:len("已入队")] != "已入队" {
		t.Errorf("应返回入队回执，实际 %q", msg)
	}
}

// 修复验证 4：cron.json 里负周期任务加载时跳过（旧代码每 tick 触发）
func TestLoadNegativeEverySkipped(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UnixMilli()
	f := `{"jobs":[{"id":"bad","name":"bad","prompt":"p","last_run_at":0,"next_run_at":` +
		itoa(now) + `,"every_ms":-100},{"id":"good","name":"good","prompt":"p","last_run_at":0,"next_run_at":` +
		itoa(now) + `,"every_ms":60000}],"runs":[]}`
	if err := os.WriteFile(filepath.Join(dir, "cron.json"), []byte(f), 0644); err != nil {
		t.Fatal(err)
	}
	st := newCronStore(dir, 3)
	for _, j := range st.jobs {
		if j.ID == "bad" {
			t.Fatal("加载了负周期任务（会每 tick 触发）")
		}
		if j.ID == "good" {
			return
		}
	}
	t.Fatal("good 任务丢失")
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// 2026-10-22 只管发：makeHandle 必须打 /enqueue（不是 /chat），拿到位置就返回。
// 这条锁住的是"别再退回同步等回复"——/chat 会等整轮，带来超时误判和长窗口竞态。
func TestMakeHandleUsesEnqueueNotChat(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"ok":true,"position":42,"running":false}`)
	}))
	defer srv.Close()
	h := makeHandle(srv.URL, "i1")
	pos, err := h(CronJob{ID: "j", Prompt: "该交周报了"})
	if err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	if gotPath != "/enqueue" {
		t.Errorf("打到 %s，期望 /enqueue（只管发，不等整轮）", gotPath)
	}
	if pos != 42 {
		t.Errorf("position=%d，期望 42", pos)
	}
	if !strings.Contains(gotBody, "该交周报了") {
		t.Errorf("prompt 没发出去：%s", gotBody)
	}
}
