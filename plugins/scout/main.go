// scout —— 技能/插件检索服务（Go 版，纯关键词）
//
// agent 要用技能/插件前，先问 scout「这事该用哪个」：它扫两棵树入库（名称 + 关键词 + 简介 + 路径），
// 按 名称 > 关键词 > 简介 > 路径 加权打分，返回 名称+简介+路径 清单。
//
// 2026-11-05 两处方向性改动（实测数据见 README，别再重新讨论一遍）：
//  1. 拆掉语义向量。代价：一次 reindex 16.2 秒、llama-server 常驻 3.49 GB、查询 50ms 里大头是给查询词做
//     embedding；收益实测≈0（26 条直述能力词：带向量 R@1 21/26、不带 19/26；8 条口语式：带与不带都是 3/8）。
//     补回召回靠 meta.json 里的 keywords 字段（实测：补 keywords 后 直述 R@3 26/26、口语 R@3 8/8）。
//  2. 索引不落盘。真源是磁盘上的 meta.json/README，内存索引可再生：扫 25 个文件 2.6ms、1000 条约 165ms。
//     不持久化 = 不会拿旧快照骗人；外接盘没挂载时 docs=0，/health 里明说，不假装正常。
//
// 只支持 Linux（按用户的范围约定，不做交叉编译）。
//
// 用法:
//
//	./scout              启动服务 (127.0.0.1:8480)，启动即扫树建索引
//	POST /reindex        重扫（只处理变化项，返回 new/changed/unchanged/removed 计数）
//	GET  /doctor         当场重扫并与内存索引对照，报差异（只报告，不动数据）
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// 2026-11-05 用户定：一次给 10 条，不再靠翻页。理由是对的——调用方是 LLM，
	// 多几条候选比来回翻页便宜；page 参数仍保留（老调用方不破）。
	pageSize = 10
	maxTotal = 50
)

// ---------- 配置 ----------

type Config struct {
	Port       int    `json:"port"`
	PluginRoot string `json:"plugin_root"`
}

func loadConfig(dir string) Config {
	cfg := Config{Port: 8480}
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
	"view_image": {"附件", "看图", "读图", "attach_look"}, "attach_look": {"view_image"},
	"附件": {"view_image"}, "看图": {"view_image"}, "读图": {"view_image"}, "图片注入": {"view_image"},
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

// ---------- 语料扫描 ----------

type corpusDoc struct {
	Name     string
	Keywords string
	Summary  string
	Path     string
	Kind     string // plugin | skill
}

// splitInlineKeywords：历史上大家把触发词写在 summary 末尾的「关键词：…」里。
// 现在 keywords 是一等字段：显式 meta.json 的 keywords 优先，内联那截切出来并进去，
// 并从 summary 里摘掉——同一信号不能算两遍（否则 sumKw 虚高）。
func splitInlineKeywords(summary, explicit string) (string, string) {
	seps := []string{"关键词：", "关键词:"}
	cut := -1
	for _, s := range seps {
		if j := strings.Index(summary, s); j >= 0 && (cut < 0 || j < cut) {
			cut = j
		}
	}
	kw := strings.TrimSpace(explicit)
	if cut >= 0 {
		tail := strings.TrimSpace(summary[cut:])
		for _, s := range seps {
			tail = strings.TrimPrefix(tail, s)
		}
		tail = strings.TrimSpace(strings.Trim(tail, "*"))
		summary = strings.TrimSpace(summary[:cut])
		if kw == "" {
			kw = tail
		} else if tail != "" {
			kw = kw + " " + tail
		}
	}
	return summary, kw
}

// kindOf：skill 与 plugin 的区分看是否在 skills 树下（2026-11-05 前所有条目 source 都被误写成 plugin，
// 我因此判断错过一次，这里必须分对）。
func kindOf(path string) string {
	sep := string(filepath.Separator)
	for _, marker := range []string{"OkHuman_skill", "skills"} { // 公开仓的技能目录叫 skills/
		if strings.Contains(path, sep+marker+sep) {
			return "skill"
		}
	}
	return "plugin"
}

// metaFile：keywords 允许是字符串或字符串数组（2026-11-05：multi-gpu-torch 写成了数组，
// 我按 string 解码 → Unmarshal 报错 → 整条被静默丢掉，索引 26 变 25 才发现。
// 从此解析失败不再静默跳过，一律记进 problems，/health 与 /reindex 报计数、/doctor 列明细。
type metaFile struct {
	Name     string          `json:"name"`
	Summary  string          `json:"summary"`
	Keywords json.RawMessage `json:"keywords"`
}

type corpusProblem struct {
	Path  string `json:"path"`
	Issue string `json:"issue"`
}

func keywordsText(raw json.RawMessage) (string, string) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", ""
	}
	var str string
	if json.Unmarshal([]byte(s), &str) == nil {
		return strings.TrimSpace(str), ""
	}
	var arr []string
	if json.Unmarshal([]byte(s), &arr) == nil {
		return strings.TrimSpace(strings.Join(arr, " ")), ""
	}
	var anyv []any
	if json.Unmarshal([]byte(s), &anyv) == nil {
		out := make([]string, 0, len(anyv))
		for _, v := range anyv {
			out = append(out, fmt.Sprint(v))
		}
		return strings.TrimSpace(strings.Join(out, " ")), ""
	}
	return "", "keywords 必须是字符串或字符串数组，这条的 keywords 没被索引"
}

