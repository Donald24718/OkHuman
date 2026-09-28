package context

// 动态注入（2026-09-20）：自我生命感知
//
// 系统提示词首 prepend 一块"你的生命"（本实例端口/pid/源码位置 + 接入
// LLM 的端口/pid），每次 LLM 调用现算 → 端口/pid/源码位置变化实时反映。语义明确告诉模型：
// 这些端口与 PID 就是它自己与它思考的载体，谨慎操作。
// （消息时间戳功能 2026-09-20 同日砍掉：模型会模仿注入格式抄进自己的
// 输出并落盘，累积成噪声；只保留生命感知。）

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SelfInfo 自我生命感知块的静态输入（server 启动时 / POST /config 热更新时设置）
type SelfInfo struct {
	Port       int    // 本 OkHuman 服务端口
	DataDir    string // 本 OkHuman 数据目录
	LLMBaseURL string // LLM base_url（解析出 host:port）
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
	b.WriteString("本 OkHuman 实例（承载这个会话的进程，端口/PID/源码位置实时计算）：\n")
	if si.Port > 0 {
		fmt.Fprintf(&b, "- 服务端口：%d\n", si.Port)
	}
	fmt.Fprintf(&b, "- 进程 PID：%d\n", os.Getpid())
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(strings.TrimSuffix(exe, " (deleted)")); dir != "" {
			fmt.Fprintf(&b, "- 源码位置：%s（运行树，二进制所在目录）\n", dir)
		}
	}
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
