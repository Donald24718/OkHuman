// okhuman-cron：OkHuman 定时任务插件（独立进程，Go 移植自 OkHuman_p/cron，2026-09-14）
//
// 职责：管理并触发一个或多个 OkHuman 实例的定时任务。
//   - 每个实例独立维护自己的 CronStore（落 <插件>/data/<inst-id>/cron.json，实时原子写）
//   - 到点把任务 prompt 作为 user 消息 POST 到该实例的 /chat（同步等完整 reply）
//   - 自带 HTTP 管理 API（/cron/*，按 inst 区分实例）+ 独立管理页（/）
//
// 与 OkHuman 零代码耦合：只依赖 HTTP 接口 POST /chat { message } → { reply }。
// 配置：config.json（插件目录）。运行：./okhuman-cron（前台常驻）。
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------- 类型（同 OkHuman_p/cron/types.ts） ----------

type CronJob struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prompt     string `json:"prompt"`
	EveryMs    *int64 `json:"every_ms"`
	At         *int64 `json:"at"`
	LastRunAt  int64  `json:"last_run_at"`
	NextRunAt  int64  `json:"next_run_at"`
	CreatedAt  int64  `json:"created_at"`
	Catchup    int    `json:"catchup"`
}

type CronJobInput struct {
	Name    string
	Prompt  string
	EveryMs *int64
	At      *int64
}

type CronRunRecord struct {
	JobID      string `json:"job_id"`
	Name       string `json:"name"`
	At         int64  `json:"at"`
	OK         bool   `json:"ok"`
	ReplyChars int    `json:"reply_chars"`
	DurationMs int64  `json:"duration_ms"`
	Catchup    bool   `json:"catchup"`
}

type CronFileData struct {
	Jobs []CronJob         `json:"jobs"`
	Runs []CronRunRecord   `json:"runs"`
}

// 任务展示视图（补人类可读的下次触发时间；与核心 cronJobView 同形）
type cronJobView struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Prompt      string  `json:"prompt"`
	Kind        string  `json:"kind"`
	EveryMs     *int64  `json:"every_ms"`
	At          *int64  `json:"at"`
	NextRunAt   int64   `json:"next_run_at"`
	NextRunIso  *string `json:"next_run_iso"`
	LastRunAt   int64   `json:"last_run_at"`
	Paused      bool    `json:"paused"`
	Catchup     int     `json:"catchup"`
	CreatedAt   int64   `json:"created_at"`
}

func makeView(j CronJob) cronJobView {
	kind := "every"
	if j.At != nil {
		kind = "once"
	}
	var iso *string
	if j.NextRunAt > 0 {
		s := time.UnixMilli(j.NextRunAt).UTC().Format("2006-01-02T15:04:05.000Z")
		iso = &s
	}
	return cronJobView{
		ID: j.ID, Name: j.Name, Prompt: j.Prompt, Kind: kind,
		EveryMs: j.EveryMs, At: j.At, NextRunAt: j.NextRunAt, NextRunIso: iso,
		LastRunAt: j.LastRunAt,
		Paused:  j.At == nil && j.NextRunAt == 0,
		Catchup: j.Catchup, CreatedAt: j.CreatedAt,
	}
}

// ---------- 配置（同 lib.ts parseConfig：缺 instances[] / 项不合法 / id 重复 → 抛错） ----------

type InstanceCfg struct {
	ID  string
	URL string
}

type PluginConfig struct {
	Host           string
	Port           int
	TickMs         int
	CatchupMax     int
	ChatTimeoutSec int
	DataDir        string
	Instances      []InstanceCfg
}

