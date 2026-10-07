// okmon — OkHuman 系进程监控插件
//
// 监控两块：
//  1. 显卡使用（nvidia-smi：每卡 利用率/显存/温度/功耗 + 显存占用进程）
//  2. OkHuman 系 Go 常驻进程（okhuman 实例 + Go 插件），可手动 启动/停止
//
// 独立进程，HTTP :8496（config.json 可改），自带 webui。挂掉不影响主进程。
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed webui.html
var webuiHTML []byte

// ---------- 配置 ----------

type Service struct {
	Name    string `json:"name"`
	Label   string `json:"label,omitempty"`
	Port    int    `json:"port,omitempty"`  // 监听端口（>0 时按端口定位进程，最可靠）
	Match   string `json:"match,omitempty"` // 无端口时的定位方式：comm 精确名（pgrep -x）或 cmdline 模式（pgrep -f）
	Start   string `json:"start,omitempty"` // 相对 Workdir 的启动命令
	Workdir string `json:"workdir,omitempty"`
	CLI     bool   `json:"cli,omitempty"` // 纯 CLI（无常驻进程），不给开关按钮
}

type Config struct {
	Port     int       `json:"port"`
	Services []Service `json:"services"`
}

func loadConfig() Config {
	cfg := Config{Port: 8496}
	b, err := os.ReadFile("config.json")
	if err != nil {
		fmt.Println("[okmon] 无 config.json，用默认服务表")
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		fmt.Println("[okmon] config.json 解析失败:", err)
	}
	if cfg.Port == 0 {
		cfg.Port = 8496
	}
	return cfg
}

// ---------- 进程定位 ----------

var ssLineRe = regexp.MustCompile(`:(\d+)\s+.*users:\(\("([^"]+)",pid=(\d+)`)

// pidsByPort 从 ss -ltnp 找监听某端口的 pid
func pidsByPort(port int) []int {
	out, err := exec.Command("ss", "-ltnp").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		m := ssLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if p, _ := strconv.Atoi(m[1]); p == port {
			if pid, _ := strconv.Atoi(m[3]); pid > 0 {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// portsByPid ss -ltnp 全量解析 → pid→监听端口（一次 ss 调用，取每个 pid 首个监听端口）
func portsByPid() map[int]int {
	out, err := exec.Command("ss", "-ltnp").Output()
	if err != nil {
		return nil
	}
	m := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		sm := ssLineRe.FindStringSubmatch(line)
		if sm == nil {
			continue
		}
		port, _ := strconv.Atoi(sm[1])
		pid, _ := strconv.Atoi(sm[3])
		if pid > 0 && port > 0 {
			if _, exists := m[pid]; !exists {
				m[pid] = port
			}
		}
	}
	return m
}

// okhumanPids 一切 Go 版 okhuman 主程序进程（comm 精确 "okhuman"）
func okhumanPids() []int {
	out, err := exec.Command("pgrep", "-x", "okhuman").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// findPid 定位服务进程：端口优先，其次 comm 精确 / cmdline 模式
func findPid(s Service) int {
	if s.Port > 0 {
		if pids := pidsByPort(s.Port); len(pids) > 0 {
			return pids[0]
		}
		if s.Match == "" {
			return 0
		}
	}
	if s.Match == "" {
		return 0
	}
	if len(s.Match) <= 15 && !strings.ContainsAny(s.Match, "/ ") {
		out, err := exec.Command("pgrep", "-x", s.Match).Output()
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil {
				return pid
			}
		}
		return 0
	}
	out, err := exec.Command("pgrep", "-f", s.Match).Output()
	if err != nil {
		return 0
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil {
		return pid
	}
	return 0
}

// ---------- 进程指标 ----------

var cpuSamples = map[int]struct {
	ticks float64
	ts    time.Time
}{}

var statRe = regexp.MustCompile(`\)$`)

func readStat(pid int) (ticks float64, starttime float64, ok bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, false
	}
	s := statRe.Split(string(b), 2)
	if len(s) < 2 {
		return 0, 0, false
	}
	f := strings.Fields(s[1])
	// 去掉 comm 后：field0=state(原3), utime=原14→f[11], stime=原15→f[12], starttime=原22→f[19]
	if len(f) < 20 {
		return 0, 0, false
	}
	u, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	si, _ := strconv.ParseFloat(f[19], 64)
	return u + st, si, true
}

func bootSeconds() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func procInfo(pid int) (cmdline string, memMB float64, cpuPct float64, uptimeS int) {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		cmdline = strings.TrimRight(strings.ReplaceAll(string(b), "\x00", " "), " ")
		if len(cmdline) > 110 {
			cmdline = cmdline[:110] + "…"
		}
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					if kb, err := strconv.ParseFloat(f[1], 64); err == nil {
						memMB = kb / 1024
					}
				}
			}
		}
	}
	now := time.Now()
	if ticks, starttime, ok := readStat(pid); ok {
		if prev, exists := cpuSamples[pid]; exists {
			dt := now.Sub(prev.ts).Seconds()
			if dt > 0 {
				cpuPct = (ticks - prev.ticks) / (dt * 100) * 100
			}
		}
		cpuSamples[pid] = struct {
			ticks float64
			ts    time.Time
		}{ticks, now}
		boot := bootSeconds()
		if up := int(boot - starttime/100); up > 0 {
			uptimeS = up
		}
	}
	deleteStaleCpuSamples()
	return
}

