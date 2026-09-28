// scout —— 技能/插件混合检索服务（Go 版）
//
// agent 要用技能/插件前，先问 scout"这事该用哪个"：它扫两目录入库（名称+简介+路径），
// 按 名称关键词 > 简介关键词 > 语义向量 > 全局关键词 加权打分，返回 名称+简介+路径 清单。
//
// 语义向量后端：llama.cpp llama-server 常驻进程（Qwen3-Embedding GGUF）。
// scout 启动时若 embed 端口未就绪则自动拉起 llama-server（完全脱离，日志 /tmp/scout-embed.log），
// 已就绪则直接复用（scout 重启不必重载模型）。模型缺席/拉不起时语义项关闭，
// 纯关键词检索照跑（优雅降级，与 TS 版行为一致）。
//
// 用法:
//
//	./scout           启动服务 (127.0.0.1:8480)
//	./scout reindex   重建一次索引后退出（embed 服务常驻：拉起后不关闭，下次复用不重载）
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	pageSize  = 5
	maxTotal  = 50
	batchSize = 16 // 每次 /v1/embeddings 批量文本数
)

// ---------- 配置 ----------

type Config struct {
	Port       int    `json:"port"`
	PluginRoot string `json:"plugin_root"`
	ModelPath  string `json:"model_path"`          // GGUF 路径
	ServerBin  string `json:"server_bin"`          // llama-server 可执行文件
	EmbedPort  int    `json:"embed_port"`          // llama-server 监听端口
	EmbedCtx   int    `json:"embed_ctx,omitempty"` // 上下文长度（默认 1024）
}

func loadConfig(dir string) Config {
	cfg := Config{Port: 8480, EmbedPort: 8591, EmbedCtx: 1024}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		log.Printf("[scout] 无 config.json（%v），用默认值", err)
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Printf("[scout] config.json 解析失败: %v", err)
	}
	if cfg.Port == 0 {
		cfg.Port = 8480
	}
	if cfg.EmbedPort == 0 {
		cfg.EmbedPort = 8591
	}
	if cfg.EmbedCtx == 0 {
		cfg.EmbedCtx = 1024
	}
	return cfg
}

// ---------- 分词：中文 字符 unigram+bigram（免分词库），英文/数字 整词 ----------

func isCJK(r rune) bool {
	return (r >= 0x4e00 && r <= 0x9fff) || (r >= 0x3400 && r <= 0x4dbf)
}

// 中英双向词桥：补关键词层的跨语言缺口（随语料扩充词表）
var xling = map[string][]string{
	"video": {"视频"}, "视频": {"video"},
	"music": {"音乐"}, "音乐": {"music"}, "song": {"歌曲"}, "歌曲": {"song"},
	"audio": {"音频"}, "音频": {"audio"},
	"voice": {"语音"}, "语音": {"voice"}, "speech": {"语音"},
	"subtitle": {"字幕"}, "字幕": {"subtitle"}, "caption": {"字幕"},
	"image": {"图片"}, "图片": {"image"}, "photo": {"照片"}, "照片": {"photo"},
	"animation": {"动画"}, "动画": {"animation"},
	"game": {"游戏"}, "游戏": {"game"},
	"browser": {"浏览器"}, "浏览器": {"browser"},
	"screen": {"屏幕"}, "屏幕": {"screen"}, "screenshot": {"截图"}, "截图": {"screenshot"},
	"click": {"点击"}, "点击": {"click"},
	"comment": {"评论"}, "评论": {"comment"}, "reply": {"回复"}, "回复": {"reply"},
	"post": {"发帖"}, "发帖": {"post"}, "publish": {"发布"}, "发布": {"publish"},
	"upload": {"上传"}, "上传": {"upload"}, "download": {"下载"}, "下载": {"download"},
	"cron": {"定时"}, "定时": {"cron"}, "schedule": {"定时"},
	"attach": {"附件"}, "附件": {"attach"},
	"channel": {"频道"}, "频道": {"channel"},
	"chat": {"聊天"}, "聊天": {"chat"}, "message": {"消息"}, "消息": {"message"},
	"bilibili": {"哔哩哔哩"}, "哔哩哔哩": {"bilibili"},
	"h3": {"H3"},
}

