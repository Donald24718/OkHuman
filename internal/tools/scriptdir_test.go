package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetScriptDir(t *testing.T) {
	t.Helper()
	prev := ""
	if v, ok := scriptDir.Load().(string); ok {
		prev = v
	}
	t.Cleanup(func() {
		if prev == "" {
			scriptDir.Store("")
		} else {
			scriptDir.Store(prev)
		}
	})
}

// TestScriptLandsInConfiguredDirAndCleanedUp 脚本落点 = 配置的目录（不是共享 /tmp），
// 目录权限 0700（命令全文不外泄），执行完 defer 删除不留残留。
func TestScriptLandsInConfiguredDirAndCleanedUp(t *testing.T) {
	resetScriptDir(t)
	dir := filepath.Join(t.TempDir(), "bash-jobs") // 新建路径：MkdirAll 应给 0700
	SetScriptDir(dir)
	if scriptDirNow() != dir {
		t.Fatalf("SetScriptDir 未生效：%s", scriptDirNow())
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("脚本目录应为 0700，实际 %v err=%v", st.Mode().Perm(), err)
	}
	// 预置一个权限松的目录：SetScriptDir 必须收紧（命令全文不外泄）
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(loose, 0o755)
	SetScriptDir(loose)
	if st, _ := os.Stat(loose); st.Mode().Perm() != 0o700 {
		t.Errorf("已存在目录应被收紧为 0700，实际 %s", st.Mode().Perm())
	}
	Configure(30000, 600000)
	out, err := ExecuteTool("bash", map[string]interface{}{"command": "echo ok-c5"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ok-c5") {
		t.Errorf("输出异常：%q", out)
	}
	ms, _ := filepath.Glob(filepath.Join(dir, "okhuman-bash-*"))
	if len(ms) != 0 {
		t.Errorf("执行后脚本未删除：%v", ms)
	}
}

// TestCleanupStaleScripts 上次被 SIGKILL 留下的残留（mtime 早于本次进程启动）在启动时
// 被清掉；本次刚创建的（在用）不动。
func TestCleanupStaleScripts(t *testing.T) {
	resetScriptDir(t)
	dir := t.TempDir()
	SetScriptDir(dir)
	stale := filepath.Join(dir, "okhuman-bash-1-aaaa.sh")
	fresh := filepath.Join(dir, "okhuman-bash-2-bbbb.sh")
	for _, f := range []string{stale, fresh} {
		if err := os.WriteFile(f, []byte("echo x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if n := CleanupStaleScripts(); n != 1 {
		t.Errorf("应清 1 个，实际 %d", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("遗留脚本未被清掉")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("本次在用的脚本被误删")
	}
}

// TestCleanupSkipsFallbackDir 未接线（回退共享 /tmp）时绝不清理共享目录：
// 那里可能有别的实例的脚本正在用。
func TestCleanupSkipsFallbackDir(t *testing.T) {
	resetScriptDir(t)
	scriptDir.Store("")
	before := countIn(os.TempDir())
	if n := CleanupStaleScripts(); n != 0 {
		t.Errorf("回退态不应清理任何文件，实际清了 %d", n)
	}
	if after := countIn(os.TempDir()); after != before {
		t.Errorf("回退态动了共享目录：%d → %d", before, after)
	}
	if scriptDirNow() != os.TempDir() {
		t.Errorf("回退态应落 os.TempDir()，实际 %s", scriptDirNow())
	}
}

func countIn(dir string) int {
	ms, _ := filepath.Glob(filepath.Join(dir, "okhuman-bash-*"))
	return len(ms)
}
