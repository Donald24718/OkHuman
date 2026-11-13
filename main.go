// OkHuman 入口（单实例，2026-09-09 多 agent 模块移除）：
//
//	./okhuman                 # 读 config/（default.json + user.json）
//	./okhuman /path/prompts   # 可选：指定提示词目录（覆盖 system_prompt.dir）
//
// 落盘：cfg.data.dir 后自动追加端口后缀 -<port>（多实例同机互不干扰）。
//
// 多 agent 不再内置：一个 OkHuman 进程 = 一个 agent；需要多个 agent 时
// 起多个实例（不同端口），各自独立落盘目录、独立上下文。定时任务由独立插件仓
// 经 POST /enqueue 投递本实例。
//
// 环境变量 OKHUMAN_FAKE=1 时用 FakeLLM（不烧 token，脚本化行为，测试用）。
package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"okhuman/internal/textutil"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"okhuman/internal/config"
	"okhuman/internal/llm"
	"okhuman/internal/persist"
	"okhuman/internal/prompt"
	"okhuman/internal/server"
	"okhuman/internal/tools"
	"okhuman/internal/types"
)

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// promptDirFromArgs 取命令行里那个**可选的**提示词目录（第一个非 flag 参数）并解析成
// 绝对路径；没有参数则返回空串。空串或以 "-" 开头按用法错误处理（与旧内联写法同语义）。
// 2026-11-12 从 main() 抽出：这段有三条优先级、最容易写错，且不该只能靠启动来验。
func promptDirFromArgs(args []string) (string, error) {
	const usage = "非法启动参数：%s（格式：可选一个提示词目录绝对路径）"
	for _, arg := range args {
		if arg == "" || strings.HasPrefix(arg, "-") {
			return "", fmt.Errorf(usage, arg)
		}
		abs, err := filepath.Abs(arg)
		if err != nil {
			return "", fmt.Errorf(usage, arg)
		}
		return abs, nil
	}
	return "", nil
}

