// okknowledge — 关系性知识库 v0（补 LLM 缺少的专业知识）
// 规范见 docs/KB-v0-表示规范.md。三条不变式：
//   1) ID 永不改指（改语义 = 新 ID + supersedes）
//   2) 每条记录必须有出处（master|import|verified|proposed）
//   3) 硬约束优先，且 proposed 出处不得参与硬约束
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Prov struct {
	Source string `json:"source"`
	Note   string `json:"note,omitempty"`
	TS     int64  `json:"ts"`
}

type Slot struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Ref  uint64 `json:"ref,omitempty"`
}

type Constraint struct {
	Hard bool   `json:"hard"`
	Expr string `json:"expr"`
}

type Node struct {
	ID          uint64       `json:"id"`
	Kind        string       `json:"kind"`
	Label       string       `json:"label,omitempty"`
	Version     uint32       `json:"version"`
	Supersedes  uint64       `json:"supersedes,omitempty"`
	Body        []Slot       `json:"body,omitempty"`
	Constraints []Constraint `json:"constraints,omitempty"`
	Prov        Prov         `json:"prov"`
	Reuse       uint64       `json:"reuse"`
	LastUsed    int64        `json:"last_used,omitempty"`
	Canon       string       `json:"canon,omitempty"`
	Dead        bool         `json:"dead,omitempty"`
}

type Edge struct {
	Src    uint64  `json:"src"`
	Dst    uint64  `json:"dst"`
	Role   string  `json:"role"`
	Weight float64 `json:"weight"`
	Prov   Prov    `json:"prov"`
}

type KB struct {
	dir   string
	nodes map[uint64]*Node
	edges []Edge
	next  uint64
}

// 配置自管：插件目录内的 config.json（不进 OkHuman 的 config/）
type Config struct {
	DataDir   string `json:"data_dir"`
	BudgetMiB int64  `json:"budget_mib"`
}

func exeDir() string {
	exe, _ := os.Executable()
	return filepath.Dir(exe)
}

func loadConfig() Config {
	c := Config{BudgetMiB: 64}
	b, err := os.ReadFile(filepath.Join(exeDir(), "config.json"))
	if err != nil {
		return c
	}
	json.Unmarshal(b, &c)
	return c
}

func dataDir() string {
	if v := os.Getenv("OKKN_DATA"); v != "" {
		return v
	}
	if c := loadConfig(); c.DataDir != "" {
		return c.DataDir
	}
	return filepath.Join(exeDir(), "data")
}

func open() *KB {
	d := dataDir()
	os.MkdirAll(d, 0o755)
	kb := &KB{dir: d, nodes: map[uint64]*Node{}, next: 1}
	if f, err := os.Open(filepath.Join(d, "nodes.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l == "" {
				continue
			}
			var n Node
			if json.Unmarshal([]byte(l), &n) == nil {
				kb.nodes[n.ID] = &n
				if n.ID >= kb.next {
					kb.next = n.ID + 1
				}
			}
		}
		f.Close()
	}
	if f, err := os.Open(filepath.Join(d, "edges.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l == "" {
				continue
			}
			var e Edge
			if json.Unmarshal([]byte(l), &e) == nil {
				kb.edges = append(kb.edges, e)
			}
		}
		f.Close()
	}
	return kb
}

func (k *KB) save() error {
	tmp := filepath.Join(k.dir, "nodes.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	ids := make([]uint64, 0, len(k.nodes))
	for id := range k.nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		b, _ := json.Marshal(k.nodes[id])
		w.Write(b)
		w.WriteByte('\n')
	}
	w.Flush()
	f.Close()
	if err := os.Rename(tmp, filepath.Join(k.dir, "nodes.jsonl")); err != nil {
		return err
	}
	tmp = filepath.Join(k.dir, "edges.tmp")
	f, err = os.Create(tmp)
	if err != nil {
		return err
	}
	w = bufio.NewWriter(f)
	for _, e := range k.edges {
		b, _ := json.Marshal(e)
		w.Write(b)
		w.WriteByte('\n')
	}
	w.Flush()
	f.Close()
	return os.Rename(tmp, filepath.Join(k.dir, "edges.jsonl"))
}