func deleteStaleCpuSamples() {
	for pid, s := range cpuSamples {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
			delete(cpuSamples, pid)
		} else if time.Since(s.ts) > time.Minute {
			delete(cpuSamples, pid)
		}
	}
}

// ---------- GPU ----------

type GPU struct {
	Idx      int     `json:"idx"`
	Name     string  `json:"name"`
	Util     int     `json:"util"`
	MemUsed  int     `json:"mem_used"`
	MemTotal int     `json:"mem_total"`
	Temp     int     `json:"temp"`
	Power    float64 `json:"power"`
}

type GPUApp struct {
	Pid  int    `json:"pid"`
	Name string `json:"name"`
	Mem  int    `json:"mem"`
	Tag  string `json:"tag,omitempty"`
}

func nvidiaQuery(args ...string) []string {
	out, err := exec.Command("nvidia-smi", args...).Output()
	if err != nil {
		return nil
	}
	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

func atoi(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func queryGPU() ([]GPU, []GPUApp) {
	var gpus []GPU
	for _, line := range nvidiaQuery("--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw", "--format=csv,noheader,nounits") {
		f := strings.Split(line, ",")
		if len(f) < 6 {
			continue
		}
		g := GPU{Idx: atoi(f[0]), Name: strings.TrimSpace(f[1]), Util: atoi(f[2]), MemUsed: atoi(f[3]), MemTotal: atoi(f[4]), Temp: atoi(f[5])}
		if p, err := strconv.ParseFloat(strings.TrimSpace(f[6]), 64); err == nil {
			g.Power = p
		}
		gpus = append(gpus, g)
	}
	var apps []GPUApp
	tagOf := func(pid int) string {
		for _, s := range cfg.Services {
			if findPid(s) == pid {
				if s.Label != "" {
					return s.Label
				}
				return s.Name
			}
		}
		return ""
	}
	for _, line := range nvidiaQuery("--query-compute-apps=pid,process_name,used_gpu_memory", "--format=csv,noheader,nounits") {
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		pid := atoi(f[0])
		apps = append(apps, GPUApp{Pid: pid, Name: strings.TrimSpace(f[1]), Mem: atoi(f[2]), Tag: tagOf(pid)})
	}
	return gpus, apps
}

// ---------- 状态 ----------

type ServiceState struct {
	Service
	Running bool    `json:"running"`
	Pid     int     `json:"pid"`
	Cmdline string  `json:"cmdline,omitempty"`
	MemMB   float64 `json:"mem_mb"`
	CPUPct  float64 `json:"cpu_pct"`
	UptimeS int     `json:"uptime_s"`
	Auto    bool    `json:"auto,omitempty"` // 自动追踪的 okhuman 进程（配置之外，只读+可停止）
}

type State struct {
	TS       string         `json:"ts"`
	GPUs     []GPU          `json:"gpus"`
	GPUApps  []GPUApp       `json:"gpu_apps"`
	Services []ServiceState `json:"services"`
}

func collectState() State {
	st := State{TS: time.Now().Format("15:04:05")}
	st.GPUs, st.GPUApps = queryGPU()
	for _, s := range cfg.Services {
		ss := ServiceState{Service: s, Running: true}
		if s.CLI {
			ss.Running = false
			ss.Cmdline = "纯 CLI，无常驻进程"
			st.Services = append(st.Services, ss)
			continue
		}
		pid := findPid(s)
		if pid == 0 {
			ss.Running = false
		} else {
			ss.Pid = pid
			ss.Cmdline, ss.MemMB, ss.CPUPct, ss.UptimeS = procInfo(pid)
		}
		st.Services = append(st.Services, ss)
	}
	// 自动追踪：配置表之外的一切 Go 版 okhuman 进程（如一次性实例），只读 + 可停止
	covered := map[int]bool{}
	for _, ss := range st.Services {
		if ss.Pid > 0 {
			covered[ss.Pid] = true
		}
	}
	portOf := portsByPid()
	for _, pid := range okhumanPids() {
		if covered[pid] {
			continue
		}
		ss := ServiceState{
			Service: Service{
				Name:  "okhuman-extra-" + strconv.Itoa(pid),
				Label: "其他 Go okhuman 实例",
				Port:  portOf[pid],
			},
			Running: true,
			Pid:     pid,
			Auto:    true,
		}
		ss.Cmdline, ss.MemMB, ss.CPUPct, ss.UptimeS = procInfo(pid)
		st.Services = append(st.Services, ss)
	}
	return st
}

// ---------- 动作 ----------

var actMu sync.Mutex

func startService(name string) (map[string]any, int) {
	actMu.Lock()
	defer actMu.Unlock()
	var s *Service
	for i := range cfg.Services {
		if cfg.Services[i].Name == name {
			s = &cfg.Services[i]
			break
		}
	}
	if s == nil {
		return map[string]any{"ok": false, "msg": "没有这个服务: " + name}, http.StatusNotFound
	}
	if s.CLI {
		return map[string]any{"ok": false, "msg": "纯 CLI，无常驻进程可启动"}, http.StatusBadRequest
	}
	if pid := findPid(*s); pid > 0 {
		return map[string]any{"ok": false, "msg": fmt.Sprintf("已在跑（pid %d）", pid), "pid": pid}, http.StatusConflict
	}
	logf := fmt.Sprintf("/tmp/okmon-%s.log", name)
	script := fmt.Sprintf("cd %s && setsid %s </dev/null >> %s 2>&1 &", s.Workdir, s.Start, logf)
	if err := exec.Command("bash", "-c", script).Start(); err != nil {
		return map[string]any{"ok": false, "msg": "启动命令执行失败: " + err.Error()}, http.StatusInternalServerError
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if pid := findPid(*s); pid > 0 {
			return map[string]any{"ok": true, "msg": fmt.Sprintf("已启动 pid %d", pid), "pid": pid}, http.StatusOK
		}
		time.Sleep(300 * time.Millisecond)
	}
	tail := ""
	if b, err := os.ReadFile(logf); err == nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		tail = " 日志:" + strings.Join(lines, " | ")
	}
	return map[string]any{"ok": false, "msg": "12 秒内没起来" + tail}, http.StatusInternalServerError
}

// stopPid 停掉指定 pid（SIGTERM → 5s → SIGKILL）
func stopPid(pid int) (map[string]any, int) {
	syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
			return map[string]any{"ok": true, "msg": fmt.Sprintf("已停止（pid %d）", pid)}, http.StatusOK
		}
		time.Sleep(200 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return map[string]any{"ok": true, "msg": fmt.Sprintf("已强制停止（pid %d）", pid)}, http.StatusOK
	}
	return map[string]any{"ok": false, "msg": "杀不掉，pid " + strconv.Itoa(pid)}, http.StatusInternalServerError
}

