package tools

// guard —— bash 元工具的硬拦：只拦「存在几乎同成本、且更安全写法」的灾难形状。
//
// 无逃生门（2026-11-08 用户定）：不给 agent 留 --force 出口；要放开只能改这里的代码。
// 准入条件（2026-11-08 议定）：只有能低成本替换成更安全写法的形状才进硬拦。
// 语义上必须如此、改写不降低风险的（rsync --delete / git reset --hard / mkfs / dd /
// docker down -v / systemctl restart）不在此拦——它们走参数校验、副作用预览，或该问用户。
// 只拦形状不拦意图：同一危险换成 xargs rm / python shutil.rmtree 就看不见，别把它当保证。
// 0 依赖：只用标准库 + 系统自带的 bash / systemctl。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 本进程启动时刻：只有此后被改过的 .sh 才算「我这个会话写的」，才扫。
var procStart = time.Now()

type guardFinding struct {
	ID   string
	Why  string
	Fix  string
	Span string
}

func (g guardFinding) String() string {
	return fmt.Sprintf("未执行 [guard/%s] %s\n命中的写法：%s\n更安全的写法：%s\n（没有逃生门：这条不可绕过；要放开只能改 internal/tools/guard.go）",
		g.ID, g.Why, g.Span, g.Fix)
}

var (
	reComment = regexp.MustCompile(`^\s*#`)
	// rm 的递归+强制 flag（分开写或合并写都算）
	reRM     = regexp.MustCompile(`(?:^|[;&|(` + "\t" + `\s])rm\s`)
	reRMrf   = regexp.MustCompile(`\s-(?:[a-zA-Z]*r[a-zA-Z]*f[a-zA-Z]*|[a-zA-Z]*f[a-zA-Z]*r[a-zA-Z]*)\b|\s--recursive\b[\s\S]*\s--force\b`)
	reRmRoot = regexp.MustCompile(`(?:^|\s)(?:/|/\*)(?:\s|$)|--no-preserve-root`)
	// 变量紧跟斜杠 = 空值塌缩成 / 的指纹（$d/、${d}/、"$d/"、"$d/sub"）。
	// 不带斜杠的 rm -rf $v 不拦：空值时 rm 自己报「缺少操作数」，不会删根。
	reRmVarSlash = regexp.MustCompile(`"?\$\{?[A-Za-z_][A-Za-z0-9_]*\}?/`)
	reFindDel    = regexp.MustCompile(`(?:^|[;&|\s])find\b[^\n]*\s-delete\b`)
	reChmodRoot  = regexp.MustCompile(`(?:^|[;&|\s])(?:chmod|chown)\b[^\n]*\s-[a-zA-Z]*R[a-zA-Z]*\b`)
	reSysRoot    = regexp.MustCompile(`(?:^|\s)(?:/|/bin|/boot|/dev|/etc|/home|/lib64|/lib|/media|/mnt|/opt|/proc|/run|/srv|/sys|/usr|/var)(?:\s|$)`)
	reKillGroup  = regexp.MustCompile(`(?:^|[;&|\s])kill(?:all)?\b[^\n]*(?:\s-?\w+\s)*(?:\s(?:0|-1|--\s-1))(?:\s|$)`)
	reForcePush  = regexp.MustCompile(`(?:^|[;&|\s])git\s+push\b[^\n]*(?:\s-f\b|--force\b)|\s-f\s.*git\s+push`)
	// 重定向截断：只认字面路径（含变量/通配的一律放过，避免猜错）。
	reTrunc = regexp.MustCompile(`[^>2&]\s>\s(/[^\s"'$<>*;&|)]+)`)
	// 我在执行一个脚本：bash x.sh / sh x.sh / ./x.sh
	reRunScript = regexp.MustCompile(`(?:^|[;&|\s])(?:bash|sh|zsh|dash)\s+([^\s"';]+\.sh)|\./([^\s"';]+\.sh)`)
)

// guardCommand：扫这段命令（含它显式执行、且本会话写过的 .sh）。返回阻断理由与附带提醒。
func guardCommand(command string) (blocked *guardFinding, notes []string) {
	seen := map[string]bool{}
	var scan func(text string)
	scan = func(text string) {
		for _, line := range strings.Split(text, "\n") {
			if reComment.MatchString(line) || strings.TrimSpace(line) == "" {
				continue
			}
			if f := matchLine(line); f != nil && blocked == nil {
				if !seen[f.ID] {
					seen[f.ID] = true
					blocked = f
				}
			}
		}
	}
	scan(command)
	if blocked == nil { // 我写的脚本也要过同一套（只扫本会话改过的，别人的旧脚本不产生噪音）
		for _, m := range reRunScript.FindAllStringSubmatch(command, -1) {
			p := m[1]
			if p == "" {
				p = "./" + m[2]
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				continue
			}
			st, err := os.Stat(abs)
			if err != nil || st.IsDir() || st.ModTime().Before(procStart) {
				continue
			}
			if b, err := os.ReadFile(abs); err == nil {
				scan(string(b))
			}
		}
	}
	if blocked == nil {
		notes = sideEffects(command)
	}
	return blocked, notes
}

