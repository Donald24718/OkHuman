// okattach —— 素材注入 OkHuman 上下文（图片 / 视频 / 单帧；音频暂不支持）。
// 图片：默认原图注入（native，不缩放不压质），--scaled 压宽 800px/JPEG q2 省 token；
//
//	--png 走 PNG 无损（截图类素材）。
//
// 视频：直通 input_video part（llama-server 服务端抽帧+时间戳，比插件侧抽帧
//
//	    时间定位准）。策略（2026-09-20，用户定）：
//	- 源宽 < 360px → 原样注入（不放大不压缩，保清晰）；
//	- 源宽 ≥ 360px → 压到宽 360px（360p 档）注入；
//	- 每段 ≤ 1 万 tokens；360p 档单段另加 ≤ 60s 上限；
//	- 超上限自动分段，--seg N 注入第 N 段（段内时间戳从 0 起，文本 part 标注
//	  原时间轴范围）；
//	- 估算式 est_tps = W*H/420（:8080 实测 4fps 抽帧+2 帧时序 merge 的标定值，
//	  360x179≈154 tps → 60s≈9.3k tokens）；
//	- 清晰度不够 → okattach frame 抽单帧高清注入；
//	- 长内容分批 → 分段注入（--seg）。
//
// 用法：okattach <file> [--scaled] [--png] [--note "..."]
//
//	okattach video <file> [--seg N] [--note "..."] [--dry-run]
//	okattach frame <video> <t秒> [--w 宽] [--note "..."]
//
// 目标端口自动识别：本插件目录向上找 OkHuman 父目录，用其 config.json 的
// server.port（各实例树独立，端口从进程父链取，不用写死）。
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxBytes = 64 << 20 // 64MB，与 /inject 侧车上限一致

var magic = map[string]string{ // 扩展名 → mime
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".gif": "image/gif", ".bmp": "image/bmp",
	".mp4": "video/mp4", ".mov": "video/quicktime", ".mkv": "video/x-matroska",
	".webm": "video/webm",
}

// ---------- 小工具 ----------

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "okattach: "+format+"\n", a...)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `okattach —— 素材注入 OkHuman 上下文

用法:
  okattach <file>                      图片：原图注入（native，默认）
  okattach <file> --scaled             图片：压宽 800px / JPEG q2（省 token）
  okattach <file> --png                图片：压宽后走 PNG 无损（截图类素材）
  okattach video <file> [--seg N]      视频：按策略编码后直通 input_video 注入
  okattach video <file> --dry-run      视频：只打印计划（分辨率/分段/token 估算），不编码不 POST
  okattach frame <video> <t> [--w W]   视频抽 t 秒单帧，压宽 W（默认 1280）注入为图片
  以上均可加 --note "说明"（/attachments 里可见，不进 LLM 上下文）

视频策略（:8080 实测标定，详见 README）:
  源宽 <360px → 原样注入；源宽 ≥360px → 压到宽 360px
  每段 ≤1 万 tokens；360p 档单段 ≤60s；超上限自动分段
  段内时间戳从 0 起，注入文本标注原时间轴范围
  清晰度不够 → 用 frame 抽单帧高清；长内容 → --seg 分批

目标端口自动识别（本插件目录的 OkHuman 父目录 config.json 的 server.port）。
`)
	os.Exit(2)
}

type flag struct {
	note   string
	scaled bool
	png    bool
	dryRun bool
	seg    int // 0 = 未指定（视频单段/自动）
	frameW int // frame 子命令目标宽
}

func parseFlags(args []string) (flag, []string) {
	var f flag
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--note":
			if i+1 >= len(args) {
				die("--note 需要参数")
			}
			i++
			f.note = args[i]
		case a == "--scaled":
			f.scaled = true
		case a == "--png":
			f.png = true
		case a == "--dry-run":
			f.dryRun = true
		case a == "--seg":
			if i+1 >= len(args) {
				die("--seg 需要参数")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				die("--seg 须为正整数: %s", args[i])
			}
			f.seg = n
		case a == "--w":
			if i+1 >= len(args) {
				die("--w 需要参数")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 16 {
				die("--w 须为 ≥16 的整数: %s", args[i])
			}
			f.frameW = n
		default:
			pos = append(pos, a)
		}
	}
	return f, pos
}

// targetPort 沿进程父链找所属 okhuman 主进程，取其 LISTEN 端口（插件由 agent 的 bash 工具启动，
// 父链 okattach←bash←okhuman，端口随实例天然隔离）；找不到回退 8451 并提示
func targetPort() int {
	if ports := detectOwnPorts(); len(ports) > 0 {
		return ports[0]
	}
	fmt.Fprintln(os.Stderr, "okattach: 父链未找到 okhuman 主进程，回退默认端口 8451（如属错误实例请用 --okhuman-port 显式指定）")
	return 8451
}

func ppidOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	m := regexp.MustCompile(`(?m)^PPid:\s+(\d+)`).FindSubmatch(b)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

// isOkHumanMain pid 是否为 OkHuman 主程序（Go 单文件二进制：argv0 基名为 okhuman）
func isOkHumanMain(pid int) bool {
	cmd, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	parts := strings.Split(string(cmd), "\x00")
	if len(parts) == 0 || parts[0] == "" {
		return false
	}
	return filepath.Base(strings.TrimRight(parts[0], "\x00")) == "okhuman"
}

func listenPortsOf(pid int) []int {
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return nil
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		lnk, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
		if err != nil {
			continue // fd 瞬移，忽略
		}
		if m := regexp.MustCompile(`^socket:\[(\d+)\]$`).FindStringSubmatch(lnk); m != nil {
			inodes[m[1]] = true
		}
	}
	ports := map[int]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			c := strings.Fields(strings.TrimSpace(line))
			if len(c) < 10 || c[3] != "0A" { // 0A = LISTEN；inode 在第 10 列
				continue
			}
			if inodes[c[9]] {
				p, err := strconv.ParseInt(strings.Split(c[1], ":")[1], 16, 32)
				if err == nil && p > 0 {
					ports[int(p)] = true
				}
			}
		}
	}
	out := make([]int, 0, len(ports))
	for p := range ports {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// detectOwnPorts 沿父进程链找 OkHuman 主程序，返回其 LISTEN 端口（找不到返回 nil）
func detectOwnPorts() []int {
	pid := os.Getppid()
	for i := 0; i < 32 && pid > 1; i++ {
		if isOkHumanMain(pid) {
			return listenPortsOf(pid)
		}
		pid = ppidOf(pid)
	}
	return nil
}

func runCmd(cmd string, args ...string) (string, error) {
	c := exec.Command(cmd, args...)
	var buf bytes.Buffer
	c.Stdout = &buf
	var errbuf bytes.Buffer
	c.Stderr = &errbuf
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s: %v\n%s", cmd, err, errbuf.String())
	}
	return buf.String(), nil
}

// probeVideo ffprobe 视频流宽/高/时长
type probe struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Dur    float64 `json:"dur"`
}

func probeVideo(file string) (probe, error) {
	out, err := runCmd("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-show_entries", "format=duration", "-of", "json", file)
	if err != nil {
		return probe{}, err
	}
	var j struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		return probe{}, fmt.Errorf("ffprobe 输出解析失败: %w", err)
	}
	if len(j.Streams) == 0 || j.Streams[0].Width == 0 {
		return probe{}, fmt.Errorf("无视频流")
	}
	dur, err := strconv.ParseFloat(strings.TrimSpace(j.Format.Duration), 64)
	if err != nil || dur <= 0 {
		return probe{}, fmt.Errorf("时长解析失败: %q", j.Format.Duration)
	}
	return probe{j.Streams[0].Width, j.Streams[0].Height, dur}, nil
}

// ---------- 注入 ----------

type part struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
	InputVideo *struct {
		Data string `json:"data,omitempty"`
		URL  string `json:"url,omitempty"`
	} `json:"input_video,omitempty"`
}

func postInject(port int, parts []part, note string) (int, string) {
	payload, err := json.Marshal(map[string]any{"content": parts, "note": note})
	if err != nil {
		return 0, "序列化失败: " + err.Error()
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/inject", port), "application/json", bytes.NewReader(payload))
	if err != nil {
		return 0, "连接失败: " + err.Error()
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(buf)
		if n > 0 {
			body.Write(buf[:n])
		}
		if e != nil {
			break
		}
	}
	return resp.StatusCode, body.String()
}

func ok(status int, detail string) {
	if status != 200 {
		die("注入失败 %s", detail)
	}
	var m struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(detail), &m)
	fmt.Printf("ok id=%s %s\n", m.ID, detail)
}

// even n 向下取偶（ffmpeg yuv420p 要求宽高为偶）
func even(n int) int { return n - n%2 }

// estTPS 每秒 token 估算（:8080 实测标定：4fps 抽帧+2 帧时序 merge，
// 有效 patch≈30px → tokens/s ≈ W*H/420；对 720p 高估 ~13%、320p 基本持平，
// 宁多勿少——超 1 万 tokens 是硬约束）
func estTPS(w, h int) float64 { return float64(w) * float64(h) / 420.0 }

