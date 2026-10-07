// xusectl —— 桌面 computer-use 插件（Go 移植自 OkHuman_p/computer/xusectl.ts，2026-09-14）
//
// 给 agent 装"手"：整屏截图 + X11 鼠标键盘事件。微内核设计：内核零改动，
// 视觉走现有 okattach（截图注入上下文），动作走 xdotool(XTest)/ffmpeg(x11grab)。
//
//   shot [--out <f>] [--settle <ms>]   整屏截图（ffmpeg x11grab）
//   move <x> <y> [--view-w <W>]        移动鼠标
//   click <x> <y> [--button b] [--double] [--view-w <W>] [--target 窗口名|0xID] [--settle ms]
//   right_click / double_click <x> <y>
//   drag <x1> <y1> <x2> <y2>
//   scroll <x> <y> --dx <N> --dy <N>   滚轮（dy>0 向下，dx>0 向右）
//   type <text>                        输入（ASCII XTest 键入；非 ASCII 走 U+码点 keysym）
//   key <combo> [--repeat N --interval ms]
//   hold <combo> <ms>
//   wake [密码]                        解锁 xfce4-screensaver
//   focus <窗口名|wid>
//   open <cmd...>                      setsid 起应用
//   diff <prev.png> [--out new.png] [--region x,y,w,h]  像素差异预检
//   windowactivate|windowminimize|windowraise|windowmaximize|windowunmaximize <窗口名|0xID|十进制ID>
//   stack [x y] / sleep <ms>
//
// 坐标空间：默认=真实屏幕像素。若 agent 看的是 okattach 缩略图（长边 1600），
// 加 --view-w 1600，插件自动乘 屏幕宽/1600 换算。
package main

import (
	"fmt"
	"os"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	display    = envOr("DISPLAY", ":0.0")
	focusFile  = filepath.Join(os.TempDir(), "okhuman-xuse-focus")
	dangerous  = []*regexp.Regexp{
		regexp.MustCompile(`^ctrl\+alt\+del$`),
		regexp.MustCompile(`^ctrl\+alt\+backspace$`),
		regexp.MustCompile(`^super\+l$`),
		regexp.MustCompile(`^ctrl\+alt\+f\d$`),
		regexp.MustCompile(`^alt\+f4$`),
	}
	reHexID     = regexp.MustCompile(`^0x[0-9a-f]+$`)
	reDecID     = regexp.MustCompile(`^\d+$`)
	reMouseLoc  = regexp.MustCompile(`x:(\d+) y:(\d+) screen:\d+ window:(\d+)`)
	reXWinID    = regexp.MustCompile(`- id: 0x[0-9a-f]+`)
	reYAVG      = regexp.MustCompile(`YAVG[=:]\s*([\d.]+)`)
	reYMAX      = regexp.MustCompile(`YMAX[=:]\s*([\d.]+)`)
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法: xusectl <shot|move|click|right_click|double_click|drag|scroll|type|key|hold|wake|focus|open|diff|windowactivate|windowminimize|windowraise|windowmaximize|windowunmaximize|stack|sleep> [args...]")
	fmt.Fprintln(os.Stderr, "  shot [f] [--out f] [--settle ms] / click x y [--button left|right|middle] [--double] [--view-w W] [--target 窗口名|0xID] [--settle ms] /")
	fmt.Fprintln(os.Stderr, "  type text [--paste] / key combo [--repeat N --interval ms] / hold combo ms / focus 窗口名 / open cmd... / diff prev.png [--out new.png]")
	fmt.Fprintln(os.Stderr, "  windowactivate|windowminimize|windowraise|windowmaximize|windowunmaximize <窗口名|0xID|十进制ID> / stack [x y] / sleep ms")
	fmt.Fprintln(os.Stderr, "  注意: windowactivate 在 xfwm4 有 100-250ms 置顶延迟（先焦点后置顶）；要立刻置顶用 windowraise；点击前用 stack 核对顶层")
	os.Exit(2)
}

// ---------- 参数解析 ----------

type flags struct {
	pos   []string
	kv    map[string]string
	bools map[string]bool
}

func parseFlags(tokens []string) flags {
	f := flags{kv: map[string]string{}, bools: map[string]bool{}}
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if strings.HasPrefix(t, "--") {
			k := t[2:]
			if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "--") {
				f.kv[k] = tokens[i+1]
				i++
			} else {
				f.bools[k] = true
			}
		} else {
			f.pos = append(f.pos, t)
		}
	}
	return f
}

func (f flags) has(k string) bool {
	_, ok1 := f.kv[k]
	ok2 := f.bools[k]
	return ok1 || ok2
}