func tokenize(s string) []string {
	norm := strings.ToLower(s)
	out := []string{}
	runes := []rune(norm)
	i := 0
	for i < len(runes) {
		c := runes[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			j := i
			for j < len(runes) && ((runes[j] >= 'a' && runes[j] <= 'z') || (runes[j] >= '0' && runes[j] <= '9')) {
				j++
			}
			out = append(out, string(runes[i:j]))
			i = j
		} else if isCJK(c) {
			j := i
			for j < len(runes) && isCJK(runes[j]) {
				j++
			}
			run := runes[i:j]
			for k := range run {
				out = append(out, string(run[k]))
			}
			for k := 0; k+1 < len(run); k++ {
				out = append(out, string(run[k])+string(run[k+1]))
			}
			i = j
		} else {
			i++
		}
	}
	// 词桥扩充
	extra := []string{}
	for _, t := range out {
		for _, x := range xling[t] {
			if !contains(out, x) && !contains(extra, x) {
				extra = append(extra, x)
			}
		}
	}
	return append(out, extra...)
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// coverage queryTokens 在 field 里的覆盖率 |q∩f|/|q|
func coverage(queryTokens []string, field string) float64 {
	qset := map[string]bool{}
	for _, t := range queryTokens {
		qset[t] = true
	}
	if len(qset) == 0 {
		return 0
	}
	fset := map[string]bool{}
	for _, t := range tokenize(field) {
		fset[t] = true
	}
	hit := 0
	for t := range qset {
		if fset[t] {
			hit++
		}
	}
	return float64(hit) / float64(len(qset))
}

// ---------- 向量后端（llama-server /v1/embeddings） ----------

type embedServer struct {
	cfg     Config
	client  *http.Client
	cmd     *exec.Cmd // 本进程拉起的 llama-server（复用已存在的则为 nil）
	started bool      // 是否由本进程拉起（reindex CLI 退出时关闭）
}

func newEmbedServer(cfg Config) *embedServer {
	return &embedServer{cfg: cfg, client: &http.Client{Timeout: 120 * time.Second}}
}

// healthOK embed 端口是否已就绪（llama-server /health）
func (e *embedServer) healthOK() bool {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", e.cfg.EmbedPort))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return resp.StatusCode == 200 && strings.Contains(string(b), `"ok"`)
}

// ensure 就绪检查；未就绪则拉起 llama-server 并等待（最长 3 分钟）
func (e *embedServer) ensure() (ok bool, err error) {
	if e.healthOK() {
		log.Printf("[scout] embed 后端 :%d 已就绪，复用（不重载模型）", e.cfg.EmbedPort)
		return true, nil // 复用已存在的（含上次 scout 拉起的孤儿进程）
	}
	logf, lerr := os.OpenFile("/tmp/scout-embed.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if lerr != nil {
		return false, lerr
	}
	args := []string{
		"-m", e.cfg.ModelPath,
		"--port", strconv.Itoa(e.cfg.EmbedPort),
		"--host", "127.0.0.1",
		"--embedding", "--no-webui",
		"-c", strconv.Itoa(e.cfg.EmbedCtx),
	}
	cmd := exec.Command(e.cfg.ServerBin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // 完全脱离，scout 死它独立存活
	if cerr := cmd.Start(); cerr != nil {
		return false, fmt.Errorf("拉起 llama-server 失败: %w", cerr)
	}
	e.cmd, e.started = cmd, true
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		if e.healthOK() {
			log.Printf("[scout] embedder ON（llama-server pid %d，模型 %s）", cmd.Process.Pid, filepath.Base(e.cfg.ModelPath))
			return true, nil
		}
		time.Sleep(2 * time.Second)
	}
	e.stop()
	return false, fmt.Errorf("embedder 180 秒内没就绪（日志 /tmp/scout-embed.log）")
}

// stop 关闭本进程拉起的 llama-server（进程组）
func (e *embedServer) stop() {
	if e.cmd != nil && e.cmd.Process != nil {
		syscall.Kill(-e.cmd.Process.Pid, syscall.SIGTERM)
		e.cmd.Wait()
	}
}

// embedTexts 批量向量化（L2 归一；llama-server 默认已归一，这里幂等保险）
func (e *embedServer) embedTexts(texts []string) ([][]float32, error) {
	var out [][]float32
	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		chunk := texts[i:end]
		payload, _ := json.Marshal(map[string]any{"model": "qwen3-embedding", "input": chunk, "encoding_format": "float"})
		resp, err := e.client.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/embeddings", e.cfg.EmbedPort), "application/json", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			tail := body
			if len(tail) > 300 {
				tail = tail[:300]
			}
			return nil, fmt.Errorf("/v1/embeddings %d: %s", resp.StatusCode, string(tail))
		}
		var r struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if jerr := json.Unmarshal(body, &r); jerr != nil {
			return nil, jerr
		}
		vecs := make([][]float32, len(chunk))
		for _, d := range r.Data {
			if d.Index < 0 || d.Index >= len(chunk) || len(d.Embedding) == 0 {
				continue
			}
			v := d.Embedding
			n := 0.0
			for _, x := range v {
				n += float64(x) * float64(x)
			}
			if n > 0 {
				n = 1 / math.Sqrt(n)
				for k := range v {
					v[k] *= float32(n)
				}
			}
			vecs[d.Index] = v
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// ---------- 语料扫描 ----------

type corpusDoc struct {
	Name    string
	Summary string
	Path    string
	Source  string
}

func scanCorpus(cfg Config) []corpusDoc {
	docs := []corpusDoc{}
	seen := map[string]bool{}
	// 递归扫描（2026-09-21：插件目录改为分类两层结构 Frequently Used Plugins/ 等，
	// 顶层 ReadDir 漏掉第二层插件）。插件目录 = 含非空 summary 的 meta.json 的目录，
	// 任意深度均可命中；无 meta.json 的容器目录/运行时资产自然跳过。
	_ = filepath.WalkDir(cfg.PluginRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 不可读子目录：跳过不中断
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != cfg.PluginRoot && strings.HasPrefix(name, ".") {
			return filepath.SkipDir
		}
		// 运行时资产目录（venv/模型/声纹）不进去走，省时间
		if path != cfg.PluginRoot {
			switch name {
			case "venv", "kokoro_venv", "asr", "speaker", "data", "MiniMax-H3-Skills":
				return filepath.SkipDir
			}
		}
		b, e2 := os.ReadFile(filepath.Join(path, "meta.json"))
		if e2 != nil {
			return nil
		}
		var meta struct {
			Name    string `json:"name"`
			Summary string `json:"summary"`
		}
		if json.Unmarshal(b, &meta) != nil {
			return nil
		}
		pluginName := name
		if n := strings.TrimSpace(meta.Name); n != "" {
			pluginName = n
		}
		summary := strings.TrimSpace(meta.Summary)
		if summary == "" {
			return nil // 无 summary → 跳过（简介一律模型自写，不抓文件内容）
		}
		if seen[pluginName] {
			return nil // 名字冲突：保留先出现的
		}
		seen[pluginName] = true
		docs = append(docs, corpusDoc{pluginName, summary, path, "plugin"})
		return nil
	})
	return docs
}


// ---------- 索引 ----------

type Doc struct {
	Name      string    `json:"name"`
	Summary   string    `json:"summary"`
	Path      string    `json:"path"`
	Source    string    `json:"source"`
	Vec       []float32 `json:"vec,omitempty"`
	UpdatedAt string    `json:"updated_at"`
}

type Index struct {
	Dim  int   `json:"dim"`
	Docs []Doc `json:"docs"`
}

type State struct {
	mu       sync.RWMutex
	index    Index
	dim      int
	modelOn  bool
	idxFile  string
	embedder *embedServer
}

func (st *State) load() {
	b, err := os.ReadFile(st.idxFile)
	if err != nil {
		return
	}
	if json.Unmarshal(b, &st.index) == nil {
		st.dim = st.index.Dim
	}
}

func (st *State) save() error {
	tmp := st.idxFile + ".tmp"
	b, err := json.MarshalIndent(st.index, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, st.idxFile)
}

func (st *State) count() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.index.Docs)
}