// ---------- main ----------

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	sub := os.Args[1]
	f, pos := parseFlags(os.Args[2:])
	port := targetPort()

	switch sub {
	case "video":
		if len(pos) != 1 {
			usage()
		}
		file := pos[0]
		ext := strings.ToLower(filepath.Ext(file))
		mime, known := magic[ext]
		if !known || !strings.HasPrefix(mime, "video/") {
			die("视频须为 mp4/mov/mkv/webm（ffprobe 可用时其它容器也可）: %s", file)
		}
		st, err := os.Stat(file)
		if err != nil {
			die("读文件失败: %v", err)
		}
		if st.Size() > maxBytes {
			die("文件超过 64MB 上限: %s", file)
		}
		p, err := probeVideo(file)
		if err != nil {
			die("ffprobe 失败: %v", err)
		}

		// 目标分辨率：源宽 <360 原样；≥360 压到宽 360（360p 档）
		tw, th, compressed := p.Width, p.Height, false
		if p.Width >= 360 {
			tw = 360
			th = even(int(math.Round(float64(p.Height) * 360.0 / float64(p.Width))))
			compressed = true
		}
		// 分段：每段 ≤1 万 tokens；360p 档单段 ≤60s
		tps := estTPS(tw, th)
		maxSeg := 10000.0 / tps
		if compressed {
			if maxSeg > 60 {
				maxSeg = 60
			}
		}
		nSeg := int(math.Ceil(p.Dur/maxSeg - 1e-9))
		if nSeg < 1 {
			nSeg = 1
		}
		segDur := p.Dur / float64(nSeg)
		if f.seg > nSeg {
			die("--seg %d 超出段数 %d（总时长 %.1fs，单段 ≤%.0fs）", f.seg, nSeg, p.Dur, maxSeg)
		}

		mode := "原样"
		if compressed {
			mode = fmt.Sprintf("压至 360p(%dx%d)", tw, th)
		}
		plan := fmt.Sprintf("视频 %s: %.1fs, %dx%d → %s, 估算 %.0f tps",
			file, p.Dur, p.Width, p.Height, mode, tps)
		if nSeg > 1 {
			plan += fmt.Sprintf(", 共 %d 段 × ≤%.0fs（单段 ≈%.0f tokens）", nSeg, maxSeg, tps*maxSeg)
		} else {
			plan += fmt.Sprintf(", 单段 ≈%.0f tokens", tps*p.Dur)
		}
		seg := f.seg
		if seg < 1 {
			seg = 1
		}
		if seg > nSeg {
			die("--seg %d 超出段数 %d（总时长 %.1fs，单段 ≤%.0fs）", seg, nSeg, p.Dur, maxSeg)
		}
		start := float64(seg-1) * segDur
		end := math.Min(start+segDur, p.Dur)
		if seg == nSeg {
			end = p.Dur // 末段吃到尾（浮点余数归末段）
		}
		segLen := end - start
		if f.dryRun {
			fmt.Printf("dry-run（目标端口 %d，父链自识别）: %s\n", port, plan)
			if nSeg > 1 || f.seg > 0 {
				fmt.Printf("  第 %d/%d 段: 原时间轴 %.1f-%.1fs (≈%.0f tokens)\n", seg, nSeg, start, end, tps*segLen)
			}
			return
		}

		// 编码目标段（-ss 放 -i 前：快 seek + 丢弃到 t 帧精确 + 输出时间戳归零）
		out := filepath.Join(os.TempDir(), fmt.Sprintf("okattach-%d.mp4", os.Getpid()))
		defer os.Remove(out)
		encArgs := []string{"-v", "error", "-ss", fmt.Sprintf("%.3f", start), "-i", file,
			"-t", fmt.Sprintf("%.3f", segLen), "-an",
			"-c:v", "libx264", "-crf", "23", "-preset", "fast", "-pix_fmt", "yuv420p"}
		if compressed {
			encArgs = append(encArgs, "-vf", fmt.Sprintf("scale=%d:%d", tw, th))
		}
		encArgs = append(encArgs, out)
		if _, err := runCmd("ffmpeg", encArgs...); err != nil {
			die("ffmpeg 编码失败: %v", err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			die("读编码产物失败: %v", err)
		}

		// 注入文本：标注原时间轴（段内时间戳从 0 起，模型靠这段映射回原时间轴）
		text := fmt.Sprintf("视频 %s（%s，时长 %.1fs）", file, mode, p.Dur)
		if nSeg > 1 {
			text = fmt.Sprintf("视频 %s 第 %d/%d 段（%s，总时长 %.1fs）：原时间轴 %.1f-%.1fs，段内时间戳从 0 起",
				file, seg, nSeg, mode, p.Dur, start, end)
			if seg > 1 {
				text += fmt.Sprintf("（第 1-%d 段已分别注入，注意跨段衔接）", seg-1)
			}
		}
		parts := []part{{Type: "text", Text: text}}
		b64 := base64.StdEncoding.EncodeToString(data)
		parts = append(parts, part{Type: "input_video", InputVideo: &struct {
			Data string `json:"data,omitempty"`
			URL  string `json:"url,omitempty"`
		}{Data: b64}})
		status, detail := postInject(port, parts, f.note)
		ok(status, detail)
		fmt.Printf("  段 %d/%d: %.1f-%.1fs, 编码后 %dKB, 估算 ≈%.0f tokens\n",
			seg, nSeg, start, end, len(data)/1024, tps*segLen)

	case "frame":
		if len(pos) != 2 {
			usage()
		}
		file, tstr := pos[0], pos[1]
		t, err := strconv.ParseFloat(tstr, 64)
		if err != nil || t < 0 {
			die("帧时间须为 ≥0 的秒数: %s", tstr)
		}
		p, err := probeVideo(file)
		if err != nil {
			die("ffprobe 失败: %v", err)
		}
		if t > p.Dur {
			die("帧时间 %.1fs 超出视频时长 %.1fs", t, p.Dur)
		}
		w := f.frameW
		if w < 16 {
			w = 1280 // 默认 1280 宽（清晰度升级档，约 1.1k tokens/帧）
		}
		if f.dryRun {
			fmt.Printf("dry-run（目标端口 %d）: 视频 %s t=%.1fs → 抽帧压宽 %dpx 注入（不编码不 POST）\n", port, file, t, w)
			return
		}
		out := filepath.Join(os.TempDir(), fmt.Sprintf("okattach-%d.jpg", os.Getpid()))
		defer os.Remove(out)
		// -ss 在 -i 前：快 seek + 精确到 t 帧（丢弃关键帧到 t 之间的帧）
		if _, err := runCmd("ffmpeg", "-v", "error", "-ss", fmt.Sprintf("%.3f", t), "-i", file,
			"-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:-2", w), "-q:v", "2", out); err != nil {
			die("ffmpeg 抽帧失败: %v", err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			die("读帧失败: %v", err)
		}
		b64 := base64.StdEncoding.EncodeToString(data)
		text := fmt.Sprintf("视频 %s t=%.1fs 的帧（压宽 %dpx）", file, t, w)
		parts := []part{
			{Type: "text", Text: text},
			{Type: "image_url", ImageURL: &struct {
				URL string `json:"url"`
			}{URL: "data:image/jpeg;base64," + b64}},
		}
		status, detail := postInject(port, parts, f.note)
		ok(status, detail)
		fmt.Printf("  t=%.1fs, 宽 %dpx, %dKB\n", t, w, len(data)/1024)

	default:
		// 图片路径（子命令位置直接给文件；pos 只应含文件之后的多余位置参数）
		if len(pos) != 0 {
			usage()
		}
		file := sub
		ext := strings.ToLower(filepath.Ext(file))
		mime, known := magic[ext]
		if !known {
			die("不支持的素材类型: %s（图片 png/jpg/webp/gif/bmp；视频/音频用 video 子命令）", ext)
		}
		if strings.HasPrefix(mime, "audio/") {
			die("音频暂不支持（ASR 插件负责）: %s", file)
		}
		st, err := os.Stat(file)
		if err != nil {
			die("读文件失败: %v", err)
		}
		if st.Size() > maxBytes {
			die("文件超过 64MB 上限: %s", file)
		}

		data, err := os.ReadFile(file)
		if err != nil {
			die("读文件失败: %v", err)
		}

		b64, imgMime := base64.StdEncoding.EncodeToString(data), mime
		if f.scaled || f.png {
			out := filepath.Join(os.TempDir(), fmt.Sprintf("okattach-%d.%s", os.Getpid(), ext[1:]))
			defer os.Remove(out)
			encArgs := []string{"-v", "error", "-i", file, "-vf", "scale='min(800,iw)':-2", "-an"}
			if f.png {
				encArgs = append(encArgs, out)
			} else {
				encArgs = append(encArgs, "-q:v", "2", out)
			}
			if _, err := runCmd("ffmpeg", encArgs...); err != nil {
				die("ffmpeg 压缩失败: %v", err)
			}
			data, err = os.ReadFile(out)
			if err != nil {
				die("读压缩产物失败: %v", err)
			}
			b64 = base64.StdEncoding.EncodeToString(data)
			if !f.png {
				imgMime = "image/jpeg"
			}
		}

		parts := []part{{
			Type: "image_url",
			ImageURL: &struct {
				URL string `json:"url"`
			}{URL: "data:" + imgMime + ";base64," + b64},
		}}
		status, detail := postInject(port, parts, f.note)
		ok(status, detail)
	}
}