// 屏幕真实尺寸
func screenSize() (int, int) {
	out := xdoOut("getdisplaygeometry")
	parts := strings.Fields(out)
	if len(parts) < 2 {
		fail(3, "取不到屏幕尺寸（xdotool 失败）")
	}
	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	if w <= 0 || h <= 0 {
		fail(3, "取不到屏幕尺寸（xdotool 失败）")
	}
	return w, h
}

// 坐标换算：--view-w W 表示坐标是 W 宽图里的像素 → 真实屏幕像素
func toScreen(x, y float64, f flags) (float64, float64) {
	if v, ok := f.kv["view-w"]; ok {
		vw, err := strconv.ParseFloat(v, 64)
		if err != nil || vw <= 0 {
			fail(3, "--view-w 须为正数（你看的图的宽度）")
		}
		w, _ := screenSize()
		fac := float64(w) / vw
		return x * fac, y * fac
	}
	return x, y
}

// ---------- xdotool 封装 ----------

func xdoOut(args ...string) string {
	cmd := exec.Command("xdotool", args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, _ := cmd.Output()
	return string(out)
}

type xdoResult struct {
	ok  bool
	err string
}

// 定向窗口：先 windowfocus 该窗口，再用全局 XTest 投递（比 XSendEvent 可靠，应用无法忽略）
func xdo(args []string, window ...string) xdoResult {
	var wid string
	if len(window) > 0 {
		wid = window[0]
	}
	if wid != "" {
		cmd := exec.Command("xdotool", "windowfocus", wid)
		cmd.Env = append(os.Environ(), "DISPLAY="+display)
		if out, err := cmd.CombinedOutput(); err != nil {
			return xdoResult{false, "windowfocus 失败: " + truncate(string(out), 200)}
		}
	}
	cmd := exec.Command("xdotool", args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s := string(out)
		return xdoResult{false, truncate(s, 300)}
	}
	return xdoResult{true, ""}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// warp 后回读校验指针落位；没落位就重新 warp，最多 tries 次。返回实际位置+是否落位
func warpVerified(sx, sy float64, tries int) (ok bool, x, y int) {
	for i := 0; i <= tries; i++ {
		roundedX, roundedY := int(sx+0.5), int(sy+0.5)
		xdoOut("mousemove", strconv.Itoa(roundedX), strconv.Itoa(roundedY))
		out := xdoOut("getmouselocation")
		m := reMouseLoc.FindStringSubmatch(out)
		x, y = 0, 0
		if m != nil {
			x, _ = strconv.Atoi(m[1])
			y, _ = strconv.Atoi(m[2])
		}
		ok = abs(x-roundedX) <= 3 && abs(y-roundedY) <= 3
		if ok {
			break
		}
	}
	return
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// 窗口 ID 归一成十进制字符串（0x 十六进制 / 十进制 / wmctrl 标题均可；xdotool 口径是十进制）
func toDecId(s string) string {
	if reHexID.MatchString(s) {
		v, _ := strconv.ParseInt(s[2:], 16, 64)
		return strconv.FormatInt(v, 10)
	}
	return s
}

// 窗口号解析：0x 十六进制 / 十进制直接用；否则按精确标题走 wmctrl -l
func resolveWid(spec string) string {
	if reHexID.MatchString(spec) || reDecID.MatchString(spec) {
		return toDecId(spec)
	}
	cmd := exec.Command("wmctrl", "-l")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, _ := cmd.Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		// wmctrl -l 行格式: <0xID> <桌面> <主机> <标题...>（标题=第4段起；-lp 才多 PID 列）
		if len(f) >= 4 && strings.Join(f[3:], " ") == spec {
			return toDecId(f[0])
		}
	}
	return ""
}

// 目标窗口全部子窗口 ID（用于识别「指针下是目标窗口的内部子窗」）
func childWids(wid string) map[string]bool {
	cmd := exec.Command("xwininfo", "-id", wid, "-tree")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, _ := cmd.Output()
	ids := map[string]bool{}
	for _, m := range reXWinID.FindAllString(string(out), -1) {
		ids[toDecId(m[6:])] = true
	}
	return ids
}

// 指针下的真实顶层窗口 ID（XQueryPointer，X 服务器口径）
func topWidAtPointer() string {
	out := xdoOut("getmouselocation")
	if m := reMouseLoc.FindStringSubmatch(out); m != nil {
		return m[3]
	}
	return "0"
}

func screensaverActive() bool {
	cmd := exec.Command("xfce4-screensaver-command", "-q")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	out, _ := cmd.CombinedOutput()
	return strings.Contains(strings.ToLower(string(out)), "激活") || strings.Contains(strings.ToLower(string(out)), "active")
}

func focusedWid() string {
	data, err := os.ReadFile(focusFile)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[0] != "" {
		return lines[0]
	}
	return ""
}

func fail(code int, msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(code)
}

func sleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// ---------- 动作 ----------

func main() {
	argv := os.Args[1:]
	if len(argv) == 0 {
		usage()
	}
	action := argv[0]
	rest := argv[1:]

	switch action {
	case "shot":
		doShot(rest)
	case "move", "click", "right_click", "double_click":
		doMouse(action, rest)
	case "drag":
		doDrag(rest)
	case "scroll":
		doScroll(rest)
	case "type":
		doType(rest)
	case "key":
		doKey(rest)
	case "hold":
		doHold(rest)
	case "wake":
		doWake(rest)
	case "focus":
		doFocus(rest)
	case "windowactivate", "windowminimize", "windowraise", "windowmaximize", "windowunmaximize":
		doWindow(action, rest)
	case "stack":
		doStack(rest)
	case "sleep":
		ms := 0
		if len(rest) > 0 {
			ms, _ = strconv.Atoi(rest[0])
			if ms < 0 {
				ms = 0
			}
		}
		sleepMs(ms)
		fmt.Printf("已等待 %dms\n", ms)
	case "open":
		doOpen(rest)
	case "diff":
		doDiff(rest)
	default:
		usage()
	}
}

func doShot(rest []string) {
	if screensaverActive() {
		fmt.Fprintln(os.Stderr, "屏幕已锁（xfce4-screensaver 激活）——先执行: xusectl wake <密码>")
		os.Exit(3)
	}
	f := parseFlags(rest)
	if s, ok := f.kv["settle"]; ok {
		sleepMs(mustInt(s))
	}
	w, h := screenSize()
	out := ""
	if s, ok := f.kv["out"]; ok && s != "" {
		out = s
	} else if len(f.pos) > 0 {
		out = f.pos[0]
	} else {
		out = filepath.Join(os.TempDir(), fmt.Sprintf("xuse-%s.png", nowBase36()))
	}
	if strings.HasPrefix(out, os.TempDir()) {
		// 临时截图 0600：屏幕内容可能敏感，防其他本地用户读取（ffmpeg 对已存在文件只截断、保留权限）
		if fh, oerr := os.OpenFile(out, os.O_CREATE|os.O_WRONLY, 0o600); oerr == nil {
			fh.Close()
			os.Chmod(out, 0o600)
		}
	}
	// -y 必带（2026-09-20 修）：shot 会先 0600 预创建输出文件，ffmpeg 无 -y 时对已存在文件
	// 弹 "File exists. Overwrite?" 交互提示，Go exec 的 stdin 是 /dev/null → EOF 当 N → 中止，
	// 留下 0 字节文件而 exit code 仍 0 → 旧代码误报"截图成功"（diff 命令一直带 -y 所以没踩到）
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "x11grab",
		"-video_size", fmt.Sprintf("%dx%d", w, h), "-i", display, "-frames:v", "1", "-y", out)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	if cerr, err := cmd.CombinedOutput(); err != nil || !exists(out) {
		fmt.Fprintln(os.Stderr, "截图失败: "+truncate(string(cerr), 300))
		os.Exit(3)
	}
	size, serr := os.Stat(out)
	if serr != nil || size.Size() == 0 {
		fmt.Fprintln(os.Stderr, "截图失败: 输出文件为空")
		os.Exit(3)
	}
	fmt.Fprintf(os.Stderr, "已截图 %s（%d 字节）\n", out, size.Size())
	fmt.Fprintf(os.Stdout, "已截图 %s（%d×%dpx，文件为原生分辨率、坐标直接用）。okattach 默认原图直传（免换算）；"+
		"仅 --scaled 注入（长边 1600）时按注入图读坐标需乘 %s，或 click 等动作加 --view-w 1600 由插件换算。\n",
		out, w, h, strconv.FormatFloat(float64(w)/1600.0, 'f', 3, 64))
}

func doMouse(action string, rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 2 {
		usage()
	}
	sx, sy := toScreen(mustFloat(f.pos[0]), mustFloat(f.pos[1]), f)
	w, h := screenSize()
	if sx < 0 || sy < 0 || sx > float64(w) || sy > float64(h) {
		fmt.Fprintf(os.Stderr, "坐标 (%s,%s) 超出屏幕 %d×%d\n", f.pos[0], f.pos[1], w, h)
		os.Exit(3)
	}
	button := "left"
	if action == "right_click" {
		button = "right"
	} else if b, ok := f.kv["button"]; ok {
		button = b
	}
	btnNo := map[string]int{"left": 1, "middle": 2, "right": 3}[button]
	if btnNo == 0 {
		fail(3, "未知按钮: "+button)
	}
	// 指针落位预检 +（可选）目标窗显式置顶校验（--target；2026-09-13 事故二次确认）
	pv, pvx, pvy := warpVerified(sx, sy, 2)
	if !pv {
		fmt.Fprintf(os.Stderr, "⚠ 指针未落位: 目标 (%d,%d) 实际 (%d,%d)，点击可能打偏；建议改纯键盘（windowfocus+key）\n",
			int(sx+0.5), int(sy+0.5), pvx, pvy)
	} else if target, ok := f.kv["target"]; ok {
		twid := resolveWid(target)
		if twid == "" {
			fmt.Fprintf(os.Stderr, "--target %s 解析不到窗口 ID（用 wmctrl -l 的标题或 0xID）\n", target)
			os.Exit(4)
		}
		kids := childWids(twid)
		settleMs := 300
		if s, ok := f.kv["settle"]; ok {
			settleMs = mustInt(s)
		}
		top := topWidAtPointer()
		for a := 0; a < 2 && !(top == twid || kids[top]); a++ {
			xdoOut("windowraise", twid)
			exec.Command("sleep", fmt.Sprintf("%.3f", float64(max(settleMs, 1))/1000.0)).Run()
			top = topWidAtPointer()
		}
		if top != twid && !kids[top] {
			fmt.Fprintf(os.Stderr, "⚠ 指针下顶层窗口 (%s) 不是目标 %s(%s)——有别的窗口盖住了点击点，点击已中止（exit 4）。先处理盖窗（关/最小化/移开），或改用纯键盘（windowfocus+key）\n",
				top, target, twid)
			os.Exit(4)
		}
	} else {
		loc := xdoOut("getmouselocation")
		m := reMouseLoc.FindStringSubmatch(loc)
		wid := ""
		if m != nil {
			wid = m[3]
		}
		if wid != "" && wid != "0" {
			xdoOut("windowraise", wid)
		}
		actId := strings.TrimSpace(xdoOut("getactivewindow"))
		if wid != "" && wid != "0" && wid != actId && !childWids(actId)[wid] {
			fmt.Fprintf(os.Stderr, "⚠ 指针下顶层窗口 (%s) 与激活窗口 (%s) 不一致——可能有窗口盖住点击点；建议加 --target 显式校验\n", wid, actId)
		}
	}
	dbl := action == "double_click" || f.bools["double"]
	var args []string
	if dbl {
		args = []string{"mousemove", strconv.Itoa(int(sx + 0.5)), strconv.Itoa(int(sy + 0.5)),
			"click", "--repeat", "2", "--delay", "100", strconv.Itoa(btnNo)}
	} else {
		args = []string{"mousemove", strconv.Itoa(int(sx + 0.5)), strconv.Itoa(int(sy + 0.5)),
			"click", strconv.Itoa(btnNo)}
	}
	fmt.Fprintf(os.Stderr, "已 %s 屏幕像素 (%d,%d) 按钮=%s%s\n", action, int(sx+0.5), int(sy+0.5), button, map[bool]string{true: " 双击", false: ""}[dbl])
	r := xdo(args)
	if !r.ok {
		fmt.Fprintln(os.Stderr, "xdotool 失败: "+r.err)
		os.Exit(3)
	}
	fmt.Fprintf(os.Stdout, "已点击 (%d,%d)%s\n", int(sx+0.5), int(sy+0.5), map[bool]string{true: "（双击）", false: ""}[dbl])
}

func doDrag(rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 4 {
		usage()
	}
	x1, y1 := toScreen(mustFloat(f.pos[0]), mustFloat(f.pos[1]), f)
	x2, y2 := toScreen(mustFloat(f.pos[2]), mustFloat(f.pos[3]), f)
	fmt.Fprintf(os.Stderr, "已 drag (%d,%d) → (%d,%d)\n", int(x1+0.5), int(y1+0.5), int(x2+0.5), int(y2+0.5))
	r := xdo([]string{
		"mousemove", strconv.Itoa(int(x1 + 0.5)), strconv.Itoa(int(y1 + 0.5)), "mousedown", "1",
		"mousemove", strconv.Itoa(int(x2 + 0.5)), strconv.Itoa(int(y2 + 0.5)), "mouseup", "1",
	})
	if !r.ok {
		fmt.Fprintln(os.Stderr, "xdotool 失败: "+r.err)
		os.Exit(3)
	}
	fmt.Fprintf(os.Stdout, "已拖拽 (%d,%d) → (%d,%d)\n", int(x1+0.5), int(y1+0.5), int(x2+0.5), int(y2+0.5))
}

func doScroll(rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 2 {
		usage()
	}
	sx, sy := toScreen(mustFloat(f.pos[0]), mustFloat(f.pos[1]), f)
	dx := toInt(f.kv["dx"], 0)
	dy := toInt(f.kv["dy"], 0)
	if dx == 0 && dy == 0 {
		fmt.Fprintln(os.Stderr, "scroll 需要 --dx N 和/或 --dy N")
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "已 scroll @ (%d,%d) dx=%d dy=%d\n", int(sx+0.5), int(sy+0.5), dx, dy)
	if r := xdo([]string{"mousemove", strconv.Itoa(int(sx + 0.5)), strconv.Itoa(int(sy + 0.5))}); !r.ok {
		fmt.Fprintln(os.Stderr, "scroll 失败: "+r.err)
		os.Exit(3)
	}
	// xdotool 滚轮：按钮 4=上 5=下 6=左 7=右，--repeat N
	if dy > 0 {
		if r := xdo([]string{"click", "--repeat", strconv.Itoa(dy), "--delay", "40", "5"}); !r.ok {
			fmt.Fprintln(os.Stderr, "scroll 失败: "+r.err)
			os.Exit(3)
		}
	} else if dy < 0 {
		if r := xdo([]string{"click", "--repeat", strconv.Itoa(-dy), "--delay", "40", "4"}); !r.ok {
			fmt.Fprintln(os.Stderr, "scroll 失败: "+r.err)
			os.Exit(3)
		}
	}
	if dx > 0 {
		if r := xdo([]string{"click", "--repeat", strconv.Itoa(dx), "--delay", "40", "7"}); !r.ok {
			fmt.Fprintln(os.Stderr, "scroll 失败: "+r.err)
			os.Exit(3)
		}
	} else if dx < 0 {
		if r := xdo([]string{"click", "--repeat", strconv.Itoa(-dx), "--delay", "40", "6"}); !r.ok {
			fmt.Fprintln(os.Stderr, "scroll 失败: "+r.err)
			os.Exit(3)
		}
	}
	fmt.Fprintf(os.Stdout, "已滚动 dx=%d dy=%d @ (%d,%d)\n", dx, dy, int(sx+0.5), int(sy+0.5))
}

func doType(rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 1 {
		usage()
	}
	text := strings.Join(f.pos, " ")
	wid := focusedWid()
	// 主通道：分段发送——连续可打印 ASCII → xdotool type；非 ASCII 按码点 → key U+码点（实测可靠，免剪贴板）
	type run struct {
		kind string // "type" | "key"
		text string
		keys []string
	}
	var runs []run
	nKey := 0
	for _, ch := range text {
		cp := int(ch)
		if cp >= 0x20 && cp <= 0x7e {
			if len(runs) > 0 && runs[len(runs)-1].kind == "type" {
				runs[len(runs)-1].text += string(ch)
			} else {
				runs = append(runs, run{kind: "type", text: string(ch)})
			}
		} else {
			var k string
			switch cp {
			case 9:
				k = "Tab"
			case 10:
				k = "Return"
			default:
				k = "U" + strings.ToUpper(fmt.Sprintf("%x", cp))
			}
			nKey++
			if len(runs) > 0 && runs[len(runs)-1].kind == "key" {
				runs[len(runs)-1].keys = append(runs[len(runs)-1].keys, k)
			} else {
				runs = append(runs, run{kind: "key", keys: []string{k}})
			}
		}
	}
	wd := ""
	if wid != "" {
		wd = fmt.Sprintf(", 窗口 %s", wid)
	}
	fmt.Fprintf(os.Stderr, "已 type %s（%d 个非 ASCII 码点走 U+keysym%s）\n", jsonQuote(truncate(text, 60)), nKey, wd)
	for _, r := range runs {
		var res xdoResult
		if r.kind == "type" {
			args := []string{"type", "--delay", "12", "--", r.text}
			if wid != "" {
				res = xdo(args, wid)
			} else {
				res = xdo(args)
			}
		} else {
			args := append([]string{"key"}, r.keys...)
			if wid != "" {
				res = xdo(args, wid)
			} else {
				res = xdo(args)
			}
		}
		if !res.ok {
			fmt.Fprintln(os.Stderr, "xdotool 失败: "+res.err)
			os.Exit(3)
		}
	}
	fmt.Printf("已输入 %d 字符\n", len([]rune(text)))
}

func doKey(rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 1 {
		usage()
	}
	combo := f.pos[0]
	for _, re := range dangerous {
		if re.MatchString(strings.ToLower(combo)) {
			fmt.Fprintf(os.Stderr, "危险按键组合已拒绝: %s（要执行请用户手动）\n", combo)
			os.Exit(3)
		}
	}
	wid := focusedWid()
	n := max(1, toInt(f.kv["repeat"], 1))
	interval := toInt(f.kv["interval"], 400)
	for i := 1; i <= n; i++ {
		if i > 1 {
			sleepMs(interval)
		}
		if wid != "" {
			fmt.Fprintf(os.Stderr, "已 key %s（第 %d/%d 次）（窗口 %s）\n", combo, i, n, wid)
		} else {
			fmt.Fprintf(os.Stderr, "已 key %s（第 %d/%d 次）（全局）\n", combo, i, n)
		}
		var r xdoResult
		if wid != "" {
			r = xdo([]string{"key", combo}, wid)
		} else {
			r = xdo([]string{"key", combo})
		}
		if !r.ok {
			fmt.Fprintln(os.Stderr, "xdotool 失败: "+r.err)
			os.Exit(3)
		}
	}
	if n > 1 {
		fmt.Printf("已按键 %s ×%d（间隔 %dms）\n", combo, n, interval)
	} else {
		fmt.Printf("已按键 %s\n", combo)
	}
}

func doHold(rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 2 {
		usage()
	}
	combo, ms := f.pos[0], f.pos[1]
	for _, re := range dangerous {
		if re.MatchString(strings.ToLower(combo)) {
			fmt.Fprintf(os.Stderr, "危险按键组合已拒绝: %s（要执行请用户手动）\n", combo)
			os.Exit(3)
		}
	}
	fmt.Fprintf(os.Stderr, "已 hold %s %sms\n", combo, ms)
	a := xdo([]string{"keydown", combo})
	sleepMs(toInt(ms, 500))
	b := xdo([]string{"keyup", combo})
	if !a.ok || !b.ok {
		fmt.Fprintln(os.Stderr, "hold 失败")
		os.Exit(3)
	}
	fmt.Fprintf(os.Stdout, "已按住 %s %sms\n", combo, ms)
}

func doWake(rest []string) {
	if !screensaverActive() {
		fmt.Println("未锁屏")
		return
	}
	pass := ""
	if len(rest) > 0 {
		pass = rest[0]
	} else {
		pass = os.Getenv("OKHUMAN_UNLOCK_PASSWORD")
	}
	if pass == "" {
		fmt.Fprintln(os.Stderr, "锁屏中，需要密码: xusectl wake <密码>（或 env OKHUMAN_UNLOCK_PASSWORD）")
		os.Exit(3)
	}
	fmt.Fprintln(os.Stderr, "wake: 向锁屏密码框输入密码…")
	// 锁屏对话框默认聚焦密码输入框，全局 XTest 输入即可
	xdo([]string{"type", "--clearmodifiers", "--delay", "30", "--", pass})
	xdo([]string{"key", "Return"})
	sleepMs(2000)
	if screensaverActive() {
		fmt.Fprintln(os.Stderr, "解锁失败（密码错误，或焦点不在密码框）")
		os.Exit(3)
	}
	fmt.Println("已解锁")
}

func doFocus(rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 1 {
		usage()
	}
	target := f.pos[0]
	// 窗口 ID（0x 开头或纯数字）直接用；否则按名字搜（wmctrl 列表 + xdotool search）
	wid := ""
	if reDecID.MatchString(target) {
		wid = target
	}
	if wid == "" {
		cmd := exec.Command("wmctrl", "-lp")
		cmd.Env = append(os.Environ(), "DISPLAY="+display)
		out, _ := cmd.Output()
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, target) {
				fld := strings.Fields(line)
				if len(fld) > 0 {
					wid = fld[0]
				}
				break
			}
		}
	}
	if wid == "" {
		fmt.Fprintf(os.Stderr, "找不到窗口: %s\n", target)
		os.Exit(3)
	}
	r := xdo([]string{"windowfocus", "--sync", wid})
	if !r.ok {
		fmt.Fprintln(os.Stderr, "聚焦失败: "+r.err)
		os.Exit(3)
	}
	os.WriteFile(focusFile, []byte(wid+"\n"+target+"\n"), 0o600)
	os.Chmod(focusFile, 0o600) // 旧文件可能以 0644 创建：0644 时其他本地用户可改焦点劫持输入
	fmt.Fprintf(os.Stderr, "已 focus %s（%s）\n", target, wid)
	fmt.Fprintf(os.Stdout, "已聚焦窗口 %s（%s）——后续 type/key 定向投递到该窗口\n", target, wid)
}

