package ipython

// Python 解释器探测。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// detectTimeout 单次探测命令的兜底超时。
//
// 为什么必须有：Windows 上 PATH 里常见 Microsoft Store 的 `python.exe` 存根
// （App Execution Alias），它不是真解释器——运行它会弹商店且不退出，
// 没有超时就会把启动流程整个挂住。
const detectTimeout = 15 * time.Second

// DetectPython 按候选顺序找出第一个装了 IPython 的解释器。
//
// 只关心「能 import IPython」：本工具的全部能力由 IPython 提供，
// 没有它时哪怕 python 可用也应该不启用（否则模型调用必然失败）。
func DetectPython() (string, error) {
	var tried []string
	for _, cand := range []string{"python3", "python"} {
		p, err := exec.LookPath(cand)
		if err != nil {
			continue
		}
		if isStoreStub(p) {
			tried = append(tried, cand+"=WindowsApps 存根（跳过）")
			continue
		}
		ver, err := VerifyPython(p)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s=%v", cand, err))
			continue
		}
		_ = ver
		return p, nil
	}
	return "", fmt.Errorf("未找到装有 IPython 的 Python 解释器（已试 %s）；请 pip install ipython，或在配置里显式指定 tools.ipython_python",
		strings.Join(tried, "; "))
}

// VerifyPython 校验一个解释器可用且带 IPython，返回 IPython 版本。
func VerifyPython(pythonPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, pythonPath, "-c",
		"import sys;import IPython;sys.stdout.write(IPython.__version__)")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("探测超时（%v），疑似不可用的 python 存根", detectTimeout)
	}
	if err != nil {
		msg := summaryLine(strings.TrimSpace(stderr.String()))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	ver := strings.TrimSpace(string(out))
	if ver == "" {
		return "", errors.New("IPython 未报告版本")
	}
	return ver, nil
}

// summaryLine 把多行输出压成一行摘要（取最后一个非空行）。
//
// 为什么需要：探测失败时 Python 会把**完整 traceback** 打到 stderr
// （`File "<string>", line 1, in <module>` + `import IPython` 等若干行）。
// 这段文本会原样经 POST /config 的 applied 显示在 WebUI 配置页上——
// 又长又抓不住重点。真正有诊断价值的只有最后一行
// （`ModuleNotFoundError: No module named 'IPython'`）。
func summaryLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return truncateErr(t, 200)
		}
	}
	return ""
}

// truncateErr 给过长的异常消息兜底（防止单行异常把配置页撑爆）。
func truncateErr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// isStoreStub 判断路径是否落在 WindowsApps 目录（Microsoft Store 别名）。
func isStoreStub(p string) bool {
	// filepath.ToSlash 是必要的：Windows 路径分隔符是 '\'，直接 Contains 会漏判。
	return strings.Contains(strings.ToLower(filepath.ToSlash(p)), "microsoft/windowsapps/")
}