func matchLine(line string) *guardFinding {
	t := strings.TrimSpace(line)
	switch {
	case reRM.MatchString(t) && reRMrf.MatchString(t):
		if m := reRmRoot.FindString(t); m != "" {
			return &guardFinding{ID: "rm-root", Why: "`rm -rf` 的目标是根目录或用了 --no-preserve-root，一次就没了",
				Span: clip(t), Fix: `限定到具体目录；或先 mv 到回收目录（可撤销）`}
		}
		if m := reRmVarSlash.FindString(t); m != "" {
			return &guardFinding{ID: "rm-empty-var", Why: "变量紧跟 `/`：变量为空时删的就是 `/`（少个符号删全盘）",
				Span: clip(t), Fix: `rm -rf "${d:?}/"（空值立刻报错退出，等价于抛异常）`}
		}
	case reFindDel.MatchString(t) && !strings.Contains(t, "-print"):
		return &guardFinding{ID: "find-delete", Why: "`find … -delete` 没有先列出，范围只在脑子里",
			Span: clip(t), Fix: `find … -print | xargs -r -d'\n' rm —— 先看见再删`}
	case reChmodRoot.MatchString(t) && reSysRoot.MatchString(t):
		return &guardFinding{ID: "chmod-recursive-system-root", Why: "`-R` 打到系统根，影响整台机器",
			Span: clip(t), Fix: `限定到具体子目录；需要更大范围请让用户确认`}
	case reKillGroup.MatchString(t):
		return &guardFinding{ID: "kill-whole-group", Why: "`kill 0` / `kill -1` 把信号打给整个进程组，常把自己也带走",
			Span: clip(t), Fix: `先 pgrep -af 拿具体 PID，再 kill <pid>，然后复核为 0`}
	case reForcePush.MatchString(t) && !strings.Contains(t, "--force-with-lease"):
		return &guardFinding{ID: "git-push-force", Why: "`git push -f` 会覆盖别人已推的提交，远端不可回退",
			Span: clip(t), Fix: `git push --force-with-lease（远端变了就拒绝，不覆盖）`}
	}
	// 重定向截断已存在的文件：> 一执行就先清空，失败时内容已经没了
	for _, m := range reTrunc.FindAllStringSubmatch(t, -1) {
		p := m[1]
		if strings.HasPrefix(p, "/dev/") {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			return &guardFinding{ID: "truncate-existing", Why: fmt.Sprintf("`> %s` 会先把一个已存在的非空文件清空", p),
				Span: clip(t), Fix: fmt.Sprintf("先 cp %s %s.bak-<今天>，或改用 >>", p, p)}
		}
	}
	return nil
}

// guardNotes：只算副作用预览（调用方已确认未被拦）。
func guardNotes(command string) []string { return sideEffects(command) }

// sideEffects 不拦，只把看不见的连带后果说出来（一次输出，不是多一轮）。
// 本机真实需要过的一条：重启 user 单元会连带收走同 cgroup 里 agent 起的进程。
func sideEffects(command string) []string {
	var notes []string
	for _, m := range regexp.MustCompile(`systemctl\s+(--user\s+)?(?:restart|stop)\s+([A-Za-z0-9_.@-]+\.service)`).FindAllStringSubmatch(command, -1) {
		userFlag := strings.TrimSpace(m[1]) != ""
		unit := m[2]
		args := []string{"show", "-p", "ControlGroup", "--value", unit}
		if userFlag {
			args = append([]string{"--user"}, args...)
		}
		out, err := exec.Command("systemctl", args...).Output()
		if err != nil {
			continue
		}
		cg := strings.TrimSpace(string(out))
		if cg == "" {
			continue
		}
		procs := cgroupProcs(cg)
		if len(procs) > 1 {
			notes = append(notes, fmt.Sprintf("[guard/side-effect] %s 会连带收走同 cgroup 里的进程（unit 名下不止它自己）：%s",
				unit, strings.Join(procs, ", ")))
		}
	}
	return notes
}

// cgroupProcs：读 cgroup 里的 pid 与命令名（只读 /proc 与 /sys，不额外起进程）。
func cgroupProcs(cg string) []string {
	data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cg, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range strings.Fields(string(data)) {
		cmd, err := os.ReadFile(filepath.Join("/proc", s, "comm"))
		if err != nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s(%s)", strings.TrimSpace(string(cmd)), s))
	}
	return out
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

var _ = strconv.Itoa // 保留：测试与将来参数校验共用
