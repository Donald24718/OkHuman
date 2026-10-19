package agent

// 工具内部 panic 必须被兜住：工具跑在独立 goroutine，drain 层的 panic 隔离抓不到
// 别的 goroutine，未兜住 = 整个进程死（不是本轮失败）。
import (
	"strings"
	"testing"

	"okhuman/internal/tools"
)

func TestGuardToolPanic(t *testing.T) {
	got := guardToolPanic("bash", func() string {
		var m map[string]string
		m["boom"] = "x" // nil map 写入 → panic
		return "不该走到这"
	})
	if !strings.HasPrefix(got, tools.ToolFailPrefix) {
		t.Fatalf("panic 应转成工具失败字符串，实际：%q", got)
	}
	if !strings.Contains(got, "工具内部错误") {
		t.Errorf("失败字符串应说明是内部错误，实际：%q", got)
	}
	if ok := guardToolPanic("bash", func() string { return "正常结果" }); ok != "正常结果" {
		t.Errorf("正常路径不该被改写：%q", ok)
	}
}

func TestExecToolSafeUnknownTool(t *testing.T) {
	got := execToolSafe("不存在的工具", map[string]interface{}{})
	if !strings.HasPrefix(got, tools.ToolFailPrefix) {
		t.Errorf("未知工具应转成失败字符串，实际：%q", got)
	}
}
