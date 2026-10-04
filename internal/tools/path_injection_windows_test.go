//go:build windows

package tools

// 复现测试：Agent 通道下 coreutils 必须可达（回归防护）。
//
// 背景（2026-10-04 实证，用户提供的 Agent 侧自检日志）：
//   Agent 进程从 Windows 原生血统启动（cmd/PowerShell），其 PATH 里**没有**
//   Git 的 usr\bin。而 bash 元工具用的是非 login shell（`bash <script>`，
//   无 -l），不读 /etc/profile → 不补 PATH。
//   结果：cat/grep/head/sed/awk/ls/tr/wc/date 等**全部 command not found**，
//   而 find/sort 落到 C:\WINDOWS\system32\ 下的 **Windows 版同名程序**（语义完全不同）。
//
// 本测试用"剥掉 PATH 中所有 Git/usr/bin 段"来模拟该环境（实测可稳定复现），
// 断言修复后 coreutils 仍必须可达。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stripGitFromPath 去掉 PATH 中所有指向 Git / PortableGit / usr\bin 的段，
// 模拟"Agent 从 Windows 原生血统启动"的环境。
func stripGitFromPath(env []string) []string {
	var out []string
	for _, e := range env {
		if !strings.HasPrefix(strings.ToUpper(e), "PATH=") {
			out = append(out, e)
			continue
		}
		val := e[len("PATH="):]
		var keep []string
		for _, seg := range strings.Split(val, ";") {
			low := strings.ToLower(seg)
			if strings.Contains(low, "portablegit") ||
				strings.Contains(low, "\\git\\") ||
				strings.HasSuffix(low, "\\usr\\bin") ||
				strings.HasSuffix(low, "/usr/bin") {
				continue
			}
			keep = append(keep, seg)
		}
		out = append(out, "PATH="+strings.Join(keep, ";"))
	}
	return out
}

// runRealWithEnv 同 runReal，但允许指定环境变量。
func runRealWithEnv(t *testing.T, script string, timeoutSec int, env []string) (ExecInfo, string, string) {
	t.Helper()
	dir := t.TempDir()
	sp := filepath.Join(dir, "s.sh")
	if err := os.WriteFile(sp, []byte(script), 0o644); err != nil {
		t.Fatalf("写脚本失败: %v", err)
	}
	var out, errb strings.Builder
	// 需要绕过 Run 的固定 env，故此用例直接验证 childEnv() 的产物。
	_ = env
	info, err := (defaultExecutor{}).Run(sp, timeoutSec, &out, &errb)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	return info, out.String(), errb.String()
}

// TestChildEnvInjectsGitUsrBin 直接断言 childEnv() 注入的 PATH 含 Git usr\bin，
// 且该段位于 System32 **之前**（保证 find/sort 拿到 Unix 版而非 Windows 版）。
func TestChildEnvInjectsGitUsrBin(t *testing.T) {
	bp, err := findBash()
	if err != nil {
		t.Fatalf("findBash: %v", err)
	}
	usrBin := filepath.Dir(bp) // .../usr/bin
	t.Logf("bash=%s → 期望注入的 usr/bin=%s", bp, usrBin)

	env := childEnv()
	var pathVal string
	for _, e := range env {
		if strings.HasPrefix(strings.ToUpper(e), "PATH=") {
			pathVal = e[len("PATH="):]
			break
		}
	}
	if pathVal == "" {
		t.Fatal("childEnv() 未返回 PATH 项")
	}

	// 归一化为 Windows 原生形式比较（子进程收到的是 Windows 风格 PATH）。
	segs := strings.Split(pathVal, ";")
	foundUsrBin := -1
	foundSystem32 := -1
	for i, s := range segs {
		low := strings.ToLower(filepath.ToSlash(strings.TrimSpace(s)))
		if strings.Contains(low, "portablegit") && strings.Contains(low, "usr/bin") {
			if foundUsrBin < 0 {
				foundUsrBin = i
			}
		}
		if strings.Contains(low, "/git/") && strings.HasSuffix(low, "usr/bin") {
			if foundUsrBin < 0 {
				foundUsrBin = i
			}
		}
		if foundSystem32 < 0 && strings.Contains(low, "windows/system32") {
			foundSystem32 = i
		}
	}

	if foundUsrBin < 0 {
		t.Errorf("childEnv() 的 PATH 未包含 Git usr/bin（Agent 通道会 coreutils 全灭）")
	}
	if foundUsrBin >= 0 && foundSystem32 >= 0 && foundUsrBin > foundSystem32 {
		t.Errorf("Git usr/bin（idx=%d）排在 System32（idx=%d）之后，"+
			"find/sort 仍会命中 Windows 版", foundUsrBin, foundSystem32)
	}
}

// simulateAgentEnv 构造「Agent 从 Windows 原生血统启动」的子进程环境。
//
// **必须走 buildChildEnv（被测函数本身）**，不能在这里复制它的逻辑——
// 否则变异打在 buildChildEnv 上测试也看不见（本文件首版即犯此错，
// 变异测试抓出"假绿"后重写）。
func simulateAgentEnv(t *testing.T) []string {
	t.Helper()
	base := stripGitFromPath(os.Environ()) // 剥掉 Git 段 = 模拟无 Git 的父环境
	env := buildChildEnv(base)             // ← 被测对象

	if getEnvVar(env, "PATH") == getEnvVar(base, "PATH") {
		t.Fatalf("buildChildEnv 未改变 PATH（注入失效）：原=%q", getEnvVar(base, "PATH"))
	}
	return env
}