func parseConfig(root string) PluginConfig {
	raw, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		fatalf("config.json 读取失败: %v", err)
	}
	var f map[string]interface{}
	if err := json.Unmarshal(raw, &f); err != nil {
		fatalf("config.json 解析失败: %v", err)
	}
	rawDataDir, _ := f["data_dir"].(string)
	if rawDataDir == "" {
		rawDataDir = "data"
	}
	dataDir := rawDataDir
	if !filepath.IsAbs(dataDir) {
		dataDir = filepath.Join(root, dataDir)
	}
	cfg := PluginConfig{Host: "127.0.0.1", Port: 8601, TickMs: 1000, CatchupMax: 3, ChatTimeoutSec: 600, DataDir: dataDir}
	if server, ok := f["server"].(map[string]interface{}); ok {
		if h, ok := server["host"].(string); ok {
			cfg.Host = h
		}
		if p, ok := server["port"].(float64); ok {
			cfg.Port = int(p)
		}
	}
	if v, ok := f["tick_ms"].(float64); ok {
		cfg.TickMs = int(v)
	}
	if v, ok := f["catchup_max"].(float64); ok {
		cfg.CatchupMax = int(v)
	}
	if v, ok := f["chat_timeout_sec"].(float64); ok && v > 0 {
		cfg.ChatTimeoutSec = int(v)
	}
	if cfg.TickMs <= 0 {
		fatalf("tick_ms 需为正整数（毫秒）")
	}
	insts, ok := f["instances"].([]interface{})
	if !ok || len(insts) == 0 {
		fatalf("config.json 缺 instances[]（至少一项 { id, url }）")
	}
	seen := map[string]bool{}
	for _, it := range insts {
		m, ok := it.(map[string]interface{})
		if !ok {
			fatalf("instances[] 每项需 { id: 非空字符串, url: http(s) 地址 }")
		}
		id, _ := m["id"].(string)
		url, _ := m["url"].(string)
		if id == "" || url == "" || !strings.HasPrefix(url, "http") {
			b, _ := json.Marshal(it)
			fatalf("instances[] 每项需 { id: 非空字符串, url: http(s) 地址 }：" + string(b))
		}
		if seen[id] {
			fatalf("instances[].id 重复：" + id)
		}
		seen[id] = true
		cfg.Instances = append(cfg.Instances, InstanceCfg{ID: id, URL: url})
	}
	return cfg
}

// ---------- 落盘（同 persist.ts：原子写 tmp+rename；读端永远看到完整文件） ----------

func atomicWriteFile(file, data string) {
	tmp := file + ".tmp"
	os.WriteFile(tmp, []byte(data), 0o644)
	os.Rename(tmp, file)
}

// 加载定时任务 + 执行记录（无文件/损坏 → 空；逐条校验，坏条目丢弃不连累好条目）
func loadCronFile(agentDir string) CronFileData {
	d := CronFileData{Jobs: []CronJob{}, Runs: []CronRunRecord{}}
	s, err := os.ReadFile(filepath.Join(agentDir, "cron.json"))
	if err != nil {
		return d
	}
	var raw struct {
		Jobs []map[string]interface{} `json:"jobs"`
		Runs []map[string]interface{} `json:"runs"`
	}
	if err := json.Unmarshal(s, &raw); err != nil {
		return d
	}
	for _, j := range raw.Jobs {
		id, _ := j["id"].(string)
		name, _ := j["name"].(string)
		prompt, _ := j["prompt"].(string)
		lastRun, ok1 := j["last_run_at"].(float64)
		nextRun, ok2 := j["next_run_at"].(float64)
		var everyMs, at *float64
		if v, ok := j["every_ms"].(float64); ok {
			everyMs = &v
		}
		if v, ok := j["at"].(float64); ok {
			at = &v
		}
		if id == "" || name == "" || prompt == "" || !ok1 || !ok2 || (everyMs == nil && at == nil) {
			continue
		}
		if everyMs != nil && *everyMs <= 0 {
			continue // 损坏的周期（≤0）→ 跳过该任务（否则每 tick 触发）
		}
		job := CronJob{ID: id, Name: name, Prompt: prompt, LastRunAt: int64(lastRun), NextRunAt: int64(nextRun)}
		if v, ok := j["created_at"].(float64); ok {
			job.CreatedAt = int64(v)
		}
		if v, ok := j["catchup"].(float64); ok {
			job.Catchup = int(v)
		}
		if everyMs != nil {
			v := int64(*everyMs)
			job.EveryMs = &v
		}
		if at != nil {
			v := int64(*at)
			job.At = &v
		}
		d.Jobs = append(d.Jobs, job)
	}
	for _, r := range raw.Runs {
		jobID, _ := r["job_id"].(string)
		name, _ := r["name"].(string)
		at, ok1 := r["at"].(float64)
		ok, ok2 := r["ok"].(bool)
		replyChars, ok3 := r["reply_chars"].(float64)
		duration, ok4 := r["duration_ms"].(float64)
		if jobID == "" || name == "" || !ok1 || !ok2 || !ok3 || !ok4 {
			continue
		}
		rec := CronRunRecord{JobID: jobID, Name: name, At: int64(at), OK: ok, ReplyChars: int(replyChars), DurationMs: int64(duration)}
		if c, isBool := r["catchup"].(bool); isBool {
			rec.Catchup = c
		}
		d.Runs = append(d.Runs, rec)
	}
	return d
}

