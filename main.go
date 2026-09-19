// OkHuman 入口（单实例，2026-09-09 多 agent 模块移除）：
//
//	./okhuman                 # 读 config/（default.json + user.json）
//	./okhuman /path/prompts   # 可选：指定提示词目录（覆盖 system_prompt.dir）
//
// 落盘：cfg.data.dir 后自动追加端口后缀 -<port>（多实例同机互不干扰）。
//
// 多 agent 不再内置：一个 OkHuman 进程 = 一个 agent；需要多个 agent 时
// 起多个实例（不同端口），各自独立落盘目录、独立上下文。定时任务由独立插件仓
// 经 POST /chat 触发本实例。
//
// 环境变量 OKHUMAN_FAKE=1 时用 FakeLLM（不烧 token，脚本化行为，测试用）。
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"okhuman/internal/config"
	"okhuman/internal/llm"
	"okhuman/internal/persist"
	"okhuman/internal/prompt"
	"okhuman/internal/server"
	"okhuman/internal/types"
)

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// utf16Len UTF-16 code units 长度（对齐 TS text.length）
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
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

	// ---------- 可选提示词目录（命令行第一个非 flag 参数） ----------
	// 指定则覆盖 cfg.system_prompt.dir（解析为绝对路径）；缺省用 config 里的相对目录。
	var promptDirRel string
	for _, arg := range os.Args[1:] {
		if arg == "" || strings.HasPrefix(arg, "-") {
			fatalf("非法启动参数：%s（格式：可选一个提示词目录绝对路径）", arg)
		}
		abs, err := filepath.Abs(arg)
		if err != nil {
			fatalf("非法启动参数：%s（格式：可选一个提示词目录绝对路径）", arg)
		}
		promptDirRel = abs
		break
	}

	// ---------- 端口冲突回退（2026-09-15）：配置端口被占用 → 依次 +1 直到成功 ----------
	// 用一次性 listener 探测（bind 成功 = 可用，随即关闭）；末尾真实服务器再绑同一端口。
	// 探测→真实窗口毫秒级，本机仅并发启动另一个 OkHuman 才可能抢占，届时以 EADDRINUSE 明确报错。
	// 端口若被顶走，落盘目录后缀 -<port>（下段）随实际绑定端口走，保持"一端口一目录"。
	host := cfg.Server.Host
	port := cfg.Server.Port
	for {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			_ = ln.Close()
			break
		}
		if strings.Contains(strings.ToLower(err.Error()), "in use") {
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

	useFake := os.Getenv("OKHUMAN_FAKE") == "1"

	// ---------- 建 agent 运行时（单实例） ----------
	var client llm.LlmClient
	if useFake {
		client = llm.NewFakeLLM([]llm.FakeStep{
			{Type: "tool", Name: "bash", Args: map[string]interface{}{"command": "echo hello-okhuman"}},
			{Type: "text", Text: "（FakeLLM）命令已执行，输出见工具结果。"},
		})
	} else {
		client = llm.NewOpenAiClient(llm.ClientConfig{BaseURL: cfg.LLM.BaseURL, Model: cfg.LLM.Model, APIKey: cfg.LLM.APIKey, TimeoutMS: cfg.LLM.TimeoutMS})
	}

	// 提示词：命令行目录 > config 的 system_prompt.dir（相对项目根）
	// 命令行的绝对路径目录直接读；config 相对目录走旧布局（default 即根 prompts/）
	var promptDir string
	switch {
	case promptDirRel != "":
		promptDir = promptDirRel
	case strings.HasPrefix(cfg.SystemPrompt.Dir, "/"):
		promptDir = cfg.SystemPrompt.Dir
	default:
		promptDir = filepath.Join(root, cfg.SystemPrompt.Dir)
	}
	loaded := prompt.LoadPromptDir(promptDir)

	promptFiles := make([]server.PromptFileInfo, 0, len(loaded.Files))
	for _, f := range loaded.Files {
		promptFiles = append(promptFiles, server.PromptFileInfo{Name: f.Name, Chars: f.Chars})
	}
	a := server.CreateAgentState(cfg, client, loaded.Prompt, promptFiles, loaded.Fallback, promptDir)

	// 恢复落盘会话（重启续接）
	sessSnap, _ := persist.LoadSessionFile(dataDir)
	if sessSnap != nil {
		a.SessionNo.Store(int32(sessSnap.No))
		a.CM().Restore(sessSnap.State)
		a.CM().SetSeq(sessSnap.Seq)
		// 注：对齐 TS——restore() 不更新 cm 的会话号（记录文件名沿用建 CM 时的会话号）
	}
	// 会话变更 → 实时同步落盘
	a.CM().AddPersistListener(func(s *types.SessionState, seq int) {
		_ = p.SaveSession(&persist.Snapshot{No: int(a.SessionNo.Load()), Seq: seq, State: s})
	})

	fileNames := make([]string, 0, len(loaded.Files))
	for _, f := range loaded.Files {
		fileNames = append(fileNames, f.Name)
	}
	promptSrc := ""
	switch {
	case loaded.Fallback:
		promptSrc = "built-in default"
	case promptDirRel != "":
		promptSrc = fmt.Sprintf("%s/（%s）", promptDirRel, strings.Join(fileNames, ", "))
	default:
		promptSrc = fmt.Sprintf("%s/（%s）", cfg.SystemPrompt.Dir, strings.Join(fileNames, ", "))
	}
	restored := "（新建）"
	if sessSnap != nil {
		restored = ""
	}
	log(fmt.Sprintf("[OkHuman] 就绪：prompt=%s（%d 字符） | llm=%s | session#%d%s | data=%s",
		promptSrc, utf16Len(loaded.Prompt), cfg.LLM.BaseURL, int(a.SessionNo.Load()), restored, dataDir))

	// ---------- 服务状态（单实例） ----------
	appState := server.NewAppState(cfg, root, a)

	app := server.New(appState, &server.Hooks{
		OnSessionRebuilt: func(a *server.AgentState) {
			// /reset 重建 cm 后重挂落盘监听，并立即落盘空会话（2026-08-30 修）：
			// 监听器只在 addMessage 时触发，若不立即写，重启会载入旧快照
			// （会话号回退 + 旧消息"复活"）。
			a.CM().AddPersistListener(func(s *types.SessionState, seq int) {
				_ = p.SaveSession(&persist.Snapshot{No: int(a.SessionNo.Load()), Seq: seq, State: s})
			})
			_ = p.SaveSession(&persist.Snapshot{No: int(a.SessionNo.Load()), Seq: a.CM().GetSeq(), State: a.CM().Session()})
			log(fmt.Sprintf("[OkHuman] 会话已重置 #%d，落盘监听已重挂（空会话已落盘）", int(a.SessionNo.Load())))
		},
		OnConfigSaved: func(patch map[string]interface{}) {
			saveGlobalConfigPatch(root, patch)
			keys := make([]string, 0, len(patch))
			for k := range patch {
				keys = append(keys, k)
			}
			log(fmt.Sprintf("[config] 全局配置已保存并即时生效（落盘 config/user.json）：%s", strings.Join(keys, ", ")))
		},
	})

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
	if err := srv.ListenAndServe(); err != nil {
		fatalf("http 服务退出：%v", err)
	}
}

