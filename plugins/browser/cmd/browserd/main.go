// OkHuman_p/browser — 浏览器实例守护（browserd）
//
// 起一个 headless chromium（CDP 监听指定端口）并管理其生命周期：
//   - 默认：600s 内无任何 browserctl 动作命令 → 自动杀 chromium 退出
//   - --keep：持久（直到显式 close / kill）
//   - --idle <sec>：自定义空闲超时（秒）
//   - --headed：有头模式（弹真实窗口；默认无头）
//   - --shared：共享实例（多 agent 共用一份登录态；持久，杀进程才灭）
//   - --data-dir <dir>：持久画像目录（保登录态跨重启存活；缺省每次启动独立 /tmp 临时目录）
//
// TS 版 browserd.ts 的 1:1 Go 移植（日志文案/退出码/状态文件布局逐字对齐）。
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var sharedPortFile = filepath.Join(os.TempDir(), "okhuman-browser-shared.port")

type daemonArgs struct {
	port    int
	keep    bool
	headed  bool
	shared  bool
	idleMs  int64
	dataDir string // 2026-09-15：空 = 每次启动独立 /tmp 临时目录；非空 = 持久画像目录（保登录态）
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "browserd: "+msg)
	os.Exit(1)
}

func parseDaemonArgs(argv []string) daemonArgs {
	if len(argv) < 1 {
		fail("usage: browserd <port> [--keep] [--idle <sec>] [--headed] [--shared] [--data-dir <dir>]")
	}
	f, err := strconv.ParseFloat(argv[0], 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f || f < 1 || f > 65535 {
		fail(fmt.Sprintf("port 须为 1-65535 整数，收到: %s", argv[0]))
	}
	keep, headed, shared := false, false, false
	var dataDir string
	idleSec := 600.0
	for i := 1; i < len(argv); i++ {
		switch argv[i] {
		case "--keep":
			keep = true
		case "--headed":
			headed = true
		case "--shared":
			shared = true
		case "--idle":
			if i+1 >= len(argv) {
				fail("--idle 需要正整数（秒）")
			}
			v, err := strconv.ParseFloat(argv[i+1], 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
				fail("--idle 需要正整数（秒）")
			}
			idleSec = v
			i++
		case "--data-dir":
			if i+1 >= len(argv) || argv[i+1] == "" {
				fail("--data-dir 需要目录路径")
			}
			dataDir = argv[i+1]
			i++
		default:
			fail(fmt.Sprintf("未知参数: %s（支持 --keep / --idle <sec> / --headed / --shared / --data-dir <dir>）", argv[i]))
		}
	}
	return daemonArgs{port: int(f), keep: keep, headed: headed, shared: shared, idleMs: int64(idleSec * 1000), dataDir: dataDir}
}

var httpc = &http.Client{Timeout: 2 * time.Second}

func cdpUp(port int) bool {
	r, err := httpc.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.StatusCode >= 200 && r.StatusCode < 300
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

type stateOut struct {
	DaemonPID int   `json:"daemon_pid"`
	ChromePID int   `json:"chrome_pid"`
	Keep      bool  `json:"keep"`
	Shared    bool  `json:"shared"`
	Headed    bool  `json:"headed"`
	StartedAt int64 `json:"started_at"`
}

func main() {
	a := parseDaemonArgs(os.Args[1:])
	hbFile := filepath.Join(os.TempDir(), fmt.Sprintf("okhuman-browser-%d.hb", a.port))
	stateFile := filepath.Join(os.TempDir(), fmt.Sprintf("okhuman-browser-%d.state", a.port))
	stopFile := filepath.Join(os.TempDir(), fmt.Sprintf("okhuman-browser-%d.stop", a.port))

	// 端口冲突快速失败（CDP 端口被占 = 已有实例或他物）
	if cdpUp(a.port) {
		fail(fmt.Sprintf("端口 %d 被占（CDP 已在监听）——换一个端口", a.port))
	}

	// 共享实例：指针文件记端口（agent 经 bash 读取发现），全局至多一个（O_EXCL 互斥）
	if a.shared {
		if fileExists(sharedPortFile) {
			b, _ := os.ReadFile(sharedPortFile)
			other, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			if cdpUp(other) {
				fail(fmt.Sprintf("共享实例已存在（端口 %d）——用它，别再开", other))
			}
			os.Remove(sharedPortFile) // 陈旧指针（进程被杀残留），清掉
		}
		f, err := os.OpenFile(sharedPortFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			fail("竞争——另一个共享实例正在启动，用它")
		}
		f.WriteString(strconv.Itoa(a.port))
		f.Close()
	}

	chromePath := os.Getenv("OKH_BROWSER_CHROME")
	if chromePath == "" {
		if p, err := exec.LookPath("chromium"); err == nil {
			chromePath = p
		} else {
			// 各发行版常见安装位置（易迁移：不钉死单一绝对路径）
			for _, c := range []string{"/usr/bin/chromium", "/usr/lib/chromium/chromium", "/usr/bin/chromium-browser", "/usr/bin/google-chrome", "/usr/bin/google-chrome-stable"} {
				if _, err := os.Stat(c); err == nil {
					chromePath = c
					break
				}
			}
		}
		if chromePath == "" {
			fail("未找到 chromium：请安装，或用 OKH_BROWSER_CHROME 环境变量指定路径")
		}
	}
	// 画像目录：默认每次启动独立 /tmp 目录（短任务用完即弃）；
	// --data-dir 指定持久目录可保登录态（如 B 站扫码）跨重启/重开机存活
	chromeDataDir := a.dataDir
	if chromeDataDir == "" {
		chromeDataDir = filepath.Join(os.TempDir(), fmt.Sprintf("okhuman-browser-%d-data-%s", a.port, strconv.FormatInt(time.Now().UnixMilli(), 36)))
	}
	chromeArgs := []string{}
	if !a.headed {
		chromeArgs = append(chromeArgs, "--headless")
	}
	chromeArgs = append(chromeArgs,
		"--no-sandbox",
		"--disable-gpu",
		fmt.Sprintf("--remote-debugging-port=%d", a.port),
		fmt.Sprintf("--user-data-dir=%s", chromeDataDir),
		"about:blank",
	)
	cmd := exec.Command(chromePath, chromeArgs...)
	// detached：chromium 自成进程组（组 leader），cleanup 可杀整棵进程树；
	// daemon 被杀后 chromium 作为孤儿继续活（与 TS detached:true 同语义）
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fail(err.Error())
	}

	var (
		exitMu       sync.Mutex
		chromeExited bool
		chromeCode   *int // nil = 信号杀死（TS 里 code=null）
	)
	go func() {
		err := cmd.Wait()
		code := 0
		signaled := false
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok2 := ee.ProcessState.Sys().(syscall.WaitStatus); ok2 {
				if ws.Signaled() {
					signaled = true
				} else {
					code = ws.ExitStatus()
				}
			}
		}
		exitMu.Lock()
		chromeExited = true
		if signaled {
			chromeCode = nil
		} else {
			chromeCode = &code
		}
		exitMu.Unlock()
	}()

	exitInfo := func() (bool, string) {
		exitMu.Lock()
		defer exitMu.Unlock()
		if !chromeExited {
			return false, ""
		}
		if chromeCode == nil {
			return true, "null"
		}
		return true, strconv.Itoa(*chromeCode)
	}

	startedAt := time.Now().UnixMilli()
	// 等 CDP 起来（≤8s）
	deadline := time.Now().Add(8 * time.Second)
	for {
		if exited, code := exitInfo(); exited {
			fail(fmt.Sprintf("chromium 提前退出（code=%s）", code))
		}
		if cdpUp(a.port) {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "browserd: chromium CDP 8s 内未就绪，放弃")
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			os.Exit(1)
		}
		time.Sleep(200 * time.Millisecond)
	}

	os.WriteFile(hbFile, []byte(strconv.FormatInt(startedAt, 10)), 0644)
	st, _ := json.Marshal(stateOut{DaemonPID: os.Getpid(), ChromePID: cmd.Process.Pid, Keep: a.keep, Shared: a.shared, Headed: a.headed, StartedAt: startedAt})
	os.WriteFile(stateFile, st, 0644)

	mode := "无头"
	if a.shared {
		mode = "共享"
	} else if a.headed {
		mode = "有头"
	}
	persist := "，持久"
	if !a.keep && !a.shared {
		persist = fmt.Sprintf("，空闲 %g%s", float64(a.idleMs)/1000, "s 自动关闭")
	}
	fmt.Printf("browserd: 实例 :%d 就绪（%s，chromium pid=%d%s）\n", a.port, mode, cmd.Process.Pid, persist)

	cleanup := func() {
		// 杀整棵进程树（chromium 为组 leader）并等死透再删目录——
		// 先删目录而孤儿子进程还活着，会在 profile 里重建文件残留
		killed := false
		if pid := cmd.Process.Pid; pid != 0 {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
				killed = true
			}
		}
		if !killed {
			cmd.Process.Kill()
		}
		exitDeadline := time.Now().Add(3 * time.Second)
		for {
			exited, _ := exitInfo()
			if exited || time.Now().After(exitDeadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		for _, f := range []string{hbFile, stateFile, stopFile} {
			os.Remove(f)
		}
		// 活跃 tab 记录：legacy .active + 按 agent 的 .active-<agent>
		entries, _ := os.ReadDir("/tmp")
		for _, e := range entries {
			name := e.Name()
			if name == fmt.Sprintf("okhuman-browser-%d.active", a.port) ||
				strings.HasPrefix(name, fmt.Sprintf("okhuman-browser-%d.active-", a.port)) {
				os.Remove(filepath.Join("/tmp", name))
			}
		}
		if a.shared {
			os.Remove(sharedPortFile)
		}
		// 仅删临时画像目录；--data-dir 指定的持久目录保留（登录态跨重启存活）
		if a.dataDir == "" {
			os.RemoveAll(chromeDataDir)
		}
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigCh
		name := "SIGTERM"
		if s == syscall.SIGINT {
			name = "SIGINT"
		}
		fmt.Printf("browserd: 收到 %s，退出\n", name)
		cleanup()
		os.Exit(0)
	}()

	for {
		time.Sleep(5 * time.Second)
		if fileExists(stopFile) {
			fmt.Println("browserd: 收到 stop 信号，退出")
			cleanup()
			os.Exit(0)
		}
		if exited, code := exitInfo(); exited {
			fmt.Printf("browserd: chromium 退出（code=%s），随之退出\n", code)
			cleanup()
			os.Exit(0)
		}
		if !a.keep && !a.shared {
			hb := time.UnixMilli(startedAt)
			if st, err := os.Stat(hbFile); err == nil {
				hb = st.ModTime()
			}
			if time.Since(hb) > time.Duration(a.idleMs)*time.Millisecond {
				fmt.Printf("browserd: 空闲 %gs 超时，自动关闭\n", float64(a.idleMs)/1000)
				cleanup()
				os.Exit(0)
			}
		}
	}
}