// ---------- CronStore：每实例一份（同 scheduler.ts） ----------

const maxRuns = 20 // 执行记录保留最近 N 条（随 cron.json 持久化）

type CronStore struct {
	mu         sync.Mutex
	agentDir   string
	jobs       []CronJob
	runs       []CronRunRecord
	maxCatchup int
}

func newCronStore(agentDir string, maxCatchup int) *CronStore {
	os.MkdirAll(agentDir, 0o755) // 原子写前目录必须存在
	loaded := loadCronFile(agentDir)
	if len(loaded.Runs) > maxRuns {
		loaded.Runs = loaded.Runs[len(loaded.Runs)-maxRuns:]
	}
	return &CronStore{agentDir: agentDir, jobs: loaded.Jobs, runs: loaded.Runs, maxCatchup: maxCatchup}
}

func (s *CronStore) file() string { return filepath.Join(s.agentDir, "cron.json") }

func (s *CronStore) save(data CronFileData) {
	b, _ := json.Marshal(data)
	atomicWriteFile(s.file(), string(b))
}

func (s *CronStore) snapshot() CronFileData {
	return CronFileData{Jobs: s.jobs, Runs: s.runs}
}

func (s *CronStore) saveJobs() {
	s.save(s.snapshot())
}

func (s *CronStore) list() []CronJob {
	out := make([]CronJob, len(s.jobs))
	copy(out, s.jobs)
	return out
}

func (s *CronStore) recentRuns(limit int) []CronRunRecord {
	if len(s.runs) > limit {
		return append([]CronRunRecord{}, s.runs[len(s.runs)-limit:]...)
	}
	return append([]CronRunRecord{}, s.runs...)
}

func (s *CronStore) recordRun(r CronRunRecord) {
	s.runs = append(s.runs, r)
	if len(s.runs) > maxRuns {
		s.runs = s.runs[len(s.runs)-maxRuns:]
	}
	s.saveJobs()
}

func (s *CronStore) get(id string) *CronJob {
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			return &s.jobs[i]
		}
	}
	return nil
}

func randID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b) // 8 位十六进制（TS: randomUUID().slice(0,8) 同形）
}

// 创建任务：循环与单次二选一；单次必须落在未来
func (s *CronStore) add(input CronJobInput) (CronJob, error) {
	now := time.Now().UnixMilli()
	if (input.EveryMs == nil || *input.EveryMs == 0) && (input.At == nil || *input.At == 0) {
		return CronJob{}, fmt.Errorf("定时任务需指定 every_ms（循环）或 at（单次）之一")
	}
	if input.EveryMs != nil && *input.EveryMs != 0 && input.At != nil && *input.At != 0 {
		return CronJob{}, fmt.Errorf("every_ms 与 at 互斥（循环与单次二选一）")
	}
	if input.EveryMs != nil && *input.EveryMs < 1000 {
		return CronJob{}, fmt.Errorf("every_ms 最小 1000（1 秒）")
	}
	if input.At != nil && *input.At <= now {
		return CronJob{}, fmt.Errorf("单次任务的 at 必须晚于当前时间")
	}
	name := input.Name
	if name == "" {
		name = "unnamed"
	}
	job := CronJob{
		ID: randID(), Name: name, Prompt: input.Prompt,
		EveryMs: input.EveryMs, At: input.At,
		LastRunAt: 0, NextRunAt: now, CreatedAt: now, Catchup: 0,
	}
	if input.At != nil && *input.At != 0 {
		job.NextRunAt = *input.At
	} else {
		job.NextRunAt = now + *input.EveryMs
	}
	s.jobs = append(s.jobs, job)
	s.saveJobs()
	return job, nil
}