func scanCorpus(cfg Config) ([]corpusDoc, []corpusProblem) {
	docs := []corpusDoc{}
	problems := []corpusProblem{}
	// 递归扫描（2026-09-21：插件目录改为分类两层结构 Frequently Used Plugins/ 等，
	// 顶层 ReadDir 漏掉第二层插件）。插件目录 = 含非空 summary 的 meta.json 的目录，
	// 任意深度均可命中；无 meta.json 的容器目录/运行时资产自然跳过。
	_ = filepath.WalkDir(cfg.PluginRoot, func(path string, d fs.DirEntry, err error) (r error) {
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
			case "venv", "kokoro_venv", "asr", "speaker", "data":
				// 目录自己带 meta.json = 它就是插件本体，不能当资产跳过
				// （2026-10-16 asr 从 voice-chat 拆成独立插件，与旧资产目录名撞车，
				//   差点进不了索引：search 搜不到 asr）。
				if _, e := os.Stat(filepath.Join(path, "meta.json")); e != nil {
					return filepath.SkipDir
				}
			}
		}
		b, e2 := os.ReadFile(filepath.Join(path, "meta.json"))
		if e2 != nil {
			return nil
		}
		var meta metaFile
		if err := json.Unmarshal(b, &meta); err != nil {
			problems = append(problems, corpusProblem{path, "meta.json 解析失败：" + err.Error()})
			log.Printf("[scout] %s meta.json 解析失败: %v", path, err)
			return nil
		}
		kwVal, kwIssue := keywordsText(meta.Keywords)
		if kwIssue != "" {
			problems = append(problems, corpusProblem{path, kwIssue})
		}
		pluginName := name
		if n := strings.TrimSpace(meta.Name); n != "" {
			pluginName = n
		}
		summary, keywords := splitInlineKeywords(strings.TrimSpace(meta.Summary), kwVal)
		if summary == "" {
			return nil // 无 summary → 跳过（简介一律模型自写，不抓文件内容）
		}
		// 子技能不独立建索引（2026-11-05 用户定）：这个目录一旦自认成文档，它下面的一切
		// 都属于它的子模块/子技能——只通过母文档的路由表被发现，不在索引里各占一条。
		// （母文档必须点名每个子模块/子技能 + 触发词，这是 RULE 规范三的要求。）
		defer func() { r = filepath.SkipDir }()
		// 同名不去重（2026-11-05，用户的判断）：全部保留，让 agent 看见两处自己决定删哪个。
		// 原来的 `if seen[name] return` 会让谁被看见取决于 WalkDir 顺序 —— 那是真 bug。
		docs = append(docs, corpusDoc{pluginName, keywords, summary, path, kindOf(path)})
		return nil
	})
	return docs, problems
}

// ---------- 索引（只在内存；不落盘） ----------

type Doc struct {
	Name     string `json:"name"`
	Keywords string `json:"keywords,omitempty"`
	Summary  string `json:"summary"`
	Path     string `json:"path"`
	Kind     string `json:"kind"` // plugin | skill | manual
	Updated  string `json:"updated"`
}

type Index struct {
	Docs []Doc
}

type State struct {
	mu    sync.RWMutex
	cfg   Config
	index Index
}

func keyOf(d Doc) string { return d.Kind + ":" + d.Name + ":" + d.Path }

func (st *State) count() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.index.Docs)
}

