package tools

// bash 元工具实现。第二个元工具（如 ipython）应新建同目录 ipython.go，
// 不要塞进本文件；本文件只放 bash 特有逻辑，公共设施见 output.go / args.go。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

// toolBash bash 元工具：detached 独立进程组 + 超时杀整组。
// 串行链是死锁放大器——一条命令挂住，后面全部堵死（真实事故：agent 用
// nohup ... & 拉起服务，后台子壳握着输出管道写端不退出，等 EOF 永挂）。
// 配套防死锁：独立进程组 + 超时杀整组（-pid）——孤儿子进程（管道写端持有者）
// 一并 SIGKILL，管道必关，命令永不永久挂起。
func toolBash(args map[string]interface{}) (string, error) {
	command, ok := requireNonEmptyStr(args, "command")
	if !ok {
		return "", fmt.Errorf("参数 command 缺失或不是字符串")
	}
	// 上限来自配置 tools.timeout_ms（2026-10-02；旧为硬编码 600）。
	//
	// 粒度契约：配置以**毫秒**为单位，但执行上限按**秒向下取整**——超时语义是
	// "最多等多久"，向下取整保证实际等待不超过配置值（向上取整会让 1001ms 变成
	// 等 2 秒，违背配置意图）。因此 timeout_ms=1999 → 上限 1 秒（静默丢弃 999ms），
	// timeout_ms 不足 1000 → limitSec=0 → 回落默认 600 秒（见下方 `< 1` 分支）。
	limitSec := int(toolTimeoutMS.Load()) / 1000
	if limitSec < 1 {
		limitSec = DefaultToolTimeoutMS / 1000
	}
	timeoutSec := limitSec
	if v, ok := args["timeout_seconds"]; ok {
		if f, ok := numArg(v); ok && f > 0 {
			// 必须先在 float64 域 clamp 再转 int：numArg 的 Float64 分支只挡 NaN/±Inf，
			// 不挡 1e300 这类"有限但远超 int64"的值。直接 int(f) 是 Go 规范的
			// implementation-specific 溢出（amd64 得最小 int64 负数，arm64 可能饱和为正
			// 最大值），随后被 `< 1` 兜底反转成 1 秒——模型意图"跑很久"变成"1 秒被杀"，
			// 且跨架构行为不一致。先在 float64 域收敛到 [1, limitSec]，永不进溢出区。
			if f > float64(limitSec) {
				f = float64(limitSec)
			}
			if f < 1 {
				f = 1
			}
			timeoutSec = int(f)
		}
	}
	// 双保险：理论上上面已夹紧，但保留兜底以防御未来改动（如新增其它赋值路径）。
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	if timeoutSec > limitSec { // 模型可下调，不可越过配置上限
		timeoutSec = limitSec
	}

	// 方案 B（2026-09-15）：命令文本进 /tmp 临时脚本，不进 argv。pkill/pgrep -f 按完整
	// 命令行（argv）匹配——旧 `bash -c <全文>` 把命令文本挂在包装壳 argv 上，任何命中
	// 命令文本的 -f 模式必然匹配到包装壳自己（自杀 exit 143 / 自匹配假 pid，2026-09-15
	// 实测事故）。执行脚本文件是 bash 原生形态：命令文本既不在 argv 也不在 environ。
	// 文件在 toolBash 返回时删除（= 前台命令结束），绑的是本函数的 defer，不是真正的
	// 进程退出：命令转后台、或 setsid 完全脱离后本函数已返回，脚本此时已被删除，而
	// 命令本身仍在运行。（提前 unlink 不中断执行，故此举无副作用。）
	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("okhuman-bash-%d-%s.sh", time.Now().UnixMilli(), randHex(4)))
	if err := os.WriteFile(scriptPath, []byte(command), 0o700); err != nil {
		return "", fmt.Errorf("写临时脚本失败: %w", err)
	}
	defer os.Remove(scriptPath)
	cmd := exec.Command("bash", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 独立进程组
	// 审计 H3 修（2026-09-14）：输出按流封顶 32MB（防单条命令灌 GB 级输出
	// OOM 整个进程；600s 只限时间不限大小）。超上限部分丢弃但持续读取
	// （管道不堵，子进程不挂起），正文留前 32MB + 截断说明。
	stdout := newCappedBuffer(maxToolOutputBytes)
	stderr := newCappedBuffer(maxToolOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	// 超时标记用 atomic.Bool（2026-09-14 审计修：旧裸 bool 由 AfterFunc
	// goroutine 写、主 goroutine 读，无同步 → 数据竞争）。
	//
	// 竞态修（2026-10-04，两轮）：旧写法在计时器回调里**先** killed.Store(true) 后 Kill。
	// 若命令恰好在计时器触发后、SIGKILL 生效前自行 exit(0)，Wait 返回 nil（code=0），
	// 但 killed 已为 true → 输出"退出码: 0（超过 N 秒上限被强制终止）"——自相矛盾
	// （实测相位扫描命中 ~2-3%）。
	//
	// 第一层修（done 标志 + Kill 成功才置位 killed）经实测**不足以**根除：
	// SIGKILL 送达时若组首进程已退出但尚未被 Wait 回收（僵尸态），Kill 返回 nil
	// （杀僵尸是成功的空操作），killed 仍被置位，而 Wait 随后报告正常退出 0。
	// 即"Kill 成功"推不出"是我们杀死的"。
	//
	// 第二层修（根治）：权威信号来自 wait 状态——Go 的 ExitError.Sys() 给出
	// syscall.WaitStatus，Signaled() 为真表示进程**死于信号**（而非自己的退出码）。
	// 只有 killed（我们发过 kill）**且** Signaled（进程确实死于此信号）才算超时终止；
	// 进程自行 exit(0)/exit(N) 则不论 killed 与否都不加超时文案（见下方 timedOut 计算）。
	//
	// 注意：仍保留 done 快路径（Wait 已返回 → 回调直接不做），缩小无谓的 kill 尝试。
	var killed atomic.Bool
	var done atomic.Bool
	timer := time.AfterFunc(time.Duration(timeoutSec)*time.Second, func() {
		if done.Load() { // Wait 已返回 → 命令自行结束，不是超时
			return
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
			killed.Store(true)
		} else if err := cmd.Process.Kill(); err == nil {
			killed.Store(true) // 组已不存在时兜底杀顶层，成功才计为超时
		}
	})
	defer timer.Stop()

	waitErr := cmd.Wait()
	done.Store(true)
	code := 0
	signaled := false
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				signaled = ws.Signaled()
			}
		} else {
			code = -1
		}
	}
	// 超时终止的权威判定：我们发过 kill **且** 进程死于信号。
	// 只 killed 不看 signaled 会把"命令自行 exit(0)"误报成超时（退出码 0 + 超时文案）；
	// 只 signaled 不看 killed 会把"命令自己 kill -9 自己"误报成超时。
	timedOut := isTimeoutKill(killed.Load(), signaled)
	head := fmt.Sprintf("退出码: %d", code)
	if timedOut {
		// 光说"已超时"不够：模型分不清"命令自己失败"与"被上限杀掉"，会误判重试。
		// 给出上限值与出路（脱离进程组 / 调大配置）。
		head += fmt.Sprintf("（超过 %d 秒上限被强制终止；要跑更久：用 setsid + 重定向完全脱离进程组，或调大配置 tools.timeout_ms）", timeoutSec)
	}
	out := stdout.String()
	errOut := stderr.String()
	body := out
	if errOut != "" {
		if body != "" {
			body += "\n"
		}
		body += "[stderr]\n" + errOut
	}
	if body != "" {
		return head + "\n\n" + body, nil
	}
	return head, nil
}

// isTimeoutKill 判定一次执行是否属于"被本工具的超时机制强制终止"。
//
// 抽成纯函数是为了可确定性单测：真实竞态是概率性的（~2-3%），
// 靠集成测试反复跑无法可靠区分真修与假修（变异测试会漏网）。
//
// 参数语义：
//   - killed：计时器回调是否成功向进程组/顶层进程发出过 SIGKILL；
//   - signaled：被 Wait 回收的进程是否死于信号（syscall.WaitStatus.Signaled()）。
//
// 两个条件缺一不可：
//   - 只看 killed：命令在计时器触发的同一瞬间自行 exit(0) 时，Kill 会打在
//     尚未回收的僵尸组上并"成功"返回，killed=true 而进程其实正常退出 →
//     输出"退出码: 0（超过 N 秒上限被强制终止）"的自相矛盾文案。
//   - 只看 signaled：命令自己 `kill -9 $$` 也会 Signaled=true，但那不是超时。
func isTimeoutKill(killed, signaled bool) bool {
	return killed && signaled
}