func (s *CronStore) remove(id string) bool {
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
			s.saveJobs()
			return true
		}
	}
	return false
}

// 暂停：循环任务取消排程（next_run_at=0）；单次任务直接删除（无法暂停未来时刻）
func (s *CronStore) pause(id string) string {
	j := s.get(id)
	if j == nil {
		return ""
	}
	if j.At != nil {
		s.remove(id)
		return "deleted"
	}
	j.NextRunAt = 0
	s.saveJobs()
	return "paused"
}

// 恢复：循环任务按 last_run_at（或现在）重排下一次
func (s *CronStore) resume(id string) bool {
	j := s.get(id)
	if j == nil || j.At != nil {
		return false
	}
	period := int64(60000)
	if j.EveryMs != nil {
		period = *j.EveryMs
	}
	now := time.Now().UnixMilli()
	anchor := j.LastRunAt
	if anchor == 0 || anchor+period <= now {
		// 锚点已落在过去（暂停超过一个周期）→ 改按现在重排，否则 next 落在过去会被立即触发
		anchor = now
	}
	j.NextRunAt = anchor + period
	s.saveJobs()
	return true
}

// 启动时重排（补跑决策）：返回需立即触发的任务。
// 循环任务以 last_run_at 为相位锚点（从未执行则用 next_run_at 锚点）：
//   missed = 已错过的周期数 → catchup=min(missed, 上限)；
//   next_run_at = 锚点 + (missed+1) 个周期 → 严格落在现在之后（相位不漂移）。
// 单次任务：已过期 → 补跑一次。
func (s *CronStore) rescheduleOnBoot() []CronJob {
	now := time.Now().UnixMilli()
	var due []CronJob
	for i := range s.jobs {
		j := &s.jobs[i]
		if j.At != nil {
			if j.NextRunAt <= now {
				j.Catchup = 1
				// 补跑后清掉到期时刻：trigger() 不查 firing 表，旧值会让 tick 在补跑期间二次触发
				j.NextRunAt = 0
				due = append(due, *j)
			}
			continue
		}
		if j.NextRunAt == 0 {
			continue // 已暂停
		}
		period := int64(60000)
		if j.EveryMs != nil {
			period = *j.EveryMs
		}
		if j.NextRunAt <= now {
			var missed int64
			if j.LastRunAt > 0 {
				anchor := j.LastRunAt
				missed = (now - anchor) / period
				if missed < 1 {
					missed = 1
				}
				j.NextRunAt = anchor + (missed+1)*period
			} else {
				anchor := j.NextRunAt
				missed = (now - anchor) / period + 1
				if missed < 1 {
					missed = 1
				}
				j.NextRunAt = anchor + missed*period
			}
			j.Catchup = int(missed)
			if j.Catchup > s.maxCatchup {
				j.Catchup = s.maxCatchup
			}
			// 补跑只补一次：无论错过几次，启动后最多立即触发 1 次
			due = append(due, *j)
		}
	}
	if len(due) > 0 {
		s.saveJobs()
	}
	return due
}

// 触发执行完成：
//   advance=true（自然到点触发）→ 循环任务锚点推进 next=now+period；单次任务删除。
//   advance=false（手动立即执行）→ 循环任务排程相位不动；单次任务仍视为已消费，删除。
func (s *CronStore) markDone(job CronJob, advance bool) {
	if job.At != nil {
		s.remove(job.ID)
		return
	}
	if !advance {
		return
	}
	period := int64(60000)
	if job.EveryMs != nil {
		period = *job.EveryMs
	}
	j := s.get(job.ID)
	if j == nil {
		return
	}
	j.LastRunAt = time.Now().UnixMilli()
	j.NextRunAt = j.LastRunAt + period
	j.Catchup = 0
	s.saveJobs()
}

