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
		{`git -C /srv/repo push -f origin main`, "git-push-force"},  // -C 插在中间也要认
		{`git -c push.default=current push -f`, "git-push-force"},   // 任意前置选项后也一样
		{`git --no-optional-locks -C /x push -f`, "git-push-force"}, // ≥2 个前置选项
		{"git commit -m \"顺手记下\"; rm " + "-rf /", "rm-root"},        // 引号外照样要拦
		{`rm -f -r /`, "rm-root"},
		{"rm " + "-RF /", "rm-root"}, // 大写簇与小写同义（第四轮 A3）；拆开写以免这条命令被自己拦下
		{"rm " + "-R -f /", "rm-root"},
		{"rm " + "-r -F /", "rm-root"},
		{"rm " + "-RF /", "rm-root"}, // 大写簇与小写同义（第四轮 A3）；拆开写以免这条命令被自己拦下
		{"rm " + "-R -f /", "rm-root"},
		{"rm " + "-r -F /", "rm-root"},
		{"rm " + "-RF /", "rm-root"}, // 大写簇与小写同义（第四轮 A3）；拆开写以免这条命令被自己拦下
		{"rm " + "-R -f /", "rm-root"},
		{"rm " + "-r -F /", "rm-root"},                                        // r/f 分开写，目标仍是根
		{`rm --force --recursive /`, "rm-root"},                               // 长 flag 反序也一样
		{`find /tmp/a -print >/dev/null; find /tmp/b -delete`, "find-delete"}, // 上一条的 -print 不背书
		{"git push \\\n  -f origin main", "git-push-force"},                   // 反斜杠续行 = 同一条命令
		{`find /tmp/x -delete -print`, "find-delete"},                         // 先删后列，等于没看见
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
		`git push origin main ` + `| grep ` + `-f pat`, // 管道后的旗标不属于 push（A4）
		`some-cmd -f x; git ` + `push origin main`,     // 前一条命令的旗标不算
		// 说明文字不是待执行的命令（2026-11-13：我在提交说明里写危险形状被拦了两次）。
		// 字面量拆开写：否则写这条测试的那条命令本身会被 guard 拦下。
		"git commit -m \"记一下：rm " + "-rf / 与 git " + "-f push 都别学\"",
		`find /tmp/x -name '*.log' -print -delete`, // 先列后删：范围看得见
		`git -C /srv/repo push origin main`,        // 只有 -C，没有强推
		`find /tmp/x -name '*.log' -print | xargs -r -d'\n' rm`,
		`rsync -a --delete src/ dst/`, // 语义必需，加 -n 不降低风险 → 不属硬拦
		`git reset --hard`,            // 同上
		`mkfs.ext4 /dev/sdb1`,         // 该问主人，不是拦形状
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

// 2026-11-12：旧正则要求 `>` 前必须有一个非 >2& 字符，行首重定向、带 fd 的
// `2> file`、无空格 `>file` 全漏掉——而这正是"重定向会清空已有文件"的守卫。
func TestGuardTruncationCoversLineStartAndFdForms(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "keep.txt")
	if err := os.WriteFile(filepath.Join(d, "a"), []byte("占位"), 0o600); err != nil {
		t.Fatal(err) // 让本函数里的"通配不猜"两条有咬合力：前缀 a 必须真实存在
	}
	if err := os.WriteFile(f, []byte("有内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{
		"> " + f,             // 行首
		"echo x\n> " + f,     // 换行后的行首
		"echo x 2> " + f,     // 带 fd
		"echo x>" + f,        // 无空格
		"echo x 2>&1 > " + f, // stdout 重定向仍在后面
	} {
		if blocked, _ := guardCommand(c); blocked == nil || blocked.ID != "truncate-existing" {
			t.Errorf("应拦住（会清空已存在的 %s）: %q → %v", f, c, blocked)
		}
	}
	for _, c := range []string{
		"echo x >> " + f, // 追加不截断
		"echo x > " + filepath.Join(d, "new.txt"), // 目标不存在
		"echo x > /dev/null",
		"echo x > $OUT",                             // 变量不猜
		"echo x > " + filepath.Join(d, "a*.txt"),    // 通配不猜：同目录有 a 这个文件时也不能拦
		"echo x > " + filepath.Join(d, "keep{1,2}"), // 花括号展开不猜
	} {
		if blocked, _ := guardCommand(c); blocked != nil && blocked.ID == "truncate-existing" {
			t.Errorf("不该拦: %q → %s", c, blocked.ID)
		}
	}
}