func doWindow(action string, rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 1 {
		usage()
	}
	wid := resolveWid(f.pos[0])
	if wid == "" {
		fmt.Fprintf(os.Stderr, "解析不到窗口: %s（用 wmctrl -l 的完整标题或 0xID/十进制ID）\n", f.pos[0])
		os.Exit(3)
	}
	switch action {
	case "windowactivate":
		r := xdo([]string{"windowactivate", "--sync", wid})
		if !r.ok {
			fmt.Fprintln(os.Stderr, "windowactivate 失败: "+r.err)
			os.Exit(3)
		}
		fmt.Fprintf(os.Stderr, "已 windowactivate %s（%s）\n", f.pos[0], wid)
		fmt.Fprintf(os.Stdout, "已激活 %s（%s）——xfwm4 置顶有 100-250ms 延迟，点击前先用 stack 核对顶层\n", f.pos[0], wid)
	case "windowminimize":
		r := xdo([]string{"windowminimize", wid})
		if !r.ok {
			fmt.Fprintln(os.Stderr, "windowminimize 失败: "+r.err)
			os.Exit(3)
		}
		fmt.Fprintf(os.Stderr, "已 windowminimize %s（%s）\n", f.pos[0], wid)
		fmt.Fprintf(os.Stdout, "已最小化 %s（%s）\n", f.pos[0], wid)
	case "windowraise":
		r := xdo([]string{"windowraise", wid})
		if !r.ok {
			fmt.Fprintln(os.Stderr, "windowraise 失败: "+r.err)
			os.Exit(3)
		}
		fmt.Fprintf(os.Stderr, "已 windowraise %s（%s）\n", f.pos[0], wid)
		fmt.Fprintf(os.Stdout, "已置顶 %s（%s）（windowraise 立即生效，无 activate 延迟）\n", f.pos[0], wid)
	default:
		isMax := action == "windowmaximize"
		hex := fmt.Sprintf("0x%x", toInt(wid, 0))
		cmd := exec.Command("wmctrl", "-i", "-r", hex, "-b", map[bool]string{true: "add,maximized_vert,maximized_horz", false: "remove,maximized_vert,maximized_horz"}[isMax])
		cmd.Env = append(os.Environ(), "DISPLAY="+display)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintln(os.Stderr, "wmctrl 失败: "+truncate(string(out), 200))
			os.Exit(3)
		}
		fmt.Fprintf(os.Stderr, "已 %s %s（%s）\n", action, f.pos[0], wid)
		if isMax {
			fmt.Fprintf(os.Stdout, "已最大化 %s（%s）\n", f.pos[0], wid)
		} else {
			fmt.Fprintf(os.Stdout, "已取消最大化 %s（%s）\n", f.pos[0], wid)
		}
	}
}

