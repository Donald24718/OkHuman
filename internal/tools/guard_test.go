package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 硬拦 fixture：每条都必须带「更安全的写法」——不能低成本替换的形状不允许进这张表。
func TestGuardBlocksByShape(t *testing.T) {
	cases := []struct{ cmd, id string }{
		{`rm -rf $d/`, "rm-empty-var"},
		{`rm -rf "$d/"`, "rm-empty-var"},
		{`rm -rf ${BUILD}/sub`, "rm-empty-var"},
		{`rm -rf /`, "rm-root"},
		{`rm -fr /*`, "rm-root"},
		{`rm -rf --no-preserve-root /`, "rm-root"},
		{`find /tmp/x -name '*.log' -delete`, "find-delete"},
		{`chmod -R 777 /media`, "chmod-recursive-system-root"},
		{`kill 0`, "kill-whole-group"},
		{`kill -TERM 0`, "kill-whole-group"},
		{`git push -f origin main`, "git-push-force"},
		{`git push --force`, "git-push-force"},
	}
	for _, c := range cases {
		blocked, _ := guardCommand(c.cmd)
		if blocked == nil {
			t.Errorf("该拦没拦: %q", c.cmd)
			continue
		}
		if blocked.ID != c.id {
			t.Errorf("%q 命中 %s，期望 %s", c.cmd, blocked.ID, c.id)
		}
		if blocked.Fix == "" || blocked.Why == "" {
			t.Errorf("%s 没给原因或改法", blocked.ID)
		}
	}
}

// 放过的一批：既有「不危险」的，也有「设计上明确不拦（改写不降低风险）」的。
func TestGuardAllows(t *testing.T) {
	ok := []string{
		`rm -rf "$d"`, // 无斜杠：空值时 rm 自己报缺少操作数，不会删根
		`rm -f /tmp/a.log`,
		`git push --force-with-lease`,
		`find /tmp/x -name '*.log' -print | xargs -r -d'\n' rm`,
		`rsync -a --delete src/ dst/`, // 语义必需，加 -n 不降低风险 → 不属硬拦
		`git reset --hard`,            // 同上
		`mkfs.ext4 /dev/sdb1`,         // 该问用户，不是拦形状
		`systemctl --user restart strata-8080`,
		`# rm -rf / 说明文字`,
		`echo hi > /definitely/not/exists/xyz`,
	}
	for _, c := range ok {
		if blocked, _ := guardCommand(c); blocked != nil {
			t.Errorf("不该拦: %q → %s", c, blocked.ID)
		}
	}
}

func TestGuardTruncatesExistingFile(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "keep.txt")
	if err := os.WriteFile(f, []byte("有内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := guardCommand("echo new > " + f); blocked == nil || blocked.ID != "truncate-existing" {
		t.Fatalf("截断已存在非空文件应被拦，得 %v", blocked)
	}
	if blocked, _ := guardCommand("echo new >> " + f); blocked != nil {
		t.Errorf("追加不该拦: %s", blocked.ID)
	}
	if blocked, _ := guardCommand("echo x > /dev/null"); blocked != nil {
		t.Errorf("/dev/null 不该拦: %s", blocked.ID)
	}
}

// 执行「本会话写的脚本」时同样过一遍；别人的旧脚本不产生噪音。
func TestGuardScansScriptWrittenThisSession(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "run.sh")
	if err := os.WriteFile(f, []byte("#!/bin/bash\nrm -rf $TARGET/\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := guardCommand("bash " + f); blocked == nil || blocked.ID != "rm-empty-var" {
		t.Fatalf("本会话写的脚本应被扫，得 %v", blocked)
	}
	old := filepath.Join(d, "old.sh")
	if err := os.WriteFile(old, []byte("rm -rf $TARGET/\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(old, procStart.Add(-time2h), procStart.Add(-time2h))
	if blocked, _ := guardCommand("bash " + old); blocked != nil {
		t.Errorf("会话前就存在的脚本不该扫: %s", blocked.ID)
	}
}

func TestNoEscapeHatch(t *testing.T) {
	// 无逃生门：--force/--guard=off 都不该让被拦的命令通过
	for _, suffix := range []string{" --force", " --guard=off", "; true # --force"} {
		if blocked, _ := guardCommand(`rm -rf $d/` + suffix); blocked == nil {
			t.Errorf("带 %q 就想绕过，说明漏了逃生门之外的口子", suffix)
		}
	}
	src, err := os.ReadFile("guard.go")
	if err == nil && strings.Contains(string(src), "flag.Bool") {
		t.Error("guard.go 里不该出现开关")
	}
}

// 关键安全性质：guard 只做静态检查，绝不执行被检命令——所以跑测试不会动到机器。
// 副作用预览也只读 systemctl show / cgroup 文件，不会真去 restart 任何东西。
func TestGuardNeverExecutes(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "side-effect-probe")
	for _, cmd := range []string{
		"touch " + probe,
		"mkfs.ext4 " + probe,
		"dd if=/dev/zero of=" + probe + " bs=1M count=100",
		"rm -rf " + probe,
		"systemctl --user restart this-unit-does-not-exist.service",
		`chmod -R 777 /media`,
		`git push -f`,
	} {
		if _, _ = guardCommand(cmd); fileExists(probe) {
			t.Fatalf("guard 竟然执行了命令: %q", cmd)
		}
	}
	// 副作用预览也不该把不存在的 unit 起来
	out, err := exec.Command("systemctl", "--user", "is-active", "this-unit-does-not-exist.service").Output()
	if err == nil && strings.TrimSpace(string(out)) == "active" {
		t.Fatal("副作用预览把 unit 起来了")
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

var time2h = 2 * time.Hour // 造一个「会话开始前」的文件用

// 钉住被拦时我给到的文本：格式变了这里会红，不会悄悄改掉。
func TestGuardBlockedTextIsStable(t *testing.T) {
	b, _ := guardCommand(`rm -rf $BUILD/`)
	want := "未执行 [guard/rm-empty-var] 变量紧跟 `/`：变量为空时删的就是 `/`（少个符号删全盘）\n" +
		"命中的写法：rm -rf $BUILD/\n" +
		"更安全的写法：rm -rf \"${d:?}/\"（空值立刻报错退出，等价于抛异常）\n" +
		"（没有逃生门：这条不可绕过；要放开只能改 internal/tools/guard.go）"
	if b.String() != want {
		t.Errorf("被拦文本变了。得到:\n%s\n期望:\n%s", b.String(), want)
	}
	if !strings.Contains(b.String(), "没有逃生门") {
		t.Error("被拦文本必须说明没有逃生门")
	}
}