// reindex 重扫语料：embedding 并行算，索引原子落盘；manual 条目保留
func (st *State) reindex() map[string]any {
	st.mu.RLock()
	modelOn := st.modelOn
	st.mu.RUnlock()
	corpus := scanCorpus(st.embedder.cfg)
	texts := make([]string, len(corpus))
	for i, d := range corpus {
		texts[i] = d.Name + "。" + d.Summary
	}
	vecs := make([][]float32, len(corpus))
	if modelOn {
		v, err := st.embedder.embedTexts(texts)
		if err != nil {
			log.Printf("[scout] reindex embedding 失败（降级为纯关键词）: %v", err)
			modelOn = false
		} else {
			vecs = v
		}
	}
	st.mu.Lock()
	oldCorpus := map[string]bool{}
	manual := []Doc{}
	for _, d := range st.index.Docs {
		switch d.Source {
		case "manual":
			manual = append(manual, d)
		case "skill", "plugin":
			oldCorpus[d.Name] = true
		}
	}
	docs := append([]Doc{}, manual...)
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	seen := map[string]bool{}
	for i, d := range corpus {
		var v []float32
		if modelOn && i < len(vecs) {
			v = vecs[i]
		}
		docs = append(docs, Doc{d.Name, d.Summary, d.Path, d.Source, v, now})
		seen[d.Name] = true
	}
	st.index.Docs = docs
	if len(vecs) > 0 {
		st.dim = len(vecs[0])
	}
	st.modelOn = modelOn
	st.index.Dim = st.dim
	st.mu.Unlock()
	removed := 0
	for name := range oldCorpus {
		if !seen[name] {
			removed++
		}
	}
	if err := st.save(); err != nil {
		log.Printf("[scout] 索引落盘失败: %v", err)
	}
	n := st.count()
	return map[string]any{
		"indexed": len(corpus),
		"removed": removed,
		"total":   n,
		"model":   boolStr(modelOn),
	}
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ---------- 打分 ----------
// score = 0.40*nameKw + 0.25*summaryKw + 0.20*semantic + 0.15*kwAll
// 模型缺席时 semantic 项关闭，总分按 ×1/0.8 归一（保持 0..1 量纲）

type breakdown struct {
	Score  float64  `json:"score"`
	NameKw float64  `json:"nameKw"`
	SumKw  float64  `json:"sumKw"`
	KwAll  float64  `json:"kwAll"`
	Sem    *float64 `json:"sem"`
}

func r4(x float64) float64 {
	return float64(int64(x*10000+0.5)) / 10000
}

func dot(a, b []float32) float64 {
	s := 0.0
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func scoreDoc(d Doc, qTokens []string, q string, qVec []float32, modelOn bool) breakdown {
	ql := strings.ToLower(strings.TrimSpace(q))
	nl := strings.ToLower(d.Name)
	var nameKw float64
	if nl == ql {
		nameKw = 1
	} else if strings.HasPrefix(nl, ql) || strings.HasPrefix(ql, nl) {
		nameKw = 0.9
	} else {
		nameKw = 0.6*coverage(qTokens, d.Name) + 0.4*coverage(tokenize(d.Name), q)
	}
	sumKw := coverage(qTokens, d.Summary)
	kwAll := coverage(qTokens, d.Name+" "+d.Summary+" "+d.Path)
	var sem *float64
	if modelOn && qVec != nil && len(d.Vec) > 0 {
		s := dot(qVec, d.Vec)
		if s < 0 {
			s = 0
		}
		sem = &s
	}
	base := 0.4*nameKw + 0.25*sumKw + 0.15*kwAll
	score := base / 0.8
	if sem != nil {
		score = base + 0.2*(*sem)
	}
	out := breakdown{r4(score), r4(nameKw), r4(sumKw), r4(kwAll), nil}
	if sem != nil {
		v := r4(*sem)
		out.Sem = &v
	}
	return out
}

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func main() {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	cfg := loadConfig(dir)

	// 可移植性：相对路径按可执行文件所在目录解析（配置随仓库走，clone 到哪都能跑）
	if !filepath.IsAbs(cfg.PluginRoot) {
		cfg.PluginRoot = filepath.Join(dir, cfg.PluginRoot)
	}
	if !filepath.IsAbs(cfg.ModelPath) {
		cfg.ModelPath = filepath.Join(dir, cfg.ModelPath)
	}

	st := &State{
		idxFile:  filepath.Join(dir, "index.json"),
		embedder: newEmbedServer(cfg),
	}
	st.mu.Lock()
	st.load()
	st.mu.Unlock()

	if len(os.Args) > 1 && os.Args[1] == "reindex" {
		ok, err := st.embedder.ensure()
		if err != nil {
			log.Printf("[scout] embedder 缺席（纯关键词索引）: %v", err)
		}
		st.modelOn = ok
		res := st.reindex()
		fmt.Println(mustJSON(res))
		return // embed 服务常驻：即使由本次 CLI 拉起也不关闭（复用不重载）
	}

	n := st.count()
	go func() {
		ok, err := st.embedder.ensure()
		if err != nil {
			log.Printf("[scout] embedder OFF（semantic 关闭）: %v", err)
		}
		st.mu.Lock()
		st.modelOn = ok
		st.mu.Unlock()
		if n == 0 {
			log.Println("[scout] 首次自动 reindex ...")
			res := st.reindex()
			log.Printf("[scout] 首次自动 reindex: %s", mustJSON(res))
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		st.mu.RLock()
		modelOn := st.modelOn
		st.mu.RUnlock()
		writeJSON(w, 200, map[string]any{"ok": true, "port": cfg.Port, "docs": st.count(), "model": boolStr(modelOn)})
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, 405, map[string]any{"error": "GET only"})
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		if q == "" {
			writeJSON(w, 400, map[string]any{"error": "missing q"})
			return
		}
		qTokens := tokenize(q)
		st.mu.RLock()
		docs := st.index.Docs
		modelOn := st.modelOn
		st.mu.RUnlock()
		var qVec []float32
		if modelOn {
			if vs, err := st.embedder.embedTexts([]string{q}); err == nil && len(vs) > 0 {
				qVec = vs[0]
			}
		}
		type scored struct {
			d Doc
			s breakdown
		}
		scoredList := []scored{}
		for _, d := range docs {
			s := scoreDoc(d, qTokens, q, qVec, modelOn)
			if s.Score > 0.02 {
				scoredList = append(scoredList, scored{d, s})
			}
		}
		sort.SliceStable(scoredList, func(i, j int) bool {
			if scoredList[i].s.Score != scoredList[j].s.Score {
				return scoredList[i].s.Score > scoredList[j].s.Score
			}
			return scoredList[i].d.Name < scoredList[j].d.Name
		})
		total := len(scoredList)
		if total > maxTotal {
			total = maxTotal
		}
		lo := (page - 1) * pageSize
		hi := lo + pageSize
		if lo > len(scoredList) {
			lo = len(scoredList)
		}
		if hi > len(scoredList) {
			hi = len(scoredList)
		}
		results := []map[string]any{}
		for _, x := range scoredList[lo:hi] {
			results = append(results, map[string]any{
				"name": x.d.Name, "summary": x.d.Summary, "path": x.d.Path,
				"score": x.s.Score, "breakdown": x.s,
			})
		}
		writeJSON(w, 200, map[string]any{
			"q": q, "total": total, "page": page, "page_size": pageSize,
			"model": boolStr(modelOn), "results": results,
		})
	})
	mux.HandleFunc("/doc", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			name := r.URL.Query().Get("name")
			if name == "" {
				writeJSON(w, 400, map[string]any{"error": "name required"})
				return
			}
			st.mu.Lock()
			docs := []Doc{}
			for _, d := range st.index.Docs {
				if d.Name != name {
					docs = append(docs, d)
				}
			}
			st.index.Docs = docs
			st.mu.Unlock()
			st.save()
			writeJSON(w, 200, map[string]any{"ok": true, "deleted": name})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]any{"error": "POST/DELETE only"})
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			Name    string `json:"name"`
			Summary string `json:"summary"`
			Path    string `json:"path"`
		}
		if json.Unmarshal(body, &req) != nil || strings.TrimSpace(req.Name) == "" {
			writeJSON(w, 400, map[string]any{"error": "bad json / name required"})
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		var vec []float32
		st.mu.RLock()
		modelOn := st.modelOn
		st.mu.RUnlock()
		if modelOn {
			if vs, err := st.embedder.embedTexts([]string{req.Name + "。" + req.Summary}); err == nil && len(vs) > 0 {
				vec = vs[0]
			}
		}
		now := time.Now().UTC().Format("2006-01-02 15:04:05")
		st.mu.Lock()
		replaced := false
		for i, d := range st.index.Docs {
			if d.Name == req.Name {
				st.index.Docs[i] = Doc{req.Name, req.Summary, req.Path, "manual", vec, now}
				replaced = true
				break
			}
		}
		if !replaced {
			st.index.Docs = append(st.index.Docs, Doc{req.Name, req.Summary, req.Path, "manual", vec, now})
		}
		st.mu.Unlock()
		st.save()
		writeJSON(w, 200, map[string]any{"ok": true, "name": req.Name, "model": boolStr(modelOn)})
	})
	mux.HandleFunc("/reindex", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			writeJSON(w, 405, map[string]any{"error": "POST/GET only"})
			return
		}
		res := st.reindex()
		writeJSON(w, 200, res)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeJSON(w, 404, map[string]any{"error": "not found", "endpoints": []string{"/search?q=&page=", "/doc", "/reindex", "/health"}})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "scout ok — /health /search?q= /doc /reindex\n")
	})

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	log.Printf("[scout] listening on http://%s docs=%d model=%s（embed 后端 :%d）", addr, n, boolStr(st.modelOn), cfg.EmbedPort)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