// ---------- 全局配置补丁落盘（WebUI 配置页保存后，2026-08-29） ----------

// deepMergeMaps 普通对象深合并（patch 覆盖 dst，嵌套对象递归）
func deepMergeMaps(dst, patch map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(dst)+len(patch))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range patch {
		if pv, ok := v.(map[string]interface{}); ok {
			if dv, ok2 := out[k].(map[string]interface{}); ok2 {
				out[k] = deepMergeMaps(dv, pv)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// jsonEqual 两值 JSON 相等（Object.is 语义：同类型同值）
func jsonEqual(a, b interface{}) bool {
	if af, ok := a.(float64); ok {
		bf, ok2 := b.(float64)
		return ok2 && af == bf
	}
	if as, ok := a.(string); ok {
		bs, ok2 := b.(string)
		return ok2 && as == bs
	}
	if ab, ok := a.(bool); ok {
		bb, ok2 := b.(bool)
		return ok2 && ab == bb
	}
	return false
}

// saveGlobalConfigPatch 读 user.json → 深合并补丁 → 剔"等于出厂默认"的叶子键
// （文件保持最小）→ 原子写回
//
// cfgSaveMu 串行化读改写（2026-09-14 审计修：并发 /config POST 的读-改-写
// 互相覆盖 → 丢失更新）
var cfgSaveMu sync.Mutex

func saveGlobalConfigPatch(root string, patch map[string]interface{}) {
	cfgSaveMu.Lock()
	defer cfgSaveMu.Unlock()
	file := filepath.Join(root, "config", "user.json")
	var cur map[string]interface{}
	if s, err := os.ReadFile(file); err == nil {
		var d interface{}
		if json.Unmarshal(s, &d) == nil {
			if m, ok := d.(map[string]interface{}); ok {
				cur = m
			}
		}
	}
	next := deepMergeMaps(cur, patch)

	// 清理"冗余"的叶子键，文件保持最小。
	// 基线 = 不含 user.json 的生效值（DEFAULTS+default.json），不是裸 DEFAULTS（2026-09-15 修）：
	// default.json 覆盖过的键，user 里"等于出厂默认"的值正是把它掰回来的覆盖，删掉即静默
	// 改变生效配置（8451 事故：user.json 的 server.port:8451 被删，实例身份被 default.json
	// 偷成 8452，下次重启撞端口崩死）。
	var base map[string]interface{}
	if b, err := json.Marshal(config.Default()); err == nil {
		_ = json.Unmarshal(b, &base)
	}
	if s, err := os.ReadFile(filepath.Join(root, "config", "default.json")); err == nil {
		var ds map[string]interface{}
		if json.Unmarshal(s, &ds) == nil {
			base = deepMergeMaps(base, ds)
		}
	}
	for s, sec := range next {
		secM, ok := sec.(map[string]interface{})
		if !ok {
			continue
		}
		bsec, _ := base[s].(map[string]interface{})
		for k, v := range secM {
			if bv, exists := bsec[k]; exists && jsonEqual(v, bv) {
				delete(secM, k)
			}
		}
		if len(secM) == 0 {
			delete(next, s)
		}
	}
	out, _ := json.MarshalIndent(next, "", "  ")
	_ = persist.AtomicWriteFile(file, string(out))
}
