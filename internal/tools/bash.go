package tools

// bash 元工具实现。第二个元工具（如 ipython）应新建同目录 ipython.go，
// 不要塞进本文件；本文件只放 bash 特有逻辑，公共设施见 output.go / args.go。
// 平台无关的契约见 exec_domain.go；平台专属的进程控制见 bash_unix.go / bash_windows.go。

import (
	"fmt"
	"os"
	"path/filepath"
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
	// 进程退出：命令转后台、或（Unix）setsid 完全脱离后本函数已返回，脚本此时已被删除，
	// 而命令本身仍在运行。（提前 unlink 不中断执行，故此举无副作用。）
	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("okhuman-bash-%d-%s.sh", time.Now().UnixMilli(), randHex(4)))
	if err := os.WriteFile(scriptPath, []byte(command), 0o700); err != nil {
		return "", fmt.Errorf("写临时脚本失败: %w", err)
	}
	defer os.Remove(scriptPath)
	// 审计 H3 修（2026-09-14）：输出按流封顶 32MB（防单条命令灌 GB 级输出
	// OOM 整个进程；600s 只限时间不限大小）。超上限部分丢弃但持续读取
	// （管道不堵，子进程不挂起），正文留前 32MB + 截断说明。
	stdout := newCappedBuffer(maxToolOutputBytes)
	stderr := newCappedBuffer(maxToolOutputBytes)

	// 平台专属执行：经 executor 接口（Unix 用独立进程组 + kill(-pgid)；
	// Windows 用 Job Object，阶段 3）。startErr 非 nil 表示进程**未能启动**
	// （对应原 `cmd.Start()` 的 error 语义）——原实现在此处直接 `return "", err`，
	// 外化为接口后由平台实现回传，语义保持不变。
	info, startErr := platformExecutor().Run(scriptPath, timeoutSec, stdout, stderr)
	if startErr != nil {
		return "", startErr
	}

	// 权威判定用双条件（方案 §3.1：ExitReason 不取代 isTimeoutKill）。
	// 平台实现的 Killed/Signaled 是原始条件，本处重算，得到与 Unix 原实现逐字同源的判定。
	timedOut := isTimeoutKill(info.Killed, info.Signaled)
	// 交叉校验：平台自报 Reason 若与双条件不一致 = 平台实现 bug（不改变判定，只暴露）。
	if (info.Reason == ExitTimedOut) != timedOut {
		info.Degraded = joinDegraded(info.Degraded,
			fmt.Sprintf("reason-mismatch: 平台自报 %s，双条件判定 timedOut=%v", info.Reason, timedOut))
	}

	head := fmt.Sprintf("退出码: %d", info.Code)
	if timedOut {
		// 光说"已超时"不够：模型分不清"命令自己失败"与"被上限杀掉"，会误判重试。
		// 给出上限值与出路（平台专属：Unix 用 setsid 脱离；Windows 无等价手段，见 detachHint）。
		head += fmt.Sprintf("（超过 %d 秒上限被强制终止；要跑更久：%s）", timeoutSec, detachHintBrief())
	}
	// 不静默降级（方案 §3.2）：降级说明必须出现在**模型可见的返回值**里，而非仅日志。
	if info.Degraded != "" {
		head += "\n[平台降级] " + info.Degraded
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