// TestAgentChannelCoreutilsAvailable 端到端：模拟"剥掉 Git 的 PATH"，
// 断言修复后 cat/grep/find/sort 等仍必须可达，且不得落到 System32。
func TestAgentChannelCoreutilsAvailable(t *testing.T) {
	if _, err := findBash(); err != nil {
		t.Skipf("无可用 bash: %v", err)
	}

	script := `#!/bin/bash
for c in cat grep head tail sed awk ls tr wc date cp mv sort uniq xargs find; do
  p="$(command -v "$c" 2>/dev/null)"
  if [ -z "$p" ]; then
    echo "$c MISSING"
  else
    echo "$c $p"
  fi
done
`
	dir := t.TempDir()
	sp := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(sp, []byte(script), 0o644); err != nil {
		t.Fatalf("写脚本失败: %v", err)
	}

	env := simulateAgentEnv(t)

	var out strings.Builder
	info, err := (defaultExecutor{}).RunWithEnv(sp, 10, &out, &strings.Builder{}, env)
	if err != nil {
		t.Fatalf("RunWithEnv 失败: %v", err)
	}
	if info.Code != 0 {
		t.Logf("exit code = %d", info.Code)
	}

	body := out.String()
	t.Logf("Agent 通道（PATH 无 Git）探测结果:\n%s", body)

	var missing []string
	var winSquat []string
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		name, path := f[0], f[1]
		if path == "MISSING" {
			missing = append(missing, name)
			continue
		}
		low := strings.ToLower(filepath.ToSlash(path))
		if strings.Contains(low, "/windows/system32/") {
			winSquat = append(winSquat, name+"→"+path)
		}
	}

	if len(missing) > 0 {
		t.Errorf("以下 coreutils 在 Agent 通道不可达: %v", missing)
	}
	if len(winSquat) > 0 {
		t.Errorf("以下命令命中了 Windows 同名程序（语义不同！）: %v", winSquat)
	}
}

// mergeEnv 把 extra 合并进 base（同名键以 extra 覆盖）。
func mergeEnv(base, extra []string) []string {
	idx := map[string]int{}
	var out []string
	for _, e := range base {
		k := e
		if i := strings.Index(e, "="); i >= 0 {
			k = strings.ToUpper(e[:i])
		}
		idx[k] = len(out)
		out = append(out, e)
	}
	for _, e := range extra {
		k := e
		if i := strings.Index(e, "="); i >= 0 {
			k = strings.ToUpper(e[:i])
		}
		if pos, ok := idx[k]; ok {
			out[pos] = e
			continue
		}
		idx[k] = len(out)
		out = append(out, e)
	}
	return out
}

// ---- 临时目录可写性（2026-10-05 实证，同日二次修正）------------------------
//
// **修正说明（重要，勿回退）**：本用例最初断言「Agent 通道下 /tmp 一定可写」，
// 且注释宣称该 warning 是「无害噪音」。**这两条都已被推翻**：
//
//	/tmp 是 MSYS 的 `usertemp` 挂载点（见 /etc/fstab），挂载目标**由 MSYS 运行时
//	自行推导**——实测**不受** TMP / TEMP / TMPDIR / MSYS_TMP 影响（四组控制变量，
//	目标纹丝不动）；且目标会落到宿主会话临时目录
//	（如 .../Temp/reasonix-session-tmp-<NNN>）**并可能被回收**。
//	目标一旦被回收 → 写 /tmp 必然 ENOENT → 该断言是**间歇性失败（flaky）**，已废弃。
//
// 详见 docs/bash-tmp告警排查-根因与修复-2026-10-05.md。
//
// **修正后的契约**：守护工具提示词真正依赖的那条路 —— `$TEMP` 必须可写。
// Windows 保证 %TEMP% 存在且稳定，Windows 侧的 tmpHint() 正是靠它给模型兜底的；
// 它若不可写，模型在 Windows 上将无处写临时文件。
//
// 本用例**必须走 defaultExecutor**（真实通道，含 childEnv 注入），不得手工调 bash。
func TestAgentChannelTempWritable(t *testing.T) {
	if _, err := findBash(); err != nil {
		t.Skipf("无可用 bash: %v", err)
	}

	script := `#!/bin/bash
echo "TEMP=[$TEMP]"
echo hi > "$TEMP/okhuman_temp_probe.txt" && echo TEMP_WRITE_OK || echo TEMP_WRITE_FAIL
rm -f "$TEMP/okhuman_temp_probe.txt"
`
	dir := t.TempDir()
	sp := filepath.Join(dir, "temp_probe.sh")
	if err := os.WriteFile(sp, []byte(script), 0o644); err != nil {
		t.Fatalf("写脚本失败: %v", err)
	}

	// 必须走真实执行通道（含 childEnv 注入），不得手工构造环境。
	var out strings.Builder
	info, err := (defaultExecutor{}).Run(sp, 10, &out, &strings.Builder{})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if info.Code != 0 {
		t.Logf("exit code = %d", info.Code)
	}

	got := out.String()
	t.Logf("输出:\n%s", got)
	if !strings.Contains(got, "TEMP_WRITE_OK") {
		t.Errorf("Agent 通道下 $TEMP 不可写（childEnv 注入被破坏？）:\n%s", got)
	}
}
