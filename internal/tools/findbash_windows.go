//go:build windows

package tools

// Windows 下 bash 可执行文件的探测（方案 §4.1）。
//
// 探测顺序（实证支持）：
//  1. 显式覆盖：环境变量 OKHUMAN_BASH_PATH（用户指哪用哪，最高优先）；
//  2. 已知安装路径（Git for Windows 标准位置）；
//  3. PATH 查找（LookPath），但**必须排除 WSL relay**（System32\bash.exe）。
//
// 两条实证结论驱动本设计（方案 §11.6 / §12.7）：
//   - **不能一次探测就永久缓存**：便携版 Git 路径含版本号（versions\1.2.0\），
//     升级后旧路径失效 → 必须"缓存 + 每次 os.Stat 校验 + 失效重探测"。
//   - **体积不能作为"真 bash"判据**：Git 的 bin\bash.exe 是 47KB wrapper 也是
//     合法 bash；System32\bash.exe 是 86KB WSL relay 必须排除 →
//     **改用路径特征排除**（落在 System32 下的同名文件即 relay）。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// bashPathCache 缓存已探测到的 bash 路径（避免每次调用都扫盘）。
// 缓存必须配 os.Stat 校验：路径失效（卸载/升级）时自动重探测。
var (
	bashPathMu    sync.Mutex
	bashPathCache string
)

// findBash 返回可用的 bash 可执行文件绝对路径（失败返回明确错误，不静默降级）。
func findBash() (string, error) {
	bashPathMu.Lock()
	defer bashPathMu.Unlock()

	// 缓存命中且文件仍在 → 直接用（每次 Stat 校验，防"缓存静默失效"）。
	if bashPathCache != "" {
		if _, err := os.Stat(bashPathCache); err == nil {
			return bashPathCache, nil
		}
		bashPathCache = "" // 失效 → 重探测
	}

	path, err := probeBash()
	if err != nil {
		return "", err
	}
	bashPathCache = path
	return path, nil
}

// probeBash 执行实际探测（不加锁，由 findBash 调用）。
func probeBash() (string, error) {
	// 1. 显式覆盖（用户最清楚自己的环境；探测再聪明也会猜错）。
	if p := strings.TrimSpace(os.Getenv("OKHUMAN_BASH_PATH")); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("OKHUMAN_BASH_PATH 指向的文件不可用: %w", err)
		}
		if isWSLRelay(p) {
			return "", fmt.Errorf("OKHUMAN_BASH_PATH 指向 WSL relay（%s），非 Git Bash；请指向 Git 的 bash.exe", p)
		}
		return p, nil
	}

	// 2. 已知安装路径（Git for Windows 标准位置）。
	//    usr\bin\bash.exe 是真实二进制（~2.5MB），bin\bash.exe 是 wrapper（~47KB），
	//    两者都可用；优先 usr\bin（更接近"真 bash"）。
	for _, cand := range []string{
		`C:\Program Files\Git\usr\bin\bash.exe`,
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\usr\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
	} {
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}

	// 3. PATH 查找（必须排除 WSL relay）。
	if p, err := exec.LookPath("bash.exe"); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			p = abs
		}
		if !isWSLRelay(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			p = abs
		}
		if !isWSLRelay(p) {
			return p, nil
		}
	}

	return "", fmt.Errorf("未找到可用的 Git Bash（已尝试：OKHUMAN_BASH_PATH、%s、"+
		"PATH 查找均未命中或命中 WSL relay）；请安装 Git for Windows 或用 OKHUMAN_BASH_PATH 指定",
		`C:\Program Files\Git`)
}

// isWSLRelay 判定路径是否为 Windows 自带的 WSL 桥接程序（System32\bash.exe）。
//
// 为什么必须排除：System32 在 PATH 中优先级高，其 bash.exe 会把命令转交 WSL，
// 而 WSL 里没有我们的脚本路径映射 → 静默按错误语义执行（方案 §11.6 实测）。
//
// 判据用**路径特征**而非体积：wrapper 仅 47KB 也是合法 Git Bash，
// 按体积判会误杀（方案 §12.7 实证）。
func isWSLRelay(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	// 归一化：System32 可能以 Sysnative/Windows 等多种形式出现
	for _, marker := range []string{
		"/windows/system32/bash.exe",
		"/windows/sysnative/bash.exe",
		"/windows/syswow64/bash.exe",
	} {
		if strings.HasSuffix(lower, marker) {
			return true
		}
	}
	return false
}