func doStack(rest []string) {
	f := parseFlags(rest)
	if len(f.pos) >= 2 {
		px, py := mustFloat(f.pos[0]), mustFloat(f.pos[1])
		xdoOut("mousemove", strconv.Itoa(int(px)), strconv.Itoa(int(py)))
		sleepMs(150)
	}
	// 窗口名映射
	nameMap := map[string]string{}
	cmd := exec.Command("wmctrl", "-l")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	wl, _ := cmd.Output()
	for _, l := range strings.Split(string(wl), "\n") {
		fld := strings.Fields(l)
		if len(fld) >= 4 {
			nameMap[toDecId(fld[0])] = strings.Join(fld[3:], " ")
		}
	}
	nm := func(id string) string {
		if v, ok := nameMap[id]; ok {
			return v
		}
		return "(无 wmctrl 条目)"
	}
	loc := xdoOut("getmouselocation")
	m := reMouseLoc.FindStringSubmatch(loc)
	top, xq, yq := "?", "?", "?"
	if m != nil {
		xq, yq, top = m[1], m[2], m[3]
	}
	active := strings.TrimSpace(xdoOut("getactivewindow"))
	fmt.Fprintf(os.Stderr, "指针 @ (%s,%s)\n", xq, yq)
	topHex := "?"
	if top != "?" {
		topHex = fmt.Sprintf("0x%x", toInt(top, 0))
	}
	activeHex := "?"
	if active != "" {
		activeHex = fmt.Sprintf("0x%x", toInt(active, 0))
	}
	same := ""
	if top == active {
		same = "（=active）"
	}
	fmt.Fprintf(os.Stdout, "顶层窗口: %s (%s) %s%s\n", top, topHex, nm(top), same)
	fmt.Fprintf(os.Stdout, "active 窗口: %s (%s) %s\n", active, activeHex, nm(active))
	cmd = exec.Command("xprop", "-root", "_NET_CLIENT_LIST_STACKING")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	ev, _ := cmd.Output()
	hexIds := regexp.MustCompile(`0x[0-9a-f]+`).FindAllString(string(ev), -1)
	var parts []string
	for _, h := range hexIds {
		d := toDecId(h)
		parts = append(parts, d+" "+nm(d))
	}
	fmt.Fprintf(os.Stdout, "EWMH 堆叠序（底→顶；含图标化窗、可能失真，判点击归属以顶层为准）: %s\n", strings.Join(parts, " ← "))
}

