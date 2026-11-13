// 超时标记的判定（2026-11-13 四方审计 R6）：只看计时器会把"恰好在上限那一刻正常完成"
// 的命令标成被强杀。必须两条同时成立：计时器开过火 + 进程被动信号结束。
package tools

import (
	"errors"
	"os/exec"
	"testing"
)

func TestKilledByLimitNeedsBothSignals(t *testing.T) {
	fired := make(chan struct{})
	closed := make(chan struct{})
	close(closed)

	cmd := exec.Command("sh", "-c", "kill -9 $$")
	sigErr := cmd.Run()
	ee, ok := sigErr.(*exec.ExitError)
	if !ok || ee.ExitCode() >= 0 {
		t.Fatalf("夹具没造出信号退出：%v", sigErr)
	}
	if !killedByLimit(closed, sigErr) {
		t.Error("计时器开火 + 信号退出 → 应判为被上限杀掉")
	}
	if killedByLimit(fired, sigErr) {
		t.Error("计时器没开火 → 不算（哪怕进程确实信号退出了）")
	}
	if killedByLimit(closed, nil) {
		t.Error("正常退出 → 不算")
	}
	if killedByLimit(closed, errors.New("启动失败")) {
		t.Error("非 ExitError → 不算")
	}
}