// ---------- 触发句柄（同 lib.ts makeHandle） ----------

func makeHandle(instanceURL, id string, timeout time.Duration) func(job CronJob) (string, error) {
	base := strings.TrimSuffix(instanceURL, "/")
	return func(job CronJob) (string, error) {
		body, _ := json.Marshal(map[string]string{"message": job.Prompt})
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", base+"/chat", strings.NewReader(string(body)))
		req.Header.Set("content-type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return "", fmt.Errorf("实例 %s /chat 超时（%s 无响应）", id, timeout)
			}
			return "", fmt.Errorf("实例 %s /chat 连接失败: %v", id, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			var j struct {
				Error string `json:"error"`
			}
			detail := "（无错误详情）"
			if json.Unmarshal(raw, &j) == nil && j.Error != "" {
				detail = j.Error
			}
			return "", fmt.Errorf("实例 %s /chat %d：%s", id, resp.StatusCode, detail)
		}
		var j struct {
			Reply string `json:"reply"`
		}
		if err := json.Unmarshal(raw, &j); err != nil || j.Reply == "" {
			return "", fmt.Errorf("实例 %s /chat 返回缺 reply 字段", id)
		}
		return j.Reply, nil
	}
}

// ---------- Scheduler：全局轮询，多实例触发（每实例独立串行） ----------

type CronInstance struct {
	ID     string
	URL    string
	Store  *CronStore
	Handle func(job CronJob) (string, error)
}

type Scheduler struct {
	tickMs    int
	log       func(string)
	mu        sync.Mutex
	instances []CronInstance
	perInst   map[string]*instState
}

type instState struct {
	firing  map[string]bool
	running int32 // 0/1，CAS 占位：保证同实例串行（bool 的 check-then-set 非原子）
}

func NewScheduler(tickMs int, log func(string)) *Scheduler {
	return &Scheduler{tickMs: tickMs, log: log, perInst: map[string]*instState{}}
}

func (sc *Scheduler) Register(instances []CronInstance) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.instances = instances
	for _, inst := range instances {
		if _, ok := sc.perInst[inst.ID]; !ok {
			sc.perInst[inst.ID] = &instState{firing: map[string]bool{}}
		}
	}
}

func (sc *Scheduler) start() *time.Ticker {
	t := time.NewTicker(time.Duration(sc.tickMs) * time.Millisecond)
	go func() {
		for range t.C {
			sc.tick()
		}
	}()
	return t
}

// 启动补跑：对所有实例 rescheduleOnBoot，立即触发错过的任务（各实例串行）
func (sc *Scheduler) BootCatchup() {
	sc.mu.Lock()
	instances := append([]CronInstance{}, sc.instances...)
	sc.mu.Unlock()
	for _, inst := range instances {
		sc.mu.Lock()
		due := inst.Store.rescheduleOnBoot()
		sc.mu.Unlock()
		for _, job := range due {
			sc.log(fmt.Sprintf("[cron] 重启补跑：inst=%s job=%s（%s，错过 %d 次，只补 1 次）", inst.ID, job.ID, job.Name, job.Catchup))
			sc.trigger(inst, job, true, true)
		}
	}
}

func (sc *Scheduler) tick() {
	now := time.Now().UnixMilli()
	sc.mu.Lock()
	type dueJob struct {
		inst CronInstance
		job  CronJob
	}
	var dues []dueJob
	for _, inst := range sc.instances {
		st := sc.perInst[inst.ID]
		if st == nil {
			continue
		}
		for _, j := range inst.Store.list() {
			if j.NextRunAt > 0 && j.NextRunAt <= now && !st.firing[j.ID] {
				st.firing[j.ID] = true
				dues = append(dues, dueJob{inst, j})
			}
		}
	}
	sc.mu.Unlock()
	for _, d := range dues {
		go sc.fire(d.inst, d.job, false)
	}
}

