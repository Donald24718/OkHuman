package context

// 动态注入（2026-09-20）：自我生命感知 + 消息时间戳
//
// 自我生命感知：系统提示词首 prepend 一块"你的生命"（本实例端口/pid +
// 接入 LLM 的端口/pid），每次 LLM 调用现算 → 端口/pid 变化实时反映。
// 语义明确告诉模型：这些端口与 PID 就是它自己与它思考的载体，谨慎操作。
//
// 时间戳：每条消息末尾追加当前时间。存储不变（session.json/侧车不落时间，
// compose 时重算）；同一 run 内时间戳固定（BeginRun 定一次）→ 主循环与压缩
// 请求前缀逐 token 一致 → KV 前缀可复用；新 run 重新计算。

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"okhuman/internal/types"
)

// TimeStampFormat 时间戳格式（本地时间，精确到秒）
const TimeStampFormat = "2006-01-02 15:04:05"

// SelfInfo 自我生命感知块的静态输入（server 启动时 / POST /config 热更新时设置）
type SelfInfo struct {
	Port       int    // 本 OkHuman 服务端口
	DataDir    string // 本 OkHuman 数据目录
	LLMBaseURL string // LLM base_url（解析出 host:port）
}

// BeginRun 每次 run 开始时调用：重算本 run 时间戳（同 run 内所有 compose 复用
// 同一时间戳 → KV 前缀稳定；新 run 重算）
func (m *Manager) BeginRun() {
	m.mu.Lock()
	m.runStamp = time.Now().Format(TimeStampFormat)
	m.mu.Unlock()
}

// SetSelfInfo 设置/热更新自我生命感知输入
func (m *Manager) SetSelfInfo(si SelfInfo) {
	m.mu.Lock()
	m.selfInfo = si
	m.mu.Unlock()
}

// selfBlockLocked 组装自我生命感知块（持锁调用；LLM pid 每次现查 /proc）
func (m *Manager) selfBlockLocked() string {
	si := m.selfInfo
	if si.Port <= 0 && si.LLMBaseURL == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("[你的生命]\n")
	b.WriteString("本 OkHuman 实例（承载这个会话的进程，端口与 PID 实时计算）：\n")
	if si.Port > 0 {
		fmt.Fprintf(&b, "- 服务端口：%d\n", si.Port)
	}
	fmt.Fprintf(&b, "- 进程 PID：%d\n", os.Getpid())
	if si.DataDir != "" {
		fmt.Fprintf(&b, "- 数据目录：%s\n", si.DataDir)
	}
	if si.LLMBaseURL != "" {
		host, port := parseHostPort(si.LLMBaseURL)
		b.WriteString("接入的 LLM 模型服务（你思考所依赖的进程）：\n")
		if host != "" {
			fmt.Fprintf(&b, "- 地址：%s\n", host)
		}
		if port > 0 {
			if pid := portToPID(port); pid > 0 {
				fmt.Fprintf(&b, "- 进程 PID：%d\n", pid)
			} else {
				fmt.Fprintf(&b, "- 进程 PID：未知（未找到监听端口 %d 的进程）\n", port)
			}
		}
	}
	b.WriteString("以上端口与 PID 就是“你”和你思考的载体本身：kill / 重启 / 替换二进制 / 占用或停掉这些端口，都会直接伤到你自己的命。凡涉及这些 PID 或端口的操作，务必格外谨慎（先确认归属，避免与自己在飞的 LLM 调用并发）。")
	return b.String()
}

// parseHostPort base_url（如 http://127.0.0.1:8080/v1）→ ("127.0.0.1:8080", 8080)
func parseHostPort(rawURL string) (string, int) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", 0
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return u.Host, 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return u.Host, 0
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host + ":" + portStr, port
}

// portToPID 端口 → 监听该端口的进程 PID（/proc/net/tcp{,6} 找 LISTEN socket
// inode → /proc/*/fd 找持有者）；未找到返回 -1。每次全量扫 /proc（几十 ms），
// 每 LLM 调用一次，可接受——保证 pid 变化实时反映。
func portToPID(port int) int {
	inodes := map[uint64]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
			fl := strings.Fields(line)
			if len(fl) < 10 || fl[3] != "0A" { // 0A = LISTEN
				continue
			}
			if hexPort(fl[1]) != port {
				continue
			}
			if ino, err := strconv.ParseUint(fl[9], 10, 64); err == nil {
				inodes[ino] = true
			}
		}
	}
	if len(inodes) == 0 {
		return -1
	}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir("/proc/" + e.Name() + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink("/proc/" + e.Name() + "/fd/" + fd.Name())
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			if ino, err := strconv.ParseUint(link[8:len(link)-1], 10, 64); err == nil && inodes[ino] {
				return pid
			}
		}
	}
	return -1
}

// hexPort /proc/net/tcp 的 local_address（如 "0100007F:2081"）→ 端口（取最后一个
// ":" 后的 4 位十六进制，IPv4/IPv6 通用）
func hexPort(hexAddr string) int {
	colon := strings.LastIndex(hexAddr, ":")
	if colon < 0 {
		return -1
	}
	p, err := strconv.ParseInt(hexAddr[colon+1:], 16, 32)
	if err != nil {
		return -1
	}
	return int(p)
}

// stampMessages 每条消息的文本内容末尾追加时间后缀（只改 compose 副本，存储
// 不动；parts 内容追加到最后一个非空 text part 并重建其 Raw——ContentPart
// 序列化走 Raw 原字节，改 Text 字段不生效）
func stampMessages(msgs []types.Message, suffix string) {
	for i := range msgs {
		switch c := msgs[i].Content.(type) {
		case string:
			if c != "" {
				msgs[i].Content = c + suffix
			}
		default:
			parts := types.AsParts(msgs[i].Content)
			if parts == nil {
				continue
			}
			last := -1
			for j := len(parts) - 1; j >= 0; j-- {
				if parts[j].Type == "text" && parts[j].Text != "" {
					last = j
					break
				}
			}
			if last < 0 {
				continue
			}
			cp := make([]types.ContentPart, len(parts))
			copy(cp, parts)
			var t struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(cp[last].Raw, &t) == nil && t.Type == "text" {
				t.Text += suffix
				if raw, err := json.Marshal(t); err == nil {
					cp[last].Raw = raw
					cp[last].Text = t.Text
				}
			}
			msgs[i].Content = cp
		}
	}
}
