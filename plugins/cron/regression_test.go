package main

import (
	"os"
	"path/filepath"
	"strconv"
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
		Handle: func(job CronJob) (string, error) {
			atomic.AddInt32(&hits, 1)
			time.Sleep(300 * time.Millisecond) // 模拟慢 /chat，给 tick 撞进来的窗口
			return "ok", nil
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
func TestRunNowSerialization(t *testing.T) {
	st := newCronStore(t.TempDir(), 3)
	now := time.Now().UnixMilli()
	ev := int64(60_000)
	st.jobs = append(st.jobs, CronJob{ID: "t3", Name: "t3", Prompt: "p", EveryMs: &ev, LastRunAt: now, NextRunAt: now + 60_000})
	var active, maxActive int32
	sc := NewScheduler(50, func(string) {})
	sc.Register([]CronInstance{{ID: "i3", URL: "http://127.0.0.1:1", Store: st,
		Handle: func(job CronJob) (string, error) {
			c := atomic.AddInt32(&active, 1)
			for {
				m := atomic.LoadInt32(&maxActive)
				if c <= m || atomic.CompareAndSwapInt32(&maxActive, m, c) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			return "ok", nil
		}}})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 30; i++ {
			sc.RunNow("i3", "t3")
		}
		close(done)
	}()
	for i := 0; i < 10; i++ {
		go func() { sc.RunNow("i3", "t3") }()
	}
	<-done
	if m := atomic.LoadInt32(&maxActive); m > 1 {
		t.Fatalf("同实例并发触发（maxActive=%d），串行保证被打破", m)
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
