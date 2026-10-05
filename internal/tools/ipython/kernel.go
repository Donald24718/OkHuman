package ipython

// JupyterKernel 的替代品：一个 IPython 进程 = 一个持久 REPL，宿主用
// stdin/stdout 上的 JSON Lines 与它对话。
//
// 为什么不用 Jupyter/ZMQ（实测结论，见 docs/ipython-最优解-级联中断实证.md）：
//   - Windows 上 ipykernel **拒收** ZMQ interrupt_request（kernelbase.py 明写
//     "Interrupt message not supported on Windows"），却仍回 status:ok —— 静默假成功；
//   - Windows 上唯一有效的跨进程中断手段是 jupyter_client 自己造的 Win32 Event，
//     而该 handle 活在 launcher 进程内，宿主拿不到也无法 SetEvent。
// pipe 路线把「执行用户代码」和「接收宿主控制指令」放进**同一个 Python 进程**，
// 于是可以直接用 signal 原语自中断，不需要任何 OS IPC。
//
// 进程模型：本包只起一个 python 进程（launcher.py），它自身不 fork 子内核，
// 所以 Kill 一棵进程树的问题不存在（对比 bash 工具需要 Job Object / setsid）。

import (
	"bufio"
	"crypto/rand"
	_ "embed" // go:embed launcher.py 所必需（侧效导入）
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 超时与级联参数（均经双平台实测校准）。
const (
	// startTimeout 等待 launcher 就绪。IPython 自身的 import 在冷机器上可能
	// 很慢（实测本机首次可达数十秒，取决于磁盘与包缓存），给 60s 兜底。
	startTimeout = 60 * time.Second

	// cascadeWait 级联中断每级之间的观察窗口。实测有效时均在 0.1~0.3s 内
	// 命中，1.5s 足以判定本级失效而不至于拖慢整体超时体验。
	cascadeWait = 1500 * time.Millisecond

	// maxCascadeStage 与 launcher 的 _interrupt_stage 的 1..3 对应。
	maxCascadeStage = 3

	// launcherMaxLine 单行响应上限（DataFrame 的 text/html 可能很大）。
	// 超出后 bufio.Scanner 报 ErrTooLong，本包将其视为协议错误而非崩溃。
	launcherMaxLine = 32 << 20

	// stderrTail 保留 launcher 进程 stderr 的尾部字节数（崩溃诊断用）。
	stderrTail = 4096
)

// ErrDead 内核已不可用（进程退出或被上一次超时硬杀回收）。
var ErrDead = errors.New("ipython: 内核已终止")

// Response launcher 的一行 JSON 响应（与 docs/assets/ipython_launcher.py 对齐）。
//
// Result 是 MIME → 文本的映射（text/plain / text/html / image/png 等）。
type Response struct {
	ID          any                 `json:"id"`
	OK          bool                `json:"ok"`
	Stdout      string              `json:"stdout"`
	Stderr      string              `json:"stderr"`
	Result      map[string]string   `json:"result"`
	Displays    []map[string]string `json:"displays"`
	Error       map[string]string   `json:"error"`
	Count       *int                `json:"count"`
	Interrupted bool                `json:"interrupted"`
	Ready       bool                `json:"ready"`
	Python      string              `json:"python"`
	IPython     string              `json:"ipython"`
}

// ExecResult 一次执行的整理结果（平台无关，供上层格式化）。
type ExecResult struct {
	Stdout      string              // 用户代码的 print 输出
	Stderr      string              // 用户代码的 stderr（含 IPython 的警告）
	Result      map[string]string   // 最后一个表达式的 rich display：MIME → 内容
	Displays    []map[string]string // display() 主动推送的内容（一个 cell 可多次）
	Saved       []Artifact          // 无法内联、已落盘的产物
	ErrName     string              // 异常类型（type name）
	ErrValue    string              // 异常消息
	Count       *int                // In[N] 序号
	Interrupted bool                // 被宿主中断（软中断成功）
	Killed      bool                // 级联全部失败，已杀进程（变量丢失）
	Lost        bool                // 进程意外退出
	Elapsed     time.Duration
}

// OK 报告本次执行是否"干净完成"：既无异常，也无中断/硬杀/丢进程。
func (r ExecResult) OK() bool {
	return r.ErrName == "" && !r.Interrupted && !r.Killed && !r.Lost
}

// ResultKeys 结果的 MIME 列表（字典序：保证日志/测试输出确定性）。
func (r ExecResult) ResultKeys() []string {
	out := make([]string, 0, len(r.Result))
	for k := range r.Result {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ResultTexts 结果的全部 MIME 内容（诊断与测试用）。
func (r ExecResult) ResultTexts() []string {
	keys := r.ResultKeys()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, r.Result[k])
	}
	return out
}

// InterruptedOrKilled 报告本次执行是否因超时而中止（不区分软/硬）。
func (r ExecResult) InterruptedOrKilled() bool { return r.Interrupted || r.Killed }

// Kernel 一个持久 IPython 进程。
//
// 并发契约：IPython 的执行只有一个主线程，因此 Kernel 的 Exec **串行化**
// （mu 保护整段 Exec）。这既是正确性要求（单 REPL 不能并行跑 cell），
// 也简化了收包逻辑（不需要多路复用 id 的路由表）。
type Kernel struct {
	pythonPath string
	launcher   string // 具现化后的 launcher 脚本路径

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	tail    *tailBuffer
	seq     int64

	pyVer  string // 就绪握手回传的 Python 版本
	ipyVer string // 就绪握手回传的 IPython 版本

	dead atomic.Bool
}

// NewKernel 创建但未启动（懒启动：首次 Exec 时才拉起进程）。
func NewKernel(pythonPath string) *Kernel {
	return &Kernel{pythonPath: pythonPath, tail: &tailBuffer{max: stderrTail}}
}

// String 便于日志/调试识别。
func (k *Kernel) String() string { return "ipython-kernel(" + k.pythonPath + ")" }

// Alive 内核进程是否还在可接受新请求的状态。
func (k *Kernel) Alive() bool { return !k.dead.Load() && k.cmd != nil }

// Version 返回就绪握手里的 python / IPython 版本（未启动时为空串）。
func (k *Kernel) Version() (python, ipython string) { return k.pyVer, k.ipyVer }

// start 拉起 launcher 并等待就绪握手（必须在持有 mu 时调用）。
func (k *Kernel) start() error {
	if k.cmd != nil {
		return nil
	}
	launcherPath, err := MaterializeLauncher()
	if err != nil {
		return fmt.Errorf("具现化 launcher 失败: %w", err)
	}
	k.launcher = launcherPath

	cmd := exec.Command(k.pythonPath, launcherPath) // #nosec G204 -- pythonPath 来自配置，非外部输入
	cmd.Env = append(os.Environ(),
		// PYTHONUNBUFFERED 是关键：否则 launch 侧的 stdout 走块缓冲，
		// 一行 JSON 迟迟不出，宿主只能一直等到超时。
		"PYTHONUNBUFFERED=1",
		"PYTHONIOENCODING=utf-8",
		// MPLBACKEND=Agg：matplotlib 的无头兜底后端。
		//
		// 实测原因：agent 进程没有显示器，后端自动选择会落到 qtagg / tkagg 这类
		// GUI 后端（本机实测 tkagg），导入时要去摸 GUI 栈，慢且可能卡住。
		//
		// 为什么是 Agg 而不是 inline：这是**兜底**。正常情况下 launcher 启动时
		// 会把后端切成 module://matplotlib_inline.backend_inline（那样 plt.show()
		// 才能出图）；只有在 matplotlib_inline 缺失时这一层才生效，此时 Agg
		// 至少保证 import 不炸、渲染仍可用（只是 show() 无效）。
		"MPLBACKEND=Agg",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("创建 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	cmd.Stderr = k.tail

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 Python 失败: %w", err)
	}
	k.cmd, k.stdin = cmd, stdin
	k.scanner = bufio.NewScanner(stdout)
	k.scanner.Buffer(make([]byte, 0, 64*1024), launcherMaxLine)

	// 就绪握手：第一行必须是 {"ready": true}（launcher 起完 InteractiveShell 后发送）。
	if err := k.readReady(); err != nil {
		_ = k.harvest()
		k.markDead()
		return err
	}
	return nil
}

func (k *Kernel) readReady() error {
	done := make(chan error, 1)
	go func() {
		var r Response
		for {
			if !k.scanner.Scan() {
				if err := k.scanner.Err(); err != nil {
					done <- fmt.Errorf("launcher 就绪握手失败: %w", err)
					return
				}
				done <- fmt.Errorf("launcher 进程已退出（未打印就绪）: %s", k.tail.String())
				return
			}
			line := k.scanner.Bytes()
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			if err := json.Unmarshal(line, &r); err != nil {
				// 启动期可能有第三方库往 stdout 打东西 —— 跳过继续等握手。
				continue
			}
			if r.Ready {
				k.pyVer, k.ipyVer = r.Python, r.IPython
				done <- nil
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(startTimeout):
		return fmt.Errorf("等待 launcher 就绪超过 %v（stderr: %s）", startTimeout, k.tail.String())
	}
}

// Exec 执行一段代码。
//
// 超时语义：**先软后硬**。超时先发 interrupt；级联三级全部无效才杀进程。
// 软中断成功的关键价值是变量保留（对比硬杀会丢失全部会话状态）。
func (k *Kernel) Exec(code string, timeout time.Duration) (ExecResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	var res ExecResult
	if err := k.start(); err != nil {
		return res, err
	}
	if timeout <= 0 {
		timeout = startTimeout
	}
	started := time.Now()
	k.seq++
	id := k.seq

	if err := k.writeJSON(map[string]any{"id": id, "code": code}); err != nil {
		k.markDead()
		return res, fmt.Errorf("写入执行请求失败: %w", err)
	}

	type outcome struct {
		r   *Response
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, err := k.readFor(id)
		ch <- outcome{r, err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	stage := 0
	for {
		select {
		case oc := <-ch:
			res.Elapsed = time.Since(started)
			if oc.err != nil {
				res.Lost = true
				k.markDead()
				res.Stderr = strings.TrimSpace(k.tail.String())
				return res, fmt.Errorf("读取响应失败: %w", oc.err)
			}
			applyResponse(oc.r, &res)
			return res, nil

		case <-timer.C:
			stage++
			if stage > maxCascadeStage {
				// 三级级联用完 → 硬杀。这是唯一会丢状态的路径，必须如实告知。
				_ = k.harvest()
				k.markDead()
				res.Elapsed = time.Since(started)
				res.Killed = true
				res.Stderr = strings.TrimSpace(k.tail.String())
				return res, nil
			}
			// 升级手段而不是重复发同一个：stage 1 在无效果是操作系统层面的
			// 投递失败（见 launcher._interrupt_stage 里的平台差异），再发一次
			// 同样的指令也不会成功，必须换一条路径。
			if err := k.writeJSON(map[string]any{"cmd": "interrupt", "stage": stage}); err != nil {
				// 管道已断 = 进程已死，下一次 select 会走 killed 分支或 read 报错。
				_ = k.harvest()
				k.markDead()
				res.Killed, res.Elapsed = true, time.Since(started)
				return res, nil
			}
			timer.Reset(cascadeWait)
		}
	}
}

// applyResponse 把 launcher 响应整理成 ExecResult。
//
// ⚠️ 判定中断的铁律（实测）：IPython 的 run_cell 会**内部消化** KeyboardInterrupt
// ——它不往上抛，而是变成 success=false + error_in_exec。此时 launcher 的
// `interrupted` 字段保持 false，**只有** error.ename == "KeyboardInterrupt" 能
// 证明这是宿主中断而非用户代码报错。只认 interrupted 字段，会把超时静默
// 误报成"用户代码出错"。
func applyResponse(r *Response, res *ExecResult) {
	res.Stdout = r.Stdout
	res.Stderr = r.Stderr
	res.Result = r.Result
	res.Displays = r.Displays
	res.Count = r.Count
	if r.Error != nil {
		res.ErrName = r.Error["ename"]
		res.ErrValue = r.Error["evalue"]
	}
	res.Interrupted = r.Interrupted || res.ErrName == "KeyboardInterrupt"
}

// readFor 读取 id 匹配的响应；不匹配的行（如别的请求的迟到响应）被丢弃。
//
// 这层过滤是必需的：硬杀/级联失败后，旧请求的响应可能在下一次 Exec 时才
// 到达，若无 id 匹配会被当成新请求的结果。
func (k *Kernel) readFor(id int64) (*Response, error) {
	for k.scanner.Scan() {
		line := strings.TrimSpace(k.scanner.Text())
		if line == "" {
			continue
		}
		var r Response
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // 用户代码往真 stdout 写东西会污染协议流，跳过
		}
		if sameID(r.ID, id) {
			return &r, nil
		}
	}
	if err := k.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// sameID 容忍 JSON number 解码成 float64 的精度表象：1 → 1.0，仍等于 1。
func sameID(v any, id int64) bool {
	switch n := v.(type) {
	case float64:
		return int64(n) == id
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i == id
		}
	case int:
		return int64(n) == id
	case int64:
		return n == id
	}
	return false
}

func (k *Kernel) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = k.stdin.Write(b)
	return err
}

// harvest 终止进程并回收（避免僵尸）。调用方需已判定进程不再可用。
func (k *Kernel) harvest() error {
	if k.cmd == nil {
		return nil
	}
	if k.stdin != nil {
		_ = k.stdin.Close()
		k.stdin = nil
	}
	if k.cmd.Process != nil {
		_ = k.cmd.Process.Kill()
	}
	err := k.cmd.Wait()
	k.cmd, k.scanner = nil, nil
	return err
}

func (k *Kernel) markDead() { k.dead.Store(true) }

// Shutdown 请求 launcher 优雅退出，失败则强杀。
func (k *Kernel) Shutdown() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.markDead()
	if k.cmd == nil {
		return
	}
	_ = k.writeJSON(map[string]any{"cmd": "shutdown"})
	done := make(chan error, 1)
	go func() { done <- k.harvest() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = k.harvest()
	}
}

// ---------------------------------------------------------------- launcher 分发

//go:embed launcher.py
var launcherSrc []byte

// launcherDirOnce 具现化目录（每进程一次），用随机后缀避免多实例互相踩踏。
var launcherDirOnce sync.Once
var launcherDirVal string

func launcherDir() string {
	launcherDirOnce.Do(func() {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			launcherDirVal = filepath.Join(os.TempDir(), "okhuman-ipython")
			return
		}
		launcherDirVal = filepath.Join(os.TempDir(), "okhuman-ipython-"+hex.EncodeToString(b))
	})
	return launcherDirVal
}

// MaterializeLauncher 把内嵌的 launcher 脚本写到磁盘并返回路径。
//
// 为什么不直接用 os.TempDir() 根：记忆里的坑——Windows 上 Go 的 os.TempDir()
// 与 Git Bash 的 /tmp 是**同一个目录**，模型一句 `rm -rf /tmp/*` 会清空它。
// 放到带随机后缀的子目录里，且每次 kernel 创建都重写（自愈），
// 既不误伤他人临时文件，被删了也能自动恢复。
//
// 写临时文件再 rename：多 kernel 并发创建时不会读到写了一半的脚本。
func MaterializeLauncher() (string, error) {
	dir := launcherDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(dir, "launcher.py")
	if fi, err := os.Stat(target); err == nil && fi.Size() == int64(len(launcherSrc)) {
		return target, nil // 已存在且大小一致 → 复用（写入是幂等的）
	}
	tmp := filepath.Join(dir, fmt.Sprintf("launcher.%d.tmp", os.Getpid()))
	if err := os.WriteFile(tmp, launcherSrc, 0o600); err != nil {
		return "", fmt.Errorf("写临时 launcher 失败: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("安装 launcher 失败: %w", err)
	}
	return target, nil
}

// tailBuffer 保留末尾 max 字节的写入者（proc stderr 诊断用）。
//
// 为什么不能用无限增长的 buffer：长期会话里第三方库的 warning 会不断累积；
// 而我们只在「进程崩溃」时才需要看它，此时有用的永远是最新的几行。
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