// reindex 重扫语料。不落盘（真源是 meta.json），但逐条与上一版比对，
// 把 new/changed/unchanged/removed 报出来——「索引其实没变」这件事必须可见，
// 否则陈旧只能靠人肉发现（2026-11-05：副本上挂着已删插件的索引，没人知道）。
func (st *State) reindex() map[string]any {
	corpus, problems := scanCorpus(st.cfg)
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	st.mu.Lock()
	prev := map[string]Doc{}
	manual := []Doc{}
	for _, d := range st.index.Docs {
		if d.Kind == "manual" {
			manual = append(manual, d)
			continue
		}
		prev[keyOf(d)] = d
	}
	docs := append([]Doc{}, manual...)
	nNew, nCh, nUn := 0, 0, 0
	names := map[string]int{}
	nowKeys := map[string]bool{}
	for _, c := range corpus {
		d := Doc{c.Name, c.Keywords, c.Summary, c.Path, c.Kind, now}
		k := keyOf(d)
		nowKeys[k] = true
		if p, ok := prev[k]; ok {
			if p.Summary == d.Summary && p.Keywords == d.Keywords {
				nUn++
				d.Updated = p.Updated // 没变就别改时间戳
			} else {
				nCh++
			}
		} else {
			nNew++
		}
		docs = append(docs, d)
		names[d.Name]++
	}
	nRemoved := 0
	for k, d := range prev {
		if !nowKeys[k] {
			nRemoved++
			log.Printf("[scout] 已从索引移出: %s (%s)", d.Name, d.Path)
		}
	}
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].Name != docs[j].Name {
			return docs[i].Name < docs[j].Name
		}
		return docs[i].Path < docs[j].Path
	})
	st.index.Docs = docs
	same := 0
	for _, c := range names {
		if c > 1 {
			same++
		}
	}
	noKw := 0
	for _, d := range docs {
		if d.Kind != "manual" && strings.TrimSpace(d.Keywords) == "" {
			noKw++
		}
	}
	st.mu.Unlock()
	return map[string]any{
		"indexed": len(docs), "new": nNew, "changed": nCh, "unchanged": nUn,
		"removed": nRemoved, "same_name": same, "missing_keywords": noKw,
		"meta_problems": len(problems),
	}
}

// ---------- 打分 ----------
// score = 0.30*nameKw + 0.35*kwKw + 0.25*sumKw + 0.10*pathKw
// （2026-11-05：去掉语义项、加 keywords 独立字段。权重是在 34 条查询的对照实验里定的，见 README）

type breakdown struct {
	Score  float64 `json:"score"`
	NameKw float64 `json:"nameKw"`
	KwKw   float64 `json:"kwKw"`
	SumKw  float64 `json:"sumKw"`
	PathKw float64 `json:"pathKw"`
}

func r4(x float64) float64 {
	return float64(int64(x*10000+0.5)) / 10000
}

