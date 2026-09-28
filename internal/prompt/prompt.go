package prompt

// 可定制系统提示词（§4.7）：扫描提示词目录下所有 .md 文件，
// 按文件名自然排序后拼接全文，作为系统提示词。
// 目录不存在 / 无 .md → 回退内置默认提示词（fallback=true，服务不因此失败）。

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultSystemPrompt 内置默认提示词（prompts 目录缺失时的兜底）
var DefaultSystemPrompt = strings.Join([]string{
	"你是 OkHuman 的 agent。你只有一个元工具 bash：一切外部操作（执行命令、文件读写改、搜索、抓被截断的 log 全文）都靠它。",
	"规则：",
	"1. 需要外部信息或执行操作时用 bash，能用命令解决的不要凭空编造。",
	"2. 工具结果超长被截断时，截断处会给出完整结果落盘的文件路径，用 bash 的 cat / grep / tail 自己去看。",
	"3. 前台等待 30 秒未完成自动转后台（收到通知后继续干活，命令结果由下一次 run 搭车带回来）；单条命令最长活 600 秒，超时整个进程组被杀。",
	"4. 起长驻服务必须 setsid 脱离，否则会被 600 秒超时连带杀掉：(setsid <启动命令> < /dev/null > /tmp/<名>.log 2>&1 &)，之后 curl 探活、cat 读日志。",
	"5. 完成任务后直接给出结论；不要重复完全相同的工具调用。",
}, "\n")

type File struct {
	Name  string
	Chars int
}

type Loaded struct {
	Prompt   string
	Files    []File
	Fallback bool
}

// NaturalCompare 文件名自然排序："02-x.md" < "10-x.md"（按数字段比较，其余字典序）
func NaturalCompare(a, b string) int {
	pa := splitNat(a)
	pb := splitNat(b)
	n := len(pa)
	if len(pb) < n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		x, y := pa[i], pb[i]
		nx, okx := asInt(x)
		ny, oky := asInt(y)
		if okx && oky && nx != ny {
			if nx < ny {
				return -1
			}
			return 1
		}
		if okx && !oky {
			return -1
		}
		if !okx && oky {
			return 1
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	if len(pa) != len(pb) {
		if len(pa) < len(pb) {
			return -1
		}
		return 1
	}
	return 0
}

func asInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n, true
}

// splitNat 按"数字段/非数字段"切分（对应 TS 的 split(/(\d+)(\D*)/g)）
func splitNat(s string) []string {
	var parts []string
	i := 0
	for i < len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			parts = append(parts, s[i:j])
			i = j
		} else {
			j := i
			for j < len(s) && (s[j] < '0' || s[j] > '9') {
				j++
			}
			parts = append(parts, s[i:j])
			i = j
		}
	}
	return parts
}

// LoadPromptDir 从任意目录加载系统提示词（*.md 自然排序拼接）
func LoadPromptDir(dir string) *Loaded {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return &Loaded{Prompt: DefaultSystemPrompt, Fallback: true}
	}
	var mdFiles []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			mdFiles = append(mdFiles, e.Name())
		}
	}
	sort.SliceStable(mdFiles, func(i, j int) bool {
		return NaturalCompare(mdFiles[i], mdFiles[j]) < 0
	})
	if len(mdFiles) == 0 {
		return &Loaded{Prompt: DefaultSystemPrompt, Fallback: true}
	}
	files := make([]File, 0, len(mdFiles))
	chunks := make([]string, 0, len(mdFiles))
	for _, name := range mdFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		text := string(data)
		files = append(files, File{Name: name, Chars: len([]rune(text))})
		chunks = append(chunks, text)
	}
	return &Loaded{Prompt: strings.Join(chunks, "\n\n"), Files: files, Fallback: false}
}