func stopService(name string) (map[string]any, int) {
	actMu.Lock()
	defer actMu.Unlock()
	if strings.HasPrefix(name, "okhuman-extra-") { // 自动追踪条目：直接按名字里的 pid 停
		pid, err := strconv.Atoi(strings.TrimPrefix(name, "okhuman-extra-"))
		if err == nil && pid > 0 {
			return stopPid(pid)
		}
		return map[string]any{"ok": false, "msg": "没在跑（进程已退出）"}, http.StatusConflict
	}
	var s *Service
	for i := range cfg.Services {
		if cfg.Services[i].Name == name {
			s = &cfg.Services[i]
			break
		}
	}
	if s == nil {
		return map[string]any{"ok": false, "msg": "没有这个服务: " + name}, http.StatusNotFound
	}
	if s.CLI {
		return map[string]any{"ok": false, "msg": "纯 CLI，无常驻进程可停止"}, http.StatusBadRequest
	}
	pid := findPid(*s)
	if pid == 0 {
		return map[string]any{"ok": false, "msg": "没在跑"}, http.StatusConflict
	}
	return stopPid(pid)
}

// ---------- HTTP ----------

var cfg Config

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func main() {
	cfg = loadConfig()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webuiHTML)
	})
	http.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, collectState())
	})
	http.HandleFunc("/api/services/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "POST only"})
			return
		}
		var req struct{ Name string }
		json.NewDecoder(r.Body).Decode(&req)
		out, code := startService(req.Name)
		writeJSON(w, code, out)
	})
	http.HandleFunc("/api/services/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "POST only"})
			return
		}
		var req struct{ Name string }
		json.NewDecoder(r.Body).Decode(&req)
		out, code := stopService(req.Name)
		writeJSON(w, code, out)
	})
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	fmt.Printf("[okmon] 就绪 监听 %s  服务 %d 个\n", addr, len(cfg.Services))
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
	} else {
		fmt.Println("[okmon] 端口探测失败:", err)
	}
	http.ListenAndServe(addr, nil)
}
