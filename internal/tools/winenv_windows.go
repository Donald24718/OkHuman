//go:build windows

package tools

// Windows 下启动 bash 子进程的**统一底层**（级 1/2/3 共用）。
//
// 设计决定（2026-10-04）：三条降级路径都自建 CreateProcess，而**不混用 os/exec**。
// 理由：混用会导致 (a) cmd.Wait 与 WaitForSingleObject 两套等待语义并存，
// (b) os.Process 资源与 os/exec 内部管道 goroutine 残留，(c) 句柄归属混乱。
// 统一自建后，只有"带不带 Job"一个差异点，句柄纪律一处可查。

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// extendedStartupInfoPresent 见 bash_windows.go（常量只在该文件定义一次）。

// childEnv 返回传给 bash 子进程的环境变量列表（基于当前进程环境）。
//
// 实现见 buildChildEnv —— 拆成纯函数是为了**可测试**：
// 「Agent 从 Windows 原生血统启动」这一故障场景要求"父环境里没有 Git 段"，
// 而真实父环境无法在测试中剥除。buildChildEnv 允许直接注入构造好的 base。
func childEnv() []string {
	return buildChildEnv(os.Environ())
}

// buildChildEnv 在给定 base 环境上施加元工具所需的环境变换（**纯函数，可测试**）。
//
// 三重变换：
//  1. **PATH 前置 Git usr\bin**（2026-10-04 实证，见下）；
//  2. MSYS_NO_PATHCONV=1 —— 防 MSYS 路径改写；
//  3. MSYS2_ARG_CONV_EXCL=* —— 让 `cmd //c` / `powershell -Command` 的参数不被重写。
//
// **为什么必须注入 PATH（真实故障，非理论）**：
// 元工具用的是**非 login shell**（`bash <script>`，无 -l），不读 /etc/profile，
// 因此 **PATH 完全继承自父进程**。Agent 进程若从 Windows 原生血统启动
// （cmd / 资源管理器双击 / 系统服务），其 PATH 里**没有** Git 的 usr\bin，导致：
//   - `cat` / `grep` / `head` / `sed` / `awk` / `ls` / `tr` / `wc` / `date` → 全部
//     `command not found`（coreutils 全灭）；
//   - 更危险：`find` / `sort` 会命中 C:\WINDOWS\system32 下的 **Windows 同名程序**
//     （语义完全不同，如 Windows `find.exe` 是字符串查找）→ **静默给出错误结果**。
//
// 修法：把 bash 自身所在目录（Git 的 usr\bin，内含全套 coreutils）**前置**到 PATH，
// 既补齐缺失命令，又保证优先于 System32 的同名程序。
func buildChildEnv(base []string) []string {
	env := make([]string, len(base))
	copy(env, base)

	// 1. PATH 前置 Git usr\bin。
	env = setEnvVar(env, "PATH", prependGitUsrBin(getEnvVar(env, "PATH")))

	// 2. MSYS 路径改写开关（必须是子进程 env，父 shell export 会被 MSYS 吃掉）。
	env = setEnvVar(env, "MSYS_NO_PATHCONV", "1")
	env = setEnvVar(env, "MSYS2_ARG_CONV_EXCL", "*")

	return env
}

// prependGitUsrBin 把 Git 的 usr\bin 前置到 PATH（找不到 bash 时原样返回）。
//
// 用 findBash 的探测结果反推目录，好处是"与真正要启动的 bash 严格同源"：
// 便携版 Git 的路径含版本号，写死会随升级失效。
func prependGitUsrBin(path string) string {
	bashPath, err := findBash()
	if err != nil {
		return path // 探测失败时不改 PATH（Run 稍后会因 findBash 失败而报错）
	}
	usrBin := filepath.Dir(bashPath)

	// 已存在则不重复前置（避免多次调用 env 膨胀）。
	for _, seg := range strings.Split(path, string(os.PathListSeparator)) {
		if strings.EqualFold(strings.TrimRight(seg, `\/`), strings.TrimRight(usrBin, `\/`)) {
			return path
		}
	}

	// Windows 原生形式（CreateProcess 的 lpEnvironment 期望 Windows 风格 PATH）。
	native := filepath.FromSlash(usrBin)
	if path == "" {
		return native
	}
	return native + string(os.PathListSeparator) + path
}

// getEnvVar 从 "K=V" 列表中取 K 的值（不区分大小写，Windows 语义）。
func getEnvVar(env []string, key string) string {
	prefix := strings.ToUpper(key) + "="
	for _, e := range env {
		if strings.ToUpper(e[:min(len(e), len(prefix))]) == prefix {
			return e[len(prefix):]
		}
	}
	return ""
}

// setEnvVar 在 "K=V" 列表中设置 K（存在则原地替换，否则追加）。
// 必须原地替换而非追加：CreateProcess 对重复变量只取其一，容易产生"看起来设了却没生效"。
func setEnvVar(env []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	for i, e := range env {
		if strings.ToUpper(e[:min(len(e), len(prefix))]) == prefix {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}

// buildWindowsEnvBlock 把 []string 编码成 CreateProcess 需要的 UTF-16 环境块。
//
// **两个关键坑（2026-10-04 实测）**：
//
//  1. 不能用 windows.UTF16PtrFromString——它对含 NUL 的字符串返回 "invalid argument"
//     （内部走 UTF16FromString 拒绝 "string with NUL"）。环境块必须手工编码：
//     每个 "K=V" 后跟一个 NUL，整个块以**双 NUL** 结尾。
//     此坑若不避开，表现为 **env 静默失效**（子进程读到的变量为空），极难排查。
//
//  2. **必须用 utf16.Encode 做代理对编码**，不能 `uint16(r)` 逐 rune 强转。
//     rune > 0xFFFF（emoji、CJK 扩展 B 区汉字等非 BMP 字符）在 UTF-16 里是
//     代理对（两个 uint16），`uint16(r)` 会**静默截断高位**：
//     `uint16(0x1F33F) = 0xF33F` → 子进程读到乱码；
//     更严重的是 `uint16(0x20000) = 0x0000` —— 直接注入一个 NUL，
//     使环境块在字符串中间被截断，CreateProcess 报
//     "The parameter is incorrect." 而**整个进程启动失败**（实测）。
//
//     注意：这段历史很隐蔽——NUL 坑（坑 1）被注释记录并避开了，但代理对坑
//     （坑 2）只在非 BMP 输入下触发，ASCII/BMP 测试全绿，故长期潜伏。
func buildWindowsEnvBlock(env []string) (*uint16, error) {
	var u16 []uint16
	for _, e := range env {
		u16 = append(u16, utf16.Encode([]rune(e))...)
		u16 = append(u16, 0)
	}
	u16 = append(u16, 0) // 双 NUL 结尾
	return &u16[0], nil
}

// mustBuildEnvBlock 同 buildWindowsEnvBlock，失败时返回 nil。
//
// 注意：当前实现**不会失败**（唯一错误源已被排除），故实际上恒返回非 nil。
// 保留 error 返回形态是为了将来加入校验（如拒绝含 NUL 的键值）时不必改调用方；
// 调用方仍需按"可能为 nil"处理（nil = 让 CreateProcess 用父进程环境）。
func mustBuildEnvBlock(env []string) *uint16 {
	p, _ := buildWindowsEnvBlock(env)
	return p
}
