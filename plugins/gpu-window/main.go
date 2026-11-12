// gpu-window —— 把某个服务占的显存临时换出到 host 内存（让卡），用完换回来：一张卡当分时槽位用。
// 关键设计：只操作「真正持有显存的 pid」——从 nvidia-smi 的显存归属取，不碰 unit 的 MainPID
// （多进程服务打在 MainPID 上会「成功但无效」，2026-11-08 在 8080 上踩过）。
package main

/*
#cgo LDFLAGS: -lcuda
typedef int CUresult;
CUresult cuInit(unsigned int);
CUresult cuGetErrorString(CUresult, const char**);
static const char* gw_errstr(CUresult r){ const char* s = 0; cuGetErrorString(r, &s); return s ? s : "unknown"; }
CUresult cuCheckpointProcessGetState(int, int*);
CUresult cuCheckpointProcessLock(int, void*);
CUresult cuCheckpointProcessCheckpoint(int, void*);
CUresult cuCheckpointProcessRestore(int, void*);
CUresult cuCheckpointProcessUnlock(int, void*);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- 配置（本目录 config.json；能力表不写死在代码里）----------

type gate struct {
	URL   string `json:"url"`   // 该服务的客户实例 /status
	Field string `json:"field"` // 默认 "running"
}
type service struct {
	Name      string  `json:"name"`
	Match     strList `json:"match"`     // 对 /proc/<pid>/cmdline 子串匹配（可写一个或数组）
	Protected bool    `json:"protected"` // true = 默认不许让卡（如 8080）
	Gates     []gate  `json:"gates"`     // 空闲闸门：这些实例在 run 就拒绝切
}
type conf struct{ Services []service }

type strList []string

func (l *strList) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*l = strList{s}
		return nil
	}
	var a []string
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*l = a
	return nil
}

func pluginRoot() string {
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		if _, e := os.Stat(filepath.Join(d, "config.json")); e == nil {
			return d
		}
	}
	wd, _ := os.Getwd()
	return wd
}
func loadConf() (*conf, error) {
	b, err := os.ReadFile(filepath.Join(pluginRoot(), "config.json"))
	if err != nil {
		return nil, err
	}
	c := &conf{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	return c, nil
}

// ---------- 显存归属（唯一的 pid 来源）----------

type gpuRow struct{ Index, UUID, Name, MemUsed string }
type appRow struct {
	UUID, PID string
	MiB       int
	Cmdline   string
}

func smi(kind, fields string) (string, error) {
	out, err := exec.Command("nvidia-smi", "--query-"+kind+"="+fields, "--format=csv,noheader").Output()
	return string(out), err
}
func parseGPUs(csv string) []gpuRow {
	var r []gpuRow
	for _, ln := range strings.Split(strings.TrimSpace(csv), "\n") {
		f := strings.Split(ln, ", ")
		if len(f) < 4 {
			continue
		}
		r = append(r, gpuRow{f[0], f[1], f[2], f[3]})
	}
	return r
}
func parseApps(csv string) []appRow {
	var r []appRow
	for _, ln := range strings.Split(strings.TrimSpace(csv), "\n") {
		f := strings.Split(ln, ", ")
		if len(f) < 3 {
			continue
		}
		mib, _ := strconv.Atoi(strings.Fields(f[2])[0])
		cl, _ := os.ReadFile("/proc/" + f[1] + "/cmdline")
		r = append(r, appRow{f[0], f[1], mib, strings.ReplaceAll(string(cl), "\x00", " ")})
	}
	return r
}
func snapshot() ([]gpuRow, []appRow) {
	g, err := smi("gpu", "index,uuid,name,memory.used")
	if err != nil {
		return nil, nil
	}
	a, err := smi("compute-apps", "gpu_uuid,pid,used_memory")
	if err != nil {
		return nil, nil
	}
	return parseGPUs(g), parseApps(a)
}

// resolvePids：按 match 找出该服务在当前 GPU 上持有显存的所有 pid（多进程服务会返回多个）
func resolvePids(svc service, apps []appRow) []string {
	var pids []string
	for _, a := range apps {
		for _, m := range svc.Match {
			if m != "" && strings.Contains(a.Cmdline, m) {
				pids = append(pids, a.PID)
				break
			}
		}
	}
	sort.Strings(pids)
	return pids
}

// ---------- 闸门与动作 ----------

// bodyBlocks：响应里 field 为 true 就说明那个客户实例正在跑（切了会打断它的回合）
func bodyBlocks(b []byte, field string) bool {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	v, ok := m[field]
	return ok && fmt.Sprint(v) == "true"
}

func gateBlock(svc service) string {
	for _, g := range svc.Gates {
		f := g.Field
		if f == "" {
			f = "running"
		}
		b, err := exec.Command("curl", "-s", "-m", "5", g.URL).Output()
		if err != nil {
			continue // 探不到就不拦（例如那台没起），README 里写明
		}
		if bodyBlocks(b, f) {
			return fmt.Sprintf("%s 显示 %s=true（正在跑，切了会打断）", g.URL, f)
		}
	}
	return ""
}

func ckCall(fn string, pid int) error {
	var r C.CUresult
	switch fn {
	case "lock":
		r = C.cuCheckpointProcessLock(C.int(pid), nil)
	case "checkpoint":
		r = C.cuCheckpointProcessCheckpoint(C.int(pid), nil)
	case "restore":
		r = C.cuCheckpointProcessRestore(C.int(pid), nil)
	case "unlock":
		r = C.cuCheckpointProcessUnlock(C.int(pid), nil)
	}
	if r != 0 {
		return fmt.Errorf("%s: %s", fn, C.GoString(C.gw_errstr(r)))
	}
	return nil
}
func getState(pid int) (int, error) {
	var st C.int
	if r := C.cuCheckpointProcessGetState(C.int(pid), &st); r != 0 {
		return -1, fmt.Errorf("getstate: %s", C.GoString(C.gw_errstr(r)))
	}
	return int(st), nil
}
func stateName(s int) string {
	switch s {
	case 0:
		return "RUNNING"
	case 1:
		return "LOCKED"
	case 2:
		return "CHECKPOINTED（已让卡）"
	case 3:
		return "FAILED"
	}
	return "未知"
}

func doYield(pids []string) error {
	for _, p := range pids {
		pid, _ := strconv.Atoi(p)
		if st, err := getState(pid); err == nil && st == 2 {
			fmt.Printf("  pid=%s 已经是 CHECKPOINTED，跳过\n", p)
			continue
		}
		if err := ckCall("lock", pid); err != nil {
			return err
		}
		if err := ckCall("checkpoint", pid); err != nil {
			_ = ckCall("unlock", pid) // 失败回退，别把进程卡在 LOCKED
			return err
		}
		fmt.Printf("  pid=%s 已让卡\n", p)
	}
	return nil
}
func doResume(pids []string) error {
	for _, p := range pids {
		pid, _ := strconv.Atoi(p)
		st, err := getState(pid)
		if err != nil {
			return err
		}
		if st != 2 {
			fmt.Printf("  pid=%s 状态是 %s，不需要换回\n", p, stateName(st))
			continue
		}
		if err := ckCall("restore", pid); err != nil {
			return err
		}
		if err := ckCall("unlock", pid); err != nil {
			return err
		}
		fmt.Printf("  pid=%s 已换回\n", p)
	}
	return nil
}

func printCards() {
	gpus, _ := snapshot()
	for _, g := range gpus {
		fmt.Printf("  GPU%s %s 已用 %s\n", g.Index, g.Name, g.MemUsed)
	}
}

// targets：先当服务名查 config，查不到才当 pid——服务名常常就是端口号（"8080"），
// 先 Atoi 会把它们当成 pid，于是 protected/gates 全部失效（第一版就栽在这）。
func targets(arg string) ([]string, *service, error) {
	c, err := loadConf()
	if err != nil && os.IsNotExist(err) {
		err = nil
	}
	_, apps := snapshot()
	if c != nil {
		for _, s := range c.Services {
			if s.Name != arg {
				continue
			}
			p := resolvePids(s, apps)
			if len(p) == 0 {
				return nil, nil, fmt.Errorf("服务 %s 当前没有任何进程持有显存（没起？或 match 不对）", arg)
			}
			return p, &s, nil
		}
	}
	if pid, e := strconv.Atoi(arg); e == nil && pid > 0 {
		return []string{arg}, nil, nil
	}
	return nil, nil, fmt.Errorf("config.json 里没有这个服务名，也不是 pid：%s", arg)
}

func usage() {
	fmt.Print(`gpu-window —— 让出/换回某个服务占的显存（一张卡当分时槽位）
用法:
  gpu-window map                      每张卡的显存归属 + 哪个是 config 里登记的服务（pid 的唯一来源）
  gpu-window status <服务名|pid>
  gpu-window yield  <服务名|pid> [--force]   换出到内存（protected 服务需 --force）
  gpu-window resume <服务名|pid>             换回来
  gpu-window window <服务名> --for <秒> | --cmd '<命令>'    让卡→做事→必定换回
判据：只在服务空闲时切；被换出的进程 CPU 侧仍活，但任何触碰显存的调用会阻塞；
      换回要求那张卡仍够它用。实测数见 host-crash-autostart/services.md 的 8081 那条。
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if r := C.cuInit(0); r != 0 {
		fmt.Fprintf(os.Stderr, "cuInit 失败：%s（驱动不支持 checkpoint？本机需 550+，实测 580.178.04 可）\n", C.GoString(C.gw_errstr(r)))
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	must := func(e error) {
		if e != nil {
			fmt.Fprintln(os.Stderr, "错误:", e)
			os.Exit(1)
		}
	}

	switch cmd {
	case "map":
		gpus, apps := snapshot()
		c, _ := loadConf()
		for _, g := range gpus {
			fmt.Printf("GPU%s %s 已用 %s\n", g.Index, g.Name, g.MemUsed)
			for _, a := range apps {
				if a.UUID != g.UUID {
					continue
				}
				tag := "未登记"
				for _, s := range append2nil(c) {
					if len(resolvePids(s, []appRow{a})) > 0 {
						tag = s.Name
						if s.Protected {
							tag += "(protected)"
						}
					}
				}
				n := a.Cmdline
				if len(n) > 60 {
					n = n[:60] + "…"
				}
				fmt.Printf("   pid=%-7s %6d MiB  [%s] %s\n", a.PID, a.MiB, tag, n)
			}
		}
	case "status", "yield", "resume":
		if len(args) < 1 {
			usage()
			os.Exit(2)
		}
		force := false
		for _, a := range args[1:] {
			if a == "--force" {
				force = true
			}
		}
		pids, svc, err := targets(args[0])
		must(err)
		if svc != nil {
			if svc.Protected && cmd == "yield" && !force {
				fmt.Fprintf(os.Stderr, "拒绝：%s 在 config 里标了 protected（要切加 --force）\n", svc.Name)
				os.Exit(3)
			}
			if cmd == "yield" {
				if b := gateBlock(*svc); b != "" {
					fmt.Fprintln(os.Stderr, "拒绝:", b)
					os.Exit(2)
				}
			}
		}
		switch cmd {
		case "status":
			for _, p := range pids {
				pid, _ := strconv.Atoi(p)
				st, err := getState(pid)
				if err != nil {
					must(err)
				}
				fmt.Printf("  pid=%s state=%d %s\n", p, st, stateName(st))
			}
		case "yield":
			fmt.Printf("让卡：%s\n", strings.Join(pids, " "))
			printCards()
			t := time.Now()
			must(doYield(pids))
			fmt.Printf("耗时 %.1fs\n", time.Since(t).Seconds())
			printCards()
		case "resume":
			t := time.Now()
			must(doResume(pids))
			fmt.Printf("耗时 %.1fs\n", time.Since(t).Seconds())
			printCards()
		}
	case "window":
		if len(args) < 1 {
			usage()
			os.Exit(2)
		}
		name := args[0]
		var forSec int
		var shellCmd string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--for":
				forSec, _ = strconv.Atoi(args[i+1])
				i++
			case "--cmd":
				shellCmd = args[i+1]
				i++
			}
		}
		pids, svc, err := targets(name)
		must(err)
		if svc != nil {
			if svc.Protected {
				must(fmt.Errorf("%s 是 protected", name))
			}
			if b := gateBlock(*svc); b != "" {
				must(fmt.Errorf(b))
			}
		}
		t := time.Now()
		must(doYield(pids))
		fmt.Printf("已让卡（%.1fs），当前显存：\n", time.Since(t).Seconds())
		printCards()
		defer func() {
			t2 := time.Now()
			if e := doResume(pids); e != nil {
				fmt.Fprintln(os.Stderr, "换回失败（可能需要重启该服务）:", e)
				return
			}
			fmt.Printf("已换回（%.1fs）\n", time.Since(t2).Seconds())
		}()
		if shellCmd != "" {
			c := exec.Command("bash", "-c", shellCmd)
			c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
			must(c.Run())
		} else {
			if forSec <= 0 {
				forSec = 30
			}
			fmt.Printf("窗口开着，%d 秒后自动换回…\n", forSec)
			time.Sleep(time.Duration(forSec) * time.Second)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func append2nil(c *conf) []service {
	if c == nil {
		return nil
	}
	return c.Services
}