// 手动立即触发（API /cron/run 用）：不挪循环相位；单次任务执行后删除
func (sc *Scheduler) RunNow(instanceID, jobID string) (ok bool, reply, errMsg string) {
	var inst *CronInstance
	sc.mu.Lock()
	for i := range sc.instances {
		if sc.instances[i].ID == instanceID {
			inst = &sc.instances[i]
			break
		}
	}
	sc.mu.Unlock()
	if inst == nil {
		return false, "", "实例不存在"
	}
	sc.mu.Lock()
	job := inst.Store.get(jobID)
	var jobVal CronJob
	found := job != nil
	if found {
		jobVal = *job
	}
	sc.mu.Unlock()
	if !found {
		return false, "", "任务不存在"
	}
	o, r, e := sc.trigger(*inst, jobVal, false, false)
	return o, r, e
}

func (sc *Scheduler) fire(inst CronInstance, job CronJob, catchup bool) {
	sc.mu.Lock()
	st := sc.perInst[inst.ID]
	sc.mu.Unlock()
	if st == nil {
		return
	}
	defer func() {
		sc.mu.Lock()
		st.firing[job.ID] = false
		sc.mu.Unlock()
	}()
	sc.trigger(inst, job, catchup, true)
}

// 实际触发：同实例串行（跨 tick 不并发打同一实例）；记录执行结果
func (sc *Scheduler) trigger(inst CronInstance, job CronJob, catchup, advance bool) (bool, string, string) {
	sc.mu.Lock()
	st := sc.perInst[inst.ID]
	if st == nil {
		sc.mu.Unlock()
		return false, "", "实例未注册"
	}
	sc.mu.Unlock()
	// 同实例串行：CAS 占位（旧 check-then-set 非原子：自然到点 + 手动立即执行可并行打同实例）
	for !atomic.CompareAndSwapInt32(&st.running, 0, 1) {
		time.Sleep(50 * time.Millisecond)
	}
	startedAt := time.Now().UnixMilli()
	sc.log(fmt.Sprintf("[cron] 触发：inst=%s job=%s（%s）%s", inst.ID, job.ID, job.Name, map[bool]string{true: "（重启补跑）", false: ""}[catchup]))
	ok := true
	reply := ""
	r, err := inst.Handle(job)
	if err != nil {
		ok = false
		reply = ""
		sc.log(fmt.Sprintf("[cron] 触发失败：inst=%s job=%s：%v", inst.ID, job.ID, err))
	} else {
		reply = r
	}
	// 执行完成 → 推进排程（单次删除 / 循环重排）。失败也推进，避免失败任务每 tick 重发。
	sc.mu.Lock()
	inst.Store.markDone(job, advance)
	inst.Store.recordRun(CronRunRecord{
		JobID: job.ID, Name: job.Name, At: startedAt,
		OK: ok, ReplyChars: len([]rune(reply)),
		DurationMs: time.Now().UnixMilli() - startedAt, Catchup: catchup,
	})
	atomic.StoreInt32(&st.running, 0)
	sc.mu.Unlock()
	if !ok {
		return false, "", "触发失败"
	}
	return true, reply, ""
}

func (sc *Scheduler) RecentRuns(instanceID string, limit int) []CronRunRecord {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, inst := range sc.instances {
		if inst.ID == instanceID {
			return inst.Store.recentRuns(limit)
		}
	}
	return []CronRunRecord{}
}

// ---------- 日志（[HH:MM:SS] [cron] msg，同 TS） ----------

func logf(format string, args ...interface{}) {
	ts := time.Now().UTC().Format("15:04:05")
	fmt.Printf("[%s] [cron] %s\n", ts, fmt.Sprintf(format, args...))
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[cron] "+format+"\n", args...)
	os.Exit(1)
}

// ---------- 启动 ----------

// 插件根目录：exe 同目录有 config.json 用它；否则回退 CWD（部署灵活性）
func pluginRoot() string {
	exe, err := os.Executable()
	if err == nil {
		d := filepath.Dir(exe)
		if _, e := os.Stat(filepath.Join(d, "config.json")); e == nil {
			return d
		}
	}
	return "."
}