func (k *KB) audit(format string, a ...interface{}) {
	f, err := os.OpenFile(filepath.Join(k.dir, "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%d %s\n", time.Now().Unix(), fmt.Sprintf(format, a...))
}

func validSource(s string) bool {
	switch s {
	case "master", "import", "verified", "proposed":
		return true
	}
	return false
}

// canon: 规范形哈希。kind + 排序后的 (槽名: 子canon|子kind)。深度上限防爆炸。
func (k *KB) canon(n *Node, depth int) string {
	if depth > 8 {
		return n.Kind
	}
	parts := make([]string, 0, len(n.Body))
	for _, s := range n.Body {
		sub := s.Kind
		if s.Ref != 0 {
			if c := k.nodes[s.Ref]; c != nil {
				sub = k.canon(c, depth+1)
			}
		}
		parts = append(parts, s.Name+"="+sub)
	}
	sort.Strings(parts)
	// 原子（无 body）必须带 label 与约束参与哈希，否则同类原子全部互相"同构"，
	// dedup 会把三条不同的决定提议合并 —— 那是假阳性。
	cs := make([]string, 0, len(n.Constraints))
	for _, c := range n.Constraints {
		t := "soft:"
		if c.Hard {
			t = "hard:"
		}
		cs = append(cs, t+c.Expr)
	}
	sort.Strings(cs)
	h := sha256.Sum256([]byte(n.Kind + "|" + n.Label + "|" + strings.Join(cs, "|") + "|" + strings.Join(parts, "|")))
	return hex.EncodeToString(h[:8])
}

// body 引用必须无环（展开处禁止环）；edge 图允许环。
func (k *KB) bodyAcyclic(ref uint64) bool {
	seen := map[uint64]bool{}
	var walk func(uint64) bool
	walk = func(id uint64) bool {
		if seen[id] {
			return false
		}
		seen[id] = true
		n := k.nodes[id]
		if n == nil {
			return true
		}
		for _, s := range n.Body {
			if s.Ref != 0 && !walk(s.Ref) {
				return false
			}
		}
		return true
	}
	return walk(ref)
}

func parseSlots(spec string) []Slot {
	var out []Slot
	for _, p := range strings.Split(spec, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		f := strings.Split(p, ":")
		if len(f) < 2 {
			continue
		}
		s := Slot{Name: f[0], Kind: f[1]}
		if len(f) >= 3 {
			if v, err := strconv.ParseUint(f[2], 10, 64); err == nil {
				s.Ref = v
			}
		}
		out = append(out, s)
	}
	return out
}

func parseConstraints(spec string) []Constraint {
	var out []Constraint
	for _, p := range strings.Split(spec, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		hard := false
		if strings.HasPrefix(p, "hard:") {
			hard, p = true, strings.TrimSpace(p[5:])
		} else if strings.HasPrefix(p, "soft:") {
			p = strings.TrimSpace(p[5:])
		}
		out = append(out, Constraint{Hard: hard, Expr: p})
	}
	return out
}

func (k *KB) cmdAdd(fs *flag.FlagSet) int {
	kind := fs.String("kind", "", "类型（如 实体.硬件.GPU / 关系.事件）")
	label := fs.String("label", "", "可读名（仅显示用，不是 ID）")
	prov := fs.String("prov", "", "出处：master|import|verified|proposed（必填）")
	note := fs.String("note", "", "出处说明/证据")
	body := fs.String("body", "", "槽位 name:kind[:refID];...")
	cons := fs.String("constraint", "", "约束 hard:表达式 / soft:表达式;...")
	sup := fs.Uint64("supersedes", 0, "取代的旧 ID")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	if *kind == "" || !validSource(*prov) {
		fmt.Fprintln(os.Stderr, "错误：-kind 必填，-prov 必须是 master|import|verified|proposed（无出处 = 条目不存在）")
		return 1
	}
	kb := open()
	cs := parseConstraints(*cons)
	for _, c := range cs {
		if c.Hard && *prov == "proposed" {
			fmt.Fprintln(os.Stderr, "拒绝：proposed 出处不得参与硬约束（不变式 3）")
			return 1
		}
	}
	n := &Node{ID: kb.next, Kind: *kind, Label: *label, Version: 1,
		Supersedes: *sup, Body: parseSlots(*body), Constraints: cs,
		Prov: Prov{Source: *prov, Note: *note, TS: time.Now().Unix()}}
	for _, s := range n.Body {
		if s.Ref != 0 {
			if kb.nodes[s.Ref] == nil {
				fmt.Fprintf(os.Stderr, "拒绝：槽位引用不存在的 ID %d（禁止编造实体 ID）\n", s.Ref)
				return 1
			}
			if !kb.bodyAcyclic(s.Ref) {
				fmt.Fprintf(os.Stderr, "拒绝：body 引用成环（展开处禁止环）\n")
				return 1
			}
		}
	}
	n.Canon = kb.canon(n, 0)
	if *sup != 0 {
		if old := kb.nodes[*sup]; old == nil {
			fmt.Fprintf(os.Stderr, "拒绝：supersedes 指向不存在的 ID %d\n", *sup)
			return 1
		} else {
			old.Dead = true
			n.Version = old.Version + 1
		}
	}
	kb.nodes[n.ID] = n
	if err := kb.save(); err != nil {
		fmt.Fprintln(os.Stderr, "写盘失败:", err)
		return 1
	}
	kb.audit("add id=%d kind=%s label=%q prov=%s canon=%s", n.ID, n.Kind, n.Label, n.Prov.Source, n.Canon)
	fmt.Printf("%d\n", n.ID)
	return 0
}

func (k *KB) cmdRevise(fs *flag.FlagSet) int {
	id := fs.Uint64("id", 0, "被修订的 ID（必填；旧 ID 不改动语义，只标记被取代）")
	kind := fs.String("kind", "", "新类型（空=沿用）")
	label := fs.String("label", "", "新可读名")
	prov := fs.String("prov", "", "新出处（必填）")
	note := fs.String("note", "", "改了什么、为什么")
	body := fs.String("body", "", "新槽位")
	cons := fs.String("constraint", "", "新约束")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	if *id == 0 || !validSource(*prov) {
		fmt.Fprintln(os.Stderr, "错误：-id 与合法 -prov 必填")
		return 1
	}
	kb := open()
	old := kb.nodes[*id]
	if old == nil {
		fmt.Fprintf(os.Stderr, "不存在 ID %d\n", *id)
		return 1
	}
	fs2 := flag.NewFlagSet("add", flag.ContinueOnError)
	_ = fs2
	n := &Node{ID: kb.next, Kind: old.Kind, Label: old.Label, Body: old.Body,
		Constraints: old.Constraints, Supersedes: old.ID, Version: old.Version + 1,
		Prov: Prov{Source: *prov, Note: *note, TS: time.Now().Unix()}}
	if *kind != "" {
		n.Kind = *kind
	}
	if *label != "" {
		n.Label = *label
	}
	if *body != "" {
		n.Body = parseSlots(*body)
	}
	if *cons != "" {
		n.Constraints = parseConstraints(*cons)
	}
	for _, c := range n.Constraints {
		if c.Hard && n.Prov.Source == "proposed" {
			fmt.Fprintln(os.Stderr, "拒绝：proposed 出处不得参与硬约束")
			return 1
		}
	}
	for _, s := range n.Body {
		if s.Ref != 0 && (kb.nodes[s.Ref] == nil || !kb.bodyAcyclic(s.Ref)) {
			fmt.Fprintf(os.Stderr, "拒绝：槽位引用无效或成环（%d）\n", s.Ref)
			return 1
		}
	}
	n.Canon = kb.canon(n, 0)
	old.Dead = true
	kb.nodes[n.ID] = n
	if err := kb.save(); err != nil {
		fmt.Fprintln(os.Stderr, "写盘失败:", err)
		return 1
	}
	kb.audit("revise old=%d -> new=%d prov=%s", old.ID, n.ID, n.Prov.Source)
	fmt.Printf("%d\n", n.ID)
	return 0
}

func (k *KB) cmdLink(fs *flag.FlagSet) int {
	src := fs.Uint64("src", 0, "源 ID")
	dst := fs.Uint64("dst", 0, "目标 ID")
	role := fs.String("role", "", "角色（agent/patient/is_a/...）")
	prov := fs.String("prov", "", "出处（必填）")
	note := fs.String("note", "", "说明")
	w := fs.Float64("weight", 1, "权重")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	if *src == 0 || *dst == 0 || *role == "" || !validSource(*prov) {
		fmt.Fprintln(os.Stderr, "错误：-src -dst -role 与合法 -prov 必填")
		return 1
	}
	kb := open()
	if kb.nodes[*src] == nil || kb.nodes[*dst] == nil {
		fmt.Fprintln(os.Stderr, "拒绝：端点不存在（禁止编造实体 ID）")
		return 1
	}
	kb.edges = append(kb.edges, Edge{Src: *src, Dst: *dst, Role: *role, Weight: *w,
		Prov: Prov{Source: *prov, Note: *note, TS: time.Now().Unix()}})
	if err := kb.save(); err != nil {
		fmt.Fprintln(os.Stderr, "写盘失败:", err)
		return 1
	}
	kb.audit("link %d -%s-> %d prov=%s", *src, *role, *dst, *prov)
	fmt.Println("ok")
	return 0
}

func touch(kb *KB, id uint64) {
	if n := kb.nodes[id]; n != nil {
		n.Reuse++
		n.LastUsed = time.Now().Unix()
	}
}

func (k *KB) cmdGet(fs *flag.FlagSet) int {
	id := fs.Uint64("id", 0, "ID")
	exp := fs.Int("expand", 0, "展开预算（节点数，0=只看名字层）")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	if *id == 0 {
		fmt.Fprintln(os.Stderr, "错误：-id 必填")
		return 1
	}
	kb := open()
	n := kb.nodes[*id]
	if n == nil {
		fmt.Fprintf(os.Stderr, "不存在 ID %d\n", *id)
		return 1
	}
	touch(kb, *id)
	kb.printNode(n, 0)
	if *exp > 0 {
		budget := *exp
		seen := map[uint64]bool{*id: true}
		kb.expand(*id, &budget, seen, 1)
	}
	kb.save()
	return 0
}

func (k *KB) printNode(n *Node, ind int) {
	pad := strings.Repeat("  ", ind)
	dead := ""
	if n.Dead {
		dead = " [已取代]"
	}
	fmt.Printf("%s#%d %s %q%s (v%d, prov=%s, reuse=%d, canon=%s)\n",
		pad, n.ID, n.Kind, n.Label, dead, n.Version, n.Prov.Source, n.Reuse, n.Canon)
	for _, c := range n.Constraints {
		t := "soft"
		if c.Hard {
			t = "HARD"
		}
		fmt.Printf("%s  @%s %s\n", pad, t, c.Expr)
	}
}

func (k *KB) expand(id uint64, budget *int, seen map[uint64]bool, ind int) {
	if *budget <= 0 {
		if ind == 1 {
			fmt.Println("  … (展开预算耗尽)")
		}
		return
	}
	n := k.nodes[id]
	if n == nil {
		return
	}
	for _, s := range n.Body {
		if s.Ref == 0 {
			fmt.Printf("%s  .%s:%s (空槽)\n", strings.Repeat("  ", ind), s.Name, s.Kind)
			continue
		}
		*budget--
		c := k.nodes[s.Ref]
		if c == nil {
			fmt.Printf("%s  .%s -> #%d (缺失!)\n", strings.Repeat("  ", ind), s.Name, s.Ref)
			continue
		}
		if seen[s.Ref] {
			fmt.Printf("%s  .%s -> #%d %s (环，停在名字)\n", strings.Repeat("  ", ind), s.Name, c.ID, c.Kind)
			continue
		}
		seen[s.Ref] = true
		fmt.Printf("%s  .%s\n", strings.Repeat("  ", ind), s.Name)
		k.printNode(c, ind)
		k.expand(s.Ref, budget, seen, ind+1)
	}
	for _, e := range k.edges {
		if e.Src != id {
			continue
		}
		*budget--
		c := k.nodes[e.Dst]
		if c == nil {
			continue
		}
		if seen[e.Dst] {
			fmt.Printf("%s  -%s-> #%d (环，停在名字)\n", strings.Repeat("  ", ind), e.Role, c.ID)
			continue
		}
		seen[e.Dst] = true
		fmt.Printf("%s  -%s->\n", strings.Repeat("  ", ind), e.Role)
		k.printNode(c, ind+1)
		k.expand(e.Dst, budget, seen, ind+1)
	}
}

func (k *KB) cmdNeighbors(fs *flag.FlagSet) int {
	id := fs.Uint64("id", 0, "ID")
	role := fs.String("role", "", "只看该角色")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	kb := open()
	if kb.nodes[*id] == nil {
		fmt.Fprintf(os.Stderr, "不存在 ID %d\n", *id)
		return 1
	}
	touch(kb, *id)
	for _, e := range kb.edges {
		if e.Src != *id && e.Dst != *id {
			continue
		}
		if *role != "" && e.Role != *role {
			continue
		}
		dir := "->"
		other := e.Dst
		if e.Src != *id {
			dir = "<-"
			other = e.Src
		}
		n := kb.nodes[other]
		lb := ""
		if n != nil {
			lb = n.Kind + " " + fmt.Sprintf("%q", n.Label)
		}
		fmt.Printf("#%d %s %s #%d %s (w=%.2f, prov=%s)\n", *id, dir, e.Role, other, lb, e.Weight, e.Prov.Source)
	}
	kb.save()
	return 0
}

func (k *KB) cmdIsa(fs *flag.FlagSet) int {
	id := fs.Uint64("id", 0, "ID")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	kb := open()
	if kb.nodes[*id] == nil {
		fmt.Fprintf(os.Stderr, "不存在 ID %d\n", *id)
		return 1
	}
	// 可读路径是计算视图：沿 is_a 边走出来，不烧进 ID
	cur := *id
	seen := map[uint64]bool{}
	var path []string
	for {
		n := kb.nodes[cur]
		if n == nil {
			break
		}
		path = append(path, n.Label)
		seen[cur] = true
		next := uint64(0)
		for _, e := range kb.edges {
			if e.Role == "is_a" && e.Src == cur && !seen[e.Dst] {
				next = e.Dst
				break
			}
		}
		if next == 0 {
			break
		}
		cur = next
	}
	fmt.Println(strings.Join(path, " / "))
	return 0
}

func (k *KB) cmdFind(fs *flag.FlagSet) int {
	kind := fs.String("kind", "", "类型前缀")
	text := fs.String("text", "", "label/note/约束 子串")
	prov := fs.String("prov", "", "出处")
	alive := fs.Bool("alive", true, "隐藏已被取代的")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	kb := open()
	ids := make([]uint64, 0)
	for id, n := range kb.nodes {
		if *alive && n.Dead {
			continue
		}
		if *kind != "" && !strings.HasPrefix(n.Kind, *kind) {
			continue
		}
		if *prov != "" && n.Prov.Source != *prov {
			continue
		}
		if *text != "" {
			blob := n.Label + " " + n.Prov.Note + " " + n.Kind
			for _, c := range n.Constraints {
				blob += " " + c.Expr
			}
			if !strings.Contains(strings.ToLower(blob), strings.ToLower(*text)) {
				continue
			}
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		kb.printNode(kb.nodes[id], 0)
	}
	fmt.Printf("(命中 %d)\n", len(ids))
	return 0
}

func (k *KB) cmdDedup(fs *flag.FlagSet) int {
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	kb := open()
	by := map[string][]uint64{}
	for id, n := range kb.nodes {
		if n.Dead {
			continue
		}
		by[n.Canon] = append(by[n.Canon], id)
	}
	n := 0
	for c, ids := range by {
		if len(ids) < 2 {
			continue
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		fmt.Printf("同构候选 canon=%s: %v —— 合并提议（不自动合并）\n", c, ids)
		n++
	}
	fmt.Printf("(同构组 %d)\n", n)
	return 0
}

func (k *KB) cmdStats(fs *flag.FlagSet) int {
	budget := fs.Int64("budget-mib", 0, "活跃集字节预算（MiB），0=只报告")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	kb := open()
	var nb, eb int64
	now := time.Now().Unix()
	type val struct {
		id uint64
		v  float64
		sz int64
	}
	var vs []val
	for _, n := range kb.nodes {
		b, _ := json.Marshal(n)
		nb += int64(len(b))
		vs = append(vs, val{n.ID, 0, int64(len(b))})
	}
	for _, e := range kb.edges {
		b, _ := json.Marshal(e)
		eb += int64(len(b))
	}
	fmt.Printf("节点 %d（活跃 %d） 边 %d\n", len(kb.nodes), func() int {
		c := 0
		for _, n := range kb.nodes {
			if !n.Dead {
				c++
			}
		}
		return c
	}(), len(kb.edges))
	fmt.Printf("体积 节点 %.2f MiB + 边 %.2f MiB = %.2f MiB\n", float64(nb)/1048576, float64(eb)/1048576, float64(nb+eb)/1048576)
	prov := map[string]int{}
	for _, n := range kb.nodes {
		prov[n.Prov.Source]++
	}
	for _, s := range []string{"master", "import", "verified", "proposed"} {
		fmt.Printf("  prov.%-9s %d\n", s, prov[s])
	}
	if *budget == 0 {
		*budget = loadConfig().BudgetMiB
	}
	if *budget > 0 {
		limit := *budget * 1048576
		if nb+eb > limit {
			fmt.Printf("超预算！%d > %d —— 加一条必须删或合并一条\n", nb+eb, limit)
			for i := range vs {
				n := kb.nodes[vs[i].id]
				age := now - n.LastUsed
				if age < 1 {
					age = 1
				}
				rec := 86400.0 * 30 / float64(age)
				vs[i].v = float64(n.Reuse+1) * rec / float64(vs[i].sz)
			}
			sort.Slice(vs, func(i, j int) bool { return vs[i].v < vs[j].v })
			fmt.Println("价值最低（候选降级/删除）：")
			for i := 0; i < len(vs) && i < 10; i++ {
				n := kb.nodes[vs[i].id]
				fmt.Printf("  #%-6d v=%.3e size=%d reuse=%d %s %q\n", n.ID, vs[i].v, vs[i].sz, n.Reuse, n.Kind, n.Label)
			}
		} else {
			fmt.Printf("预算内（余量 %d MiB）\n", (limit-nb-eb)/1048576)
		}
	}
	return 0
}

func hardFromProposed(cs []Constraint, src string) string {
	if src != "proposed" {
		return ""
	}
	for _, c := range cs {
		if c.Hard {
			return "proposed 出处不得参与硬约束"
		}
	}
	return ""
}

func (k *KB) cmdLoad(fs *flag.FlagSet) int {
	f := fs.String("f", "", "JSONL 文件：每行 {\"node\":{...}} 或 {\"edge\":{...}}")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}
	if *f == "" {
		fmt.Fprintln(os.Stderr, "错误：-f 必填")
		return 1
	}
	fh, err := os.Open(*f)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer fh.Close()
	kb := open()
	// 两遍：先建节点（拿到 ID），再建边
	var nodes []*Node
	var edges []Edge
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		var rec struct {
			Node *Node  `json:"node"`
			Edge *Edge  `json:"edge"`
		}
		if json.Unmarshal([]byte(l), &rec) != nil {
			continue
		}
		if rec.Node != nil {
			nodes = append(nodes, rec.Node)
		}
		if rec.Edge != nil {
			edges = append(edges, *rec.Edge)
		}
	}
	made := 0
	for _, n := range nodes {
		if !validSource(n.Prov.Source) {
			fmt.Fprintf(os.Stderr, "跳过：无合法出处 kind=%s label=%q\n", n.Kind, n.Label)
			continue
		}
		if bad := hardFromProposed(n.Constraints, n.Prov.Source); bad != "" {
			fmt.Fprintf(os.Stderr, "跳过：%s label=%q\n", bad, n.Label)
			continue
		}
		nn := *n
		nn.ID = kb.next
		nn.Version = 1
		kb.nodes[nn.ID] = &nn
		kb.next++
		made++
		kb.audit("load id=%d kind=%s label=%q prov=%s", nn.ID, nn.Kind, nn.Label, nn.Prov.Source)
	}
	le := 0
	for _, e := range edges {
		if !validSource(e.Prov.Source) || kb.nodes[e.Src] == nil || kb.nodes[e.Dst] == nil {
			continue
		}
		kb.edges = append(kb.edges, e)
		le++
	}
	for _, n := range kb.nodes {
		n.Canon = kb.canon(n, 0)
	}
	if err := kb.save(); err != nil {
		fmt.Fprintln(os.Stderr, "写盘失败:", err)
		return 1
	}
	fmt.Printf("载入 节点 %d 边 %d\n", made, le)
	return 0
}

func usage() int {
	fmt.Println(`okknowledge — 关系性知识库 v0
用法: okknowledge <命令> [参数]
  add       -kind K -prov P [-label L] [-note N] [-body name:kind[:ref];...] [-constraint hard:..;soft:..] [-supersedes ID]
  revise    -id ID -prov P [-kind K] [-label L] [-body ..] [-constraint ..] [-note ..]      # 旧 ID 只标记被取代
  link      -src ID -dst ID -role R -prov P [-weight W] [-note N]
  get       -id ID [-expand N]          # N=展开预算（节点数）；默认只看名字层
  neighbors -id ID [-role R]
  isa       -id ID                      # 可读路径（计算视图，不烧进 ID）
  find      [-kind 前缀] [-text 子串] [-prov P] [-alive=false]
  dedup                                 # 规范形同构 → 合并提议（不自动合并）
  stats     [-budget-mib N]             # 体积/出处分布/复用；超预算则列降级候选
  load      -f FILE                     # 批量导入（JSONL: {"node":{...}} / {"edge":{...}}）
数据目录: $OKKN_DATA 或 <二进制目录>/data`)
	return 2
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(usage())
	}
	kb := &KB{}
	switch os.Args[1] {
	case "add":
		fs := flag.NewFlagSet("add", flag.ExitOnError)
		os.Exit(kb.cmdAdd(fs))
	case "revise":
		fs := flag.NewFlagSet("revise", flag.ExitOnError)
		os.Exit(kb.cmdRevise(fs))
	case "link":
		fs := flag.NewFlagSet("link", flag.ExitOnError)
		os.Exit(kb.cmdLink(fs))
	case "get":
		fs := flag.NewFlagSet("get", flag.ExitOnError)
		os.Exit(kb.cmdGet(fs))
	case "neighbors":
		fs := flag.NewFlagSet("neighbors", flag.ExitOnError)
		os.Exit(kb.cmdNeighbors(fs))
	case "isa":
		fs := flag.NewFlagSet("isa", flag.ExitOnError)
		os.Exit(kb.cmdIsa(fs))
	case "find":
		fs := flag.NewFlagSet("find", flag.ExitOnError)
		os.Exit(kb.cmdFind(fs))
	case "dedup":
		fs := flag.NewFlagSet("dedup", flag.ExitOnError)
		os.Exit(kb.cmdDedup(fs))
	case "stats":
		fs := flag.NewFlagSet("stats", flag.ExitOnError)
		os.Exit(kb.cmdStats(fs))
	case "load":
		fs := flag.NewFlagSet("load", flag.ExitOnError)
		os.Exit(kb.cmdLoad(fs))
	default:
		os.Exit(usage())
	}
}