// resolvePromptDir 提示词目录优先级：命令行给的目录 > config 里的绝对目录 >
// root + config 里的相对目录（旧布局：default 即项目根 prompts/）。
func resolvePromptDir(argDir, root, cfgDir string) string {
	switch {
	case argDir != "":
		return argDir
	case strings.HasPrefix(cfgDir, "/"):
		return cfgDir
	default:
		return filepath.Join(root, cfgDir)
	}
}
func main() {
	// root = 可执行文件目录（TS: fileURLToPath(new URL("..", import.meta.url)) = 项目根）
	exe, err := os.Executable()
	if err != nil {
		fatalf("取可执行文件路径失败：%v", err)
	}
	root := filepath.Dir(exe)

	cfg, err := config.Load(root)
	if err != nil {
		fatalf("配置加载失败：%v", err)
	}

	// 工具超时接线（2026-10-02）：tools.timeout_sec / fg_timeout_sec 灌进 tools 包。
	// 执行上限与注入给模型的描述文本同源生成——改配置即改模型看到的数字，
	// 不再出现"配置可改、描述写死"的不一致。
	// 配置以秒为单位（2026-10-18 单位统一）→ 内部仍按毫秒计时，这里换算一次
	tools.Configure(cfg.Tools.FgTimeoutSec*1000, cfg.Tools.TimeoutSec*1000)

	// ---------- 可选提示词目录（命令行第一个非 flag 参数） ----------
	// 指定则覆盖 cfg.system_prompt.dir（解析为绝对路径）；缺省用 config 里的相对目录。
	promptDirAbs, err := promptDirFromArgs(os.Args[1:])
	if err != nil {
		fatalf("%v", err)
	}

	// ---------- 端口冲突回退（2026-09-15）：配置端口被占用 → 依次 +1 直到成功 ----------
	// 端口：直接绑到可用端口，并把绑定得到的 listener 留着交给 Serve —— 不再“探测→关闭→再监听”。
	// 旧写法两者之间有窗口，同机并启时另一个实例可能抢走，表现为随机换端口。
	// 占用判定用 errors.Is(EADDRINUSE)：错误文案随 Go 版本/平台变，字符串匹配不可靠。
	// 端口若被顶走，落盘目录后缀 -<port>（下段）随实际绑定端口走，保持“一端口一目录”。
	host := cfg.Server.Host
	port := cfg.Server.Port

	var ln net.Listener
	for {
		ln, err = net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			break // 直接留着这个 listener 交给 Serve：不再“关掉再重听”，免得中间被抢
		}
		if errors.Is(err, syscall.EADDRINUSE) {
			port++
			fmt.Fprintf(os.Stderr, "[server] 端口 %d 被占用，试 %d\n", port-1, port)
			continue
		}
		fatalf("端口 %d 绑定失败: %v", port, err)
	}
	if port != cfg.Server.Port {
		fmt.Fprintf(os.Stderr, "[server] 配置端口 %d → 实际使用 %d\n", cfg.Server.Port, port)
		cfg.Server.Port = port
	}

	// ---------- 落盘根目录：自动追加端口后缀（多实例同机互不干扰） ----------
	dataDir := cfg.Data.Dir
	suffix := fmt.Sprintf("-%d", cfg.Server.Port)
	if !strings.HasSuffix(dataDir, suffix) {
		dataDir += suffix
	}
	cfg.Data.Dir = dataDir

	p, err := persist.Init(dataDir)
	if err != nil {
		fatalf("落盘初始化失败：%v", err)
	}
	log := func(msg string) { p.Log(msg) }

	// 工具命令脚本落点（C5，2026-10-18）：实例自己的 <data>/bash-jobs，不再混进共享
	// /tmp；并在启动时清掉上次被强杀留下的残留——defer os.Remove 在 SIGKILL 下不跑，
	// 信号处理也拦不住 SIGKILL，所以兜底必须放在启动时，不依赖优雅退出。
	tools.SetScriptDir(filepath.Join(dataDir, "bash-jobs"))
	tools.SetSelfID(port, dataDir) // 向工具子进程声明身份（见 tools.SetSelfID）
	if n := tools.CleanupStaleScripts(); n > 0 {
		log(fmt.Sprintf("[tools] 已清理上次遗留的命令脚本 %d 个（%s）", n, filepath.Join(dataDir, "bash-jobs")))
	}

	useFake := os.Getenv("OKHUMAN_FAKE") == "1"

	// ---------- 建 agent 运行时（单实例） ----------
	var client llm.LlmClient
	if useFake {
		client = llm.NewFakeLLM([]llm.FakeStep{
			{Type: "tool", Name: "bash", Args: map[string]interface{}{"command": "echo hello-okhuman"}},
			{Type: "text", Text: "（FakeLLM）命令已执行，输出见工具结果。"},
			// 压缩请求（自动或手动）走这条：Deltas = 流式增量，让"压缩中"卡片在
			// fake 模式下也能看到思考与正文逐段出现（演示/手测 UI 用）。
			{Type: "compress", Text: "（FakeLLM）早前对话要点：任务、结论、待办。", Deltas: []types.Delta{
				{Thinking: "（fake）先扫最旧一批消息…"},
				{Text: "（fake）提炼任务与结论…"},
				{Text: "（fake）写入滚动 summary。"},
			}},
		})
	} else {
		client = llm.NewOpenAiClient(llm.ClientConfig{BaseURL: cfg.LLM.BaseURL, Model: cfg.LLM.Model, APIKey: cfg.LLM.APIKey, TimeoutMS: cfg.LLM.TimeoutSec * 1000, StreamIdleMS: cfg.LLM.StreamIdleSec * 1000, MaxResponseBytes: cfg.LLM.MaxResponseBytes})
		if oc, ok := client.(*llm.OpenAiClient); ok {
			oc.SetLogger(log) // 协议层异常（畸形帧等）要进 okhuman.log，否则无从查起
		}
	}

	// 提示词：命令行目录 > config 的 system_prompt.dir（相对项目根）
	// 命令行的绝对路径目录直接读；config 相对目录走旧布局（default 即根 prompts/）
	promptDir := resolvePromptDir(promptDirAbs, root, cfg.SystemPrompt.Dir)
	loaded := prompt.LoadPromptDir(promptDir)

	promptFiles := make([]server.PromptFileInfo, 0, len(loaded.Files))
	for _, f := range loaded.Files {
		promptFiles = append(promptFiles, server.PromptFileInfo{Name: f.Name, Chars: f.Chars})
	}
	a := server.CreateAgentState(cfg, client, loaded.Prompt, promptFiles, loaded.Fallback, promptDir)

	// 恢复落盘会话（重启续接）
	// 退出前把日志队列排空（2026-10-19）：日志改为异步单写手后，进程被 kill 时
	// 队列里未落盘的行会丢——丢的恰恰是"最后一次 run / 最后一次请求"的记录，
	// 也就是排查死因最需要的部分。收到 SIGTERM/SIGINT 先 flush 再退。
	// 会话本身每次消息后已同步原子落盘，不靠这里。
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		p.FlushLog()
		os.Exit(0)
	}()

	sessSnap, _ := persist.LoadSessionFile(dataDir)
	if sessSnap != nil {
		a.SessionNo.Store(int32(sessSnap.No))
		a.CM().Restore(sessSnap.State)
		a.CM().SetSeq(sessSnap.Seq)
		// 2026-09-25 修：cm 会话号也同步为落盘值。此前重启续接后压缩存档误标
		// ctx-1（建 CM 时的默认号，与磁盘上的真实会话号脱节，如 #4 会话出 ctx-1）
		a.CM().SetSessionNo(int(sessSnap.No))
	}
	// 会话变更 → 实时同步落盘
	a.CM().AddPersistListener(newPersistSave(p,
		func() int { return int(a.SessionNo.Load()) }, int(a.SessionNo.Load()), log))

	fileNames := make([]string, 0, len(loaded.Files))
	for _, f := range loaded.Files {
		fileNames = append(fileNames, f.Name)
	}
	promptSrc := ""
	switch {
	case loaded.Fallback:
		promptSrc = "built-in default"
	case promptDirAbs != "":
		promptSrc = fmt.Sprintf("%s/（%s）", promptDirAbs, strings.Join(fileNames, ", "))
	default:
		promptSrc = fmt.Sprintf("%s/（%s）", cfg.SystemPrompt.Dir, strings.Join(fileNames, ", "))
	}
	if len(loaded.Unreadable) > 0 {
		log(fmt.Sprintf("[prompt] ⚠ %d 个 .md 读不了（权限/IO）：%s", len(loaded.Unreadable), strings.Join(loaded.Unreadable, ", ")))
	}
	restored := "（新建）"
	if sessSnap != nil {
		restored = ""
	}
	log(fmt.Sprintf("[OkHuman] 就绪：prompt=%s（%d 字符） | llm=%s | session#%d%s | data=%s",
		promptSrc, textutil.UTF16Len(loaded.Prompt), cfg.LLM.BaseURL, int(a.SessionNo.Load()), restored, dataDir))

	// ---------- 服务状态（单实例） ----------
	appState := server.NewAppState(cfg, root, a)
	appState.Log = log // server 层落盘日志（2026-10-18 修 D：关键操作进 okhuman.log）

	app := server.New(appState, &server.Hooks{
		OnSessionRebuilt: func(a *server.AgentState) {
			// /reset 重建 cm 后重挂落盘监听，并立即落盘空会话（2026-08-30 修）：
			// 监听器只在 addMessage 时触发，若不立即写，重启会载入旧快照
			// （会话号回退 + 旧消息"复活"）。
			a.CM().AddPersistListener(newPersistSave(p,
				func() int { return int(a.SessionNo.Load()) }, int(a.SessionNo.Load()), log))
			if err := p.SaveSession(&persist.Snapshot{No: int(a.SessionNo.Load()), Seq: a.CM().GetSeq(), State: a.CM().Session()}); err != nil {
				p.Log("[persist] 会话快照写入失败（下一次变更会再试）：" + err.Error())
			}
			log(fmt.Sprintf("[OkHuman] 会话已重置 #%d，落盘监听已重挂（空会话已落盘）", int(a.SessionNo.Load())))
		},
		// 只负责写盘并把错误如实返回；留痕与响应由 server 层统一做（见 handleConfigPost），
		// 否则日志取决于 hook 实现，测试与真实路径会不一致（2026-10-18 复查修 1）。
		OnConfigSaved: func(patch map[string]interface{}) error {
			return saveGlobalConfigPatch(root, patch)
		},
	})

	// 空闲心跳（2026-10-17 用户定）：agent 持续无 run 到点 → 把心跳提示词入队并
	// 主动开启一轮。开关/间隔/提示词在 config 的 heartbeat 段（WebUI 配置页可改，
	// 每 tick 现取配置 → 无需重启）。计时口径：只有 run 结束才重置，正在 run 不计时。
	stopHeartbeat := server.StartHeartbeat(appState, a)
	defer stopHeartbeat()

	llmLabel := cfg.LLM.BaseURL
	if useFake {
		llmLabel = "FakeLLM"
	}
	log(fmt.Sprintf("[OkHuman] http://%s:%d | LLM=%s | data=%s（%s 日志；%s 会话，实时落盘） | maxTokens=%d resultLimit=%d",
		cfg.Server.Host, cfg.Server.Port, llmLabel, dataDir, persist.LogFile, persist.SessionFile,
		cfg.Context.MaxTokens, cfg.Tools.ResultLimit))

	// HTTP 服务启动（2026-08-29）：
	// IdleTimeout 不设（=0 禁用）：Bun.serve 默认 idleTimeout=10s，SSE 流式聊天在
	// LLM 思考 / 工具执行期间可能连续 >10s 无数据帧，默认值会把连接直接掐断。
	// 长连接场景必须禁用。
	// ReadHeaderTimeout 30s（2026-09-14 审计修：旧实现全零超时，慢速客户端可
	// 无限占用连接——slowloris；只限"读请求头"阶段，SSE 长连接不受影响）
	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:           app,
		ReadHeaderTimeout: 30 * time.Second,
	}
	if err := srv.Serve(ln); err != nil {
		fatalf("http 服务退出：%v", err)
	}
}

