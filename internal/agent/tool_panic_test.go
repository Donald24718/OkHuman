package agent

// 工具内部 panic 必须被兜住：工具跑在独立 goroutine，drain 层的 panic 隔离抓不到
// 别的 goroutine，未兜住 = 整个进程死（不是本轮失败）。
import (
	"errors"
	"strings"
	"testing"

	"okhuman/internal/tools"
)

func TestGuardToolPanic(t *testing.T) {
	got := guardToolPanic("bash", func() (string, error) {
		var m map[string]string
		m["boom"] = "x" // nil map 写入 → panic
		return "不该走到这", nil
	})
	if !strings.HasPrefix(got.Text, tools.ToolFailPrefix) {
		t.Fatalf("panic 应转成工具失败字符串，实际：%q", got.Text)
	}
	if !strings.Contains(got.Text, "工具内部错误") {
		t.Errorf("失败字符串应说明是内部错误，实际：%q", got.Text)
	}
	if got.OK {
		t.Errorf("panic 必须 OK=false（后台判定靠这个字段，不靠文本）")
	}
	ok := guardToolPanic("bash", func() (string, error) { return "正常结果", nil })
	if ok.Text != "正常结果" {
		t.Errorf("正常路径不该被改写：%q", ok.Text)
	}
	if !ok.OK {
		t.Errorf("正常路径必须 OK=true")
	}
}

// TestGuardToolPanicErrorNotInTextOnly：error 路径 OK=false 且前缀仍在——
// 两个断言合起来才是这次改动的契约：**文本不变（给模型看），判定走字段**。
func TestGuardToolPanicErrorNotInTextOnly(t *testing.T) {
	got := guardToolPanic("bash", func() (string, error) { return "", errors.New("参数 command 缺失") })
	if got.OK {
		t.Errorf("error 必须 OK=false")
	}
	if !strings.HasPrefix(got.Text, tools.ToolFailPrefix) {
		t.Errorf("回填给模型的文本必须仍带前缀（模型侧契约不变），实际：%q", got.Text)
	}
	if !strings.Contains(got.Text, "参数 command 缺失") {
		t.Errorf("错误详情不能丢：%q", got.Text)
	}
}

// TestToolOutcomeOKTrueForFailedCommandExitCode：语义边界钉死——bash 命令
// 退出码非 0 **不是**工具层失败（OK=true）。这次改动不偷偷改变这条语义。
func TestToolOutcomeOKTrueForFailedCommandExitCode(t *testing.T) {
	got := execToolSafe("bash", map[string]interface{}{"command": "false"})
	if !got.OK {
		t.Errorf("命令退出码非 0 应仍 OK=true（工具层没炸），实际 OK=false：%q", got.Text)
	}
	if !strings.Contains(got.Text, "退出码") {
		t.Errorf("退出码信息应在正文里：%q", got.Text)
	}
}

func TestExecToolSafeUnknownTool(t *testing.T) {
	got := execToolSafe("不存在的工具", map[string]interface{}{})
	if !strings.HasPrefix(got.Text, tools.ToolFailPrefix) {
		t.Errorf("未知工具应转成失败字符串，实际：%q", got.Text)
	}
	if got.OK {
		t.Errorf("未知工具必须 OK=false")
	}
}