func scoreDoc(d Doc, qTokens []string, q string) breakdown {
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
	kwKw := coverage(qTokens, d.Keywords)
	sumKw := coverage(qTokens, d.Summary)
	pathKw := coverage(qTokens, strings.ReplaceAll(d.Path, string(filepath.Separator), " "))
	return breakdown{
		Score:  r4(0.30*nameKw + 0.35*kwKw + 0.25*sumKw + 0.10*pathKw),
		NameKw: r4(nameKw), KwKw: r4(kwKw), SumKw: r4(sumKw), PathKw: r4(pathKw),
	}
}

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, mustJSON(v))
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func main() {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	cfg := loadConfig(dir)
	st := &State{cfg: cfg}

	res := st.reindex() // 启动即建索引（扫 25 个文件实测 2.6ms；1000 条约 165ms）
	log.Printf("[scout] 索引: %s", mustJSON(res))

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		st.mu.RLock()
		same, noKw := sameNameCount(st.index.Docs), missingKeywords(st.index.Docs)
		st.mu.RUnlock()
		writeJSON(w, 200, map[string]any{
			"ok": true, "port": cfg.Port, "docs": st.count(),
			"same_name": same, "missing_keywords": noKw,
			"model": "off", // 语义向量已拆除（保留这个字段只为老调用方不报错）
		})
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
		st.mu.RUnlock()
		type scored struct {
			d Doc
			s breakdown
		}
		scoredList := []scored{}
		for _, d := range docs {
			s := scoreDoc(d, qTokens, q)
			if s.Score > 0.02 {
				scoredList = append(scoredList, scored{d, s})
			}
		}
		sort.SliceStable(scoredList, func(i, j int) bool {
			if scoredList[i].s.Score != scoredList[j].s.Score {
				return scoredList[i].s.Score > scoredList[j].s.Score
			}
			return scoredList[i].d.Path < scoredList[j].d.Path // 同分按路径稳定，不看遍历顺序
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
				"name": x.d.Name, "keywords": x.d.Keywords, "summary": x.d.Summary,
				"path": x.d.Path, "kind": x.d.Kind, "score": x.s.Score, "breakdown": x.s,
			})
		}
		writeJSON(w, 200, map[string]any{
			"q": q, "total": total, "page": page, "page_size": pageSize,
			"model": "off", "same_name": sameNameCount(docs), "results": results,
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
			writeJSON(w, 200, map[string]any{"ok": true, "deleted": name, "note": "索引在内存：重启后由 meta.json 重建，manual 条目会丢"})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]any{"error": "POST/DELETE only"})
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			Name     string `json:"name"`
			Keywords string `json:"keywords"`
			Summary  string `json:"summary"`
			Path     string `json:"path"`
		}
		if json.Unmarshal(body, &req) != nil || strings.TrimSpace(req.Name) == "" {
			writeJSON(w, 400, map[string]any{"error": "bad json / name required"})
			return
		}
		now := time.Now().UTC().Format("2006-01-02 15:04:05")
		d := Doc{strings.TrimSpace(req.Name), strings.TrimSpace(req.Keywords), strings.TrimSpace(req.Summary), req.Path, "manual", now}
		st.mu.Lock()
		replaced := false
		for i, x := range st.index.Docs {
			if x.Kind == "manual" && x.Name == d.Name {
				st.index.Docs[i] = d
				replaced = true
				break
			}
		}
		if !replaced {
			st.index.Docs = append(st.index.Docs, d)
		}
		st.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "name": d.Name})
	})
	mux.HandleFunc("/reindex", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			writeJSON(w, 405, map[string]any{"error": "POST/GET only"})
			return
		}
		res := st.reindex()
		log.Printf("[scout] reindex: %s", mustJSON(res))
		writeJSON(w, 200, res)
	})
	// /doctor：当场重扫，与内存索引对照，只报告不改数据。
	// 用途：一眼看出「索引比磁盘旧」或「这条 meta.json 进不来」。
	mux.HandleFunc("/doctor", func(w http.ResponseWriter, r *http.Request) {
		live, probs := scanCorpus(cfg)
		st.mu.RLock()
		cur := append([]Doc{}, st.index.Docs...)
		st.mu.RUnlock()
		liveKeys := map[string]corpusDoc{}
		for _, c := range live {
			liveKeys[keyOf(Doc{Name: c.Name, Path: c.Path, Kind: c.Kind})] = c
		}
		curKeys := map[string]Doc{}
		for _, d := range cur {
			curKeys[keyOf(d)] = d
		}
		issues := []map[string]any{}
		for k, d := range curKeys {
			if d.Kind == "manual" {
				continue
			}
			if _, ok := liveKeys[k]; !ok {
				issues = append(issues, map[string]any{"issue": "stale_in_index", "name": d.Name, "path": d.Path, "fix": "POST /reindex"})
			}
		}
		for k, c := range liveKeys {
			if _, ok := curKeys[k]; !ok {
				issues = append(issues, map[string]any{"issue": "missing_from_index", "name": c.Name, "path": c.Path, "fix": "POST /reindex"})
			}
			if strings.TrimSpace(c.Keywords) == "" {
				issues = append(issues, map[string]any{"issue": "no_keywords", "name": c.Name, "path": c.Path, "fix": "在 meta.json 里写 keywords（口语式查询靠它）"})
			}
		}
		same := map[string][]string{}
		for _, d := range cur {
			same[d.Name] = append(same[d.Name], d.Path)
		}
		dupes := []map[string]any{}
		for n, ps := range same {
			if len(ps) > 1 {
				dupes = append(dupes, map[string]any{"issue": "same_name", "name": n, "paths": ps, "fix": "两处都在索引里（不自动取舍）：自己决定删掉或合并哪个"})
			}
		}
		writeJSON(w, 200, map[string]any{
			"ok": true, "docs_index": len(cur), "docs_live": len(live),
			"same_name": sameNameCount(cur), "missing_keywords": missingKeywords(cur),
			"issues": issues, "same_name_groups": dupes, "meta_problems": probs,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeJSON(w, 404, map[string]any{"error": "not found", "endpoints": []string{"/search?q=&page=", "/doc", "/reindex", "/health", "/doctor"}})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "scout ok — /health /search?q= /doc /reindex /doctor（纯关键词，无向量）\n")
	})

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	log.Printf("[scout] listening on http://%s docs=%s", addr, mustJSON(map[string]any{"n": st.count()}))
	log.Fatal(http.ListenAndServe(addr, mux))
}

func sameNameCount(docs []Doc) int {
	m := map[string]int{}
	for _, d := range docs {
		m[d.Name]++
	}
	n := 0
	for _, c := range m {
		if c > 1 {
			n++
		}
	}
	return n
}

func missingKeywords(docs []Doc) int {
	n := 0
	for _, d := range docs {
		if d.Kind != "manual" && strings.TrimSpace(d.Keywords) == "" {
			n++
		}
	}
	return n
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