// ---------- 全局配置补丁落盘（WebUI 配置页保存后，2026-08-29） ----------

// saveGlobalConfigPatch 读 user.json → 深合并补丁 → 剔"等于出厂默认"的叶子键
// （文件保持最小）→ 原子写回
//
// cfgSaveMu 串行化读改写（2026-09-14 审计修：并发 /config POST 的读-改-写
// 互相覆盖 → 丢失更新）
var cfgSaveMu sync.Mutex

// newPersistSave 落盘监听回调（C4，2026-10-18）：会话号在**注册时**快照，写盘前比对。
//
// 监听器挂在具体某个 cm 上；/reset 会重建 cm 并重挂监听（见 Hooks.OnSessionRebuilt）。
// 旧实现写时才读全局 a.SessionNo：万一旧 run 的尾巴事件在 reset 之后才到达，就会把
// 旧会话的状态按**新会话号**写进 session.json（会话号回退 + 旧消息"复活"）。
// 现在：出生会话号 ≠ 当前会话号 → 本监听器属于已被取代的会话，丢弃并记一行日志。
//
// 前提不变：/reset 持 RunLock（H1）保证正常路径没有在飞 run 越界。本守卫是把
// "靠调用方守规矩"变成"违反就丢并留证据"——不改事件/回调契约（这种错配概率极低，
// 改契约要动 emit 与前端，性价比不划算）。
func newPersistSave(p *persist.Persistence, currentNo func() int, bornNo int, logf func(string)) func(*types.SessionState, int) {
	return func(s *types.SessionState, seq int) {
		if cur := currentNo(); cur != bornNo {
			logf(fmt.Sprintf("[persist] 丢弃迟到的会话写入：监听器属于会话 #%d，当前会话 #%d（seq=%d）", bornNo, cur, seq))
			return
		}
		if err := p.SaveSession(&persist.Snapshot{No: bornNo, Seq: seq, State: s}); err != nil {
			logf("[persist] 会话快照写入失败（下一次变更会再试）：" + err.Error())
		}
	}
}

// saveGlobalConfigPatch 落盘一次配置补丁。合并 / 剥冗余 / 拒绝半截 JSON 这些规则在
// config.SaveUserPatch（那里能被测试覆盖），这里只保证"读-改-写"整段串行。
func saveGlobalConfigPatch(root string, patch map[string]interface{}) error {
	cfgSaveMu.Lock()
	defer cfgSaveMu.Unlock()
	return config.SaveUserPatch(root, patch)
}