func main() {
	root := pluginRoot()
	cfg := parseConfig(root)

	log := func(m string) {
		ts := time.Now().UTC().Format("15:04:05")
		fmt.Printf("[%s] [cron] %s\n", ts, m)
	}

	// 每实例一个独立 store + handle（多实例 = 多独立端口，各管各的 cron.json）
	instances := make([]CronInstance, 0, len(cfg.Instances))
	for _, it := range cfg.Instances {
		instances = append(instances, CronInstance{
			ID:     it.ID,
			URL:    it.URL,
			Store:  newCronStore(filepath.Join(cfg.DataDir, it.ID), cfg.CatchupMax),
			Handle: makeHandle(it.URL, it.ID, time.Duration(cfg.ChatTimeoutSec)*time.Second),
		})
	}

	scheduler := NewScheduler(cfg.TickMs, log)
	scheduler.Register(instances)
	ticker := scheduler.start()
	idList := make([]string, len(cfg.Instances))
	for i, it := range cfg.Instances {
		idList[i] = it.ID
	}
	log(fmt.Sprintf("调度器已启动：tick=%dms，实例=%s（数据落 %s/<inst-id>/cron.json）", cfg.TickMs, strings.Join(idList, ","), cfg.DataDir))

	// 重启补跑（错过的循环任务：≤上限补 1 次；>上限只重排）。异步，不阻塞启动。
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log(fmt.Sprintf("补跑失败：%v", r))
			}
		}()
		scheduler.BootCatchup()
	}()

	// ---------- HTTP 管理 API（按 inst 区分实例） ----------

	mux := http.NewServeMux()

	instOf := func(r *http.Request, body map[string]interface{}) *CronInstance {
		var v string
		if body != nil {
			if s, ok := body["inst"].(string); ok {
				v = s
			}
		}
		if v == "" {
			v = r.URL.Query().Get("inst")
		}
		for i := range instances {
			if instances[i].ID == v {
				return &instances[i]
			}
		}
		return nil
	}

	writeErr := func(w http.ResponseWriter, code int, msg string) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(code)
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.Encode(map[string]interface{}{"error": msg})
	}
	writeJSON := func(w http.ResponseWriter, code int, v interface{}) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(code)
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.Encode(v)
	}

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		type instView struct {
			ID   string `json:"id"`
			URL  string `json:"url"`
			Jobs int    `json:"jobs"`
		}
		var list []instView
		scheduler.mu.Lock()
		for _, i := range instances {
			list = append(list, instView{i.ID, i.URL, len(i.Store.list())})
		}
		scheduler.mu.Unlock()
		writeJSON(w, 200, map[string]interface{}{"ok": true, "instances": list})
	})

	mux.HandleFunc("/instances", func(w http.ResponseWriter, r *http.Request) {
		type instView struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		var list []instView
		for _, i := range cfg.Instances {
			list = append(list, instView{i.ID, i.URL})
		}
		writeJSON(w, 200, map[string]interface{}{"instances": list})
	})

	mux.HandleFunc("/cron", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writeErr(w, 405, "method not allowed")
			return
		}
		inst := instOf(r, nil)
		if inst == nil {
			writeErr(w, 400, "需 ?inst=<实例id>")
			return
		}
		scheduler.mu.Lock()
		jobs := inst.Store.list()
		scheduler.mu.Unlock()
		views := make([]cronJobView, 0, len(jobs))
		for _, j := range jobs {
			views = append(views, makeView(j))
		}
		writeJSON(w, 200, map[string]interface{}{
			"inst": inst.ID, "jobs": views,
			"recent_runs": scheduler.RecentRuns(inst.ID, 20),
		})
	})

	mux.HandleFunc("/cron/add", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		inst := instOf(r, body)
		if inst == nil {
			writeErr(w, 400, "需 inst + prompt（循环 every_ms / 单次 at 二选一）")
			return
		}
		prompt, _ := body["prompt"].(string)
		name, _ := body["name"].(string)
		input := CronJobInput{Name: name, Prompt: prompt}
		if v, ok := body["every_ms"].(float64); ok && v != 0 {
			vv := int64(v)
			input.EveryMs = &vv
		}
		if v, ok := body["at"].(float64); ok && v != 0 {
			vv := int64(v)
			input.At = &vv
		}
		if prompt == "" {
			writeErr(w, 400, "需 inst + prompt（循环 every_ms / 单次 at 二选一）")
			return
		}
		scheduler.mu.Lock()
		job, err := inst.Store.add(input)
		scheduler.mu.Unlock()
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "inst": inst.ID, "job": makeView(job)})
	})

	mux.HandleFunc("/cron/remove", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		inst := instOf(r, body)
		if inst == nil {
			writeErr(w, 400, "需 inst + id")
			return
		}
		id, _ := body["id"].(string)
		if id == "" {
			writeErr(w, 400, "需 id")
			return
		}
		scheduler.mu.Lock()
		ok := inst.Store.remove(id)
		scheduler.mu.Unlock()
		writeJSON(w, 200, map[string]interface{}{"ok": ok})
	})

	mux.HandleFunc("/cron/pause", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		inst := instOf(r, body)
		if inst == nil {
			writeErr(w, 400, "需 inst + id")
			return
		}
		id, _ := body["id"].(string)
		if id == "" {
			writeErr(w, 400, "需 id")
			return
		}
		scheduler.mu.Lock()
		res := inst.Store.pause(id)
		scheduler.mu.Unlock()
		if res == "" {
			writeErr(w, 404, "任务不存在")
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "action": res})
	})

	mux.HandleFunc("/cron/resume", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		inst := instOf(r, body)
		if inst == nil {
			writeErr(w, 400, "需 inst + id")
			return
		}
		id, _ := body["id"].(string)
		if id == "" {
			writeErr(w, 400, "需 id")
			return
		}
		scheduler.mu.Lock()
		ok := inst.Store.resume(id)
		scheduler.mu.Unlock()
		if ok {
			writeJSON(w, 200, map[string]interface{}{"ok": true})
		} else {
			writeErr(w, 404, "任务不存在或为单次任务")
		}
	})

	mux.HandleFunc("/cron/run", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		inst := instOf(r, body)
		if inst == nil {
			writeErr(w, 400, "需 inst + id")
			return
		}
		id, _ := body["id"].(string)
		if id == "" {
			writeErr(w, 400, "需 id")
			return
		}
		ok, reply, errMsg := scheduler.RunNow(inst.ID, id)
		if !ok && errMsg != "" {
			if errMsg == "实例不存在" || errMsg == "任务不存在" {
				writeErr(w, 404, errMsg)
			} else {
				writeErr(w, 500, errMsg) // 触发失败（实例 /chat 报错）不是 404
			}
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": ok, "reply": reply})
	})

	// ---------- 独立管理页 ----------
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		p := filepath.Join(root, "webui", "index.html")
		html, err := os.ReadFile(p)
		if err != nil {
			w.Header().Set("content-type", "text/plain; charset=utf-8")
			w.WriteHeader(503)
			fmt.Fprint(w, "OkHuman cron 插件管理页未部署（缺 webui/index.html）")
			return
		}
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.WriteHeader(200)
		w.Write(html)
	})

	server := &http.Server{Addr: netHost(cfg.Host, cfg.Port), Handler: mux}
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatalf("管理 API 监听失败（端口被占用？）：%v", err)
		}
	}()
	log(fmt.Sprintf("HTTP 管理 API 就绪：http://%s:%d（/ 管理页，/cron?inst=<id> 任务列表）", cfg.Host, cfg.Port))

	// 常驻：Ctrl-C 优雅收尾
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	if s == syscall.SIGINT {
		log("收到 SIGINT，停止调度并退出…")
	}
	ticker.Stop()
	os.Exit(0)
}

func netHost(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

func readBody(r *http.Request) map[string]interface{} {
	defer r.Body.Close()
	raw, err := io.ReadAll(bufio.NewReader(r.Body))
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}