func doOpen(rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 1 {
		usage()
	}
	cmdline := strings.Join(f.pos, " ")
	fmt.Fprintf(os.Stderr, "已 open: %s（setsid 脱离）\n", cmdline)
	logf := filepath.Join(os.TempDir(), fmt.Sprintf("xuse-open-%s.log", nowBase36()))
	c := exec.Command("bash", "-c", fmt.Sprintf("setsid %s < /dev/null > %s 2>&1 & echo $!", cmdline, logf))
	c.Env = append(os.Environ(), "DISPLAY="+display)
	out, err := c.CombinedOutput()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open 失败: "+truncate(string(out), 200))
		os.Exit(3)
	}
	pid := strings.TrimSpace(string(out))
	fmt.Fprintf(os.Stdout, "已启动 %s（pid %s，日志 %s）\n", cmdline, pid, logf)
}

func doDiff(rest []string) {
	f := parseFlags(rest)
	if len(f.pos) < 1 || !exists(f.pos[0]) {
		fmt.Fprintln(os.Stderr, "diff 需要存在的 prev.png")
		os.Exit(2)
	}
	prev := f.pos[0]
	w, h := screenSize()
	newOut := ""
	if s, ok := f.kv["out"]; ok && s != "" {
		newOut = s
	} else {
		newOut = filepath.Join(os.TempDir(), fmt.Sprintf("xuse-diff-%s.png", nowBase36()))
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "x11grab",
		"-video_size", fmt.Sprintf("%dx%d", w, h), "-i", display, "-frames:v", "1", "-y", newOut)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "截图失败")
		os.Exit(3)
	}
	// --region x,y,w,h：只比局部（小窗口在全屏 YAVG 里会被稀释到 0）；crop 要分别作用在每个输入上
	fc := "[0][1]blend=all_mode=difference,signalstats,metadata=print"
	if region, ok := f.kv["region"]; ok {
		ps := strings.Split(region, ",")
		if len(ps) != 4 {
			fail(2, fmt.Sprintf("--region 格式应为 x,y,w,h（收到 %s）", region))
		}
		var rx, ry, rw, rh float64
		for i, v := range ps {
			vv, err := strconv.ParseFloat(v, 64)
			if err != nil || !isFinite(vv) {
				fail(2, fmt.Sprintf("--region 格式应为 x,y,w,h（收到 %s）", region))
			}
			switch i {
			case 0:
				rx = vv
			case 1:
				ry = vv
			case 2:
				rw = vv
			case 3:
				rh = vv
			}
		}
		if rx < 0 || ry < 0 || rw <= 0 || rh <= 0 {
			fail(2, fmt.Sprintf("--region 格式应为 x,y,w,h（收到 %s）", region))
		}
		c := fmt.Sprintf("crop=%d:%d:%d:%d", int(rw), int(rh), int(rx), int(ry))
		fc = fmt.Sprintf("[0]%s[a];[1]%s[b];[a][b]blend=all_mode=difference,signalstats,metadata=print", c, c)
	}
	d := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "info", "-i", prev, "-i", newOut,
		"-filter_complex", fc, "-frames:v", "1", "-f", "null", "-")
	d.Env = append(os.Environ(), "DISPLAY="+display)
	dout, _ := d.CombinedOutput()
	yavg, ymax := "0", "0"
	if m := reYAVG.FindStringSubmatch(string(dout)); m != nil {
		yavg = m[1]
	}
	if m := reYMAX.FindStringSubmatch(string(dout)); m != nil {
		ymax = m[1]
	}
	changed := parseFloatDefault(yavg) > 0.5
	fmt.Fprintf(os.Stderr, "diff: YAVG=%s YMAX=%s\n", yavg, ymax)
	if changed {
		fmt.Fprintf(os.Stdout, "页面有变化（平均差 %s）——差异图已存 %s（okattach 注入可看具体变在哪）\n", yavg, newOut)
	} else {
		fmt.Fprintf(os.Stdout, "页面无变化（平均差 %s≈0）——动作可能没生效\n", yavg)
	}
}

// ---------- 小工具 ----------

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func nowBase36() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 36)
}

func mustInt(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

func mustFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		usage()
	}
	return v
}

func toInt(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func isFinite(f float64) bool {
	return f == f && f*0 == 0
}

func parseFloatDefault(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// JSON 字符串转义（xusectl type 的回显用；与 TS JSON.stringify 前 60 字一致）
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var _ = filepath.Join
