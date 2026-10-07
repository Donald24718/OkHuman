package ipython

// 结果格式化：模型的唯一可见面。
//
// 格式原则：
//  1. 区分「用户代码出错」与「宿主超时中止」——混淆两者会让模型去修一段
//     根本没问题的代码；
//  2. 单字段也不静默丢弃（尤其 image/png 这类二进制 rich display）；
//  3. 超长结果就地截断并说明，别让一个巨型 repr 吃掉整轮上下文配额。

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 截断档位。
const (
	// maxResultRunes 单个结果的内联上限。超过会标明原始长度——模型据此决定
	// 是否改成分批打印，而不是对着截断内容硬猜。
	maxResultRunes = 20000

	// maxHTMLRunes text/html 的内联上限。HTML 里表格骨架占比极高，
	// 给模型的有效性远低于 text/plain，故配额小得多。
	maxHTMLRunes = 4000
)

// plainMIME 直接按纯文本内联的 MIME（优先级顺序即选择顺序）。
var plainMIME = []string{"text/plain", "text/markdown", "text/latex"}

// FormatResult 把一次执行整理成给模型的文本。timeout 是本次生效的超时。
func FormatResult(res ExecResult, timeout time.Duration) string {
	var b strings.Builder

	// 头部：耗时 + 状态。放在最前是因为模型的注意力集中在开头。
	b.WriteString(fmt.Sprintf("耗时 %.2fs", res.Elapsed.Seconds()))

	kv := KeyboardInterruptName
	isInterrupt := res.Interrupted || res.ErrName == kv
	switch {
	case isInterrupt:
		b.WriteString(fmt.Sprintf("（超过 %.0f 秒被中断，内核仍在、变量保留）", timeout.Seconds()))
	case res.Killed:
		b.WriteString(fmt.Sprintf("（超过 %.0f 秒且无法中断，已重启内核、变量已丢失）", timeout.Seconds()))
	case res.Lost:
		b.WriteString("（内核进程意外退出，已重启、变量已丢失）")
	}
	b.WriteString("\n")

	// 结果 / 错误：二者互斥（IPython 成功才有 result，失败才有 error）。
	if res.ErrName != "" && !isInterrupt {
		fmt.Fprintf(&b, "出错: %s: %s\n", res.ErrName, res.ErrValue)
	} else if len(res.Result) > 0 {
		text := pickResultText(res.Result)
		if text != "" {
			label := "结果"
			if res.Count != nil {
				label = fmt.Sprintf("Out[%d]", *res.Count)
			}
			b.WriteString(label + ":\n" + indent(text) + "\n")
		}
	}

	// display() 的输出：一个 cell 可能推多次，逐条标出。
	//
	// 与 result 分开列的理由：result 是"最后一个表达式的值"，display 是"代码
	// 主动推送的内容"，混在一起会让模型分不清哪个是返回值。
	for i, d := range res.Displays {
		text := pickResultText(d)
		if text == "" {
			continue // 内容已全部落盘（见下），此处再列一遍只是噪声
		}
		b.WriteString(fmt.Sprintf("display[%d]:\n%s\n", i+1, indent(text)))
	}

	// traceback 落在 stdout 里且带 ANSI 颜色码，喂给模型前必须剥掉。
	if s := strings.TrimRight(stripANSI(res.Stdout), "\n"); s != "" {
		b.WriteString("标准输出:\n" + indent(s) + "\n")
	}
	if s := strings.TrimRight(stripANSI(res.Stderr), "\n"); s != "" {
		b.WriteString("标准错误:\n" + indent(s) + "\n")
	}

	// 落盘产物：给绝对路径，模型可以再用 bash 读，或交给素材插件"看见"。
	if len(res.Saved) > 0 {
		b.WriteString("已落盘:\n")
		for _, a := range res.Saved {
			b.WriteString(fmt.Sprintf("  %s（%s，%s）\n", a.Path, a.MIME, humanBytes(a.Bytes)))
		}
	}

	// 尾部提示：只有真正需要模型改动作时才出现，否则是噪声。
	if note := resultNote(res.Result); note != "" {
		b.WriteString("\n" + note + "\n")
	}
	return b.String()
}

// ansiRe ANSI 转义序列（颜色 / 光标）。
//
// 为什么要剥：IPython 的交互式 traceback 默认带颜色，实测 stdout 里混着
// `\x1b[31m...\x1b[39m`，直接喂给模型是纯噪声（还占 token）。
// 只处理 CSI 序列（\x1b[ ... 字母），够覆盖 IPython / traceback 的全部着色。
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// stripANSI 去掉 ANSI 转义序列。没有 ESC 时原样返回（快路径，不跑正则）。
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiRe.ReplaceAllString(s, "")
}

// KeyboardInterruptName IPython 内部消化中断后留下的异常名。
//
// 单独抽常量：判定中断的两个分支（此处与 applyResponse）必须一致，
// 写成字面量容易在改格式时被悄悄改坏。
const KeyboardInterruptName = "KeyboardInterrupt"

// degenerateRepr 形如 `<IPython.core.display.HTML object>` 的默认占位 repr。
var degenerateRepr = regexp.MustCompile(`^<[^<>]*\bobject\b[^<>]*>$`)

// isDegenerateRepr 判断该纯文本是不是"只有类名和地址"的默认 repr。
//
// 实测踩到：display(HTML("<b>x</b>")) 同时给出 text/plain 与 text/html，而
// text/plain 是 `<IPython.core.display.HTML object>` —— 按"优先纯文本"的规矩
// 选它，模型就只看到这一行占位符，富内容等于没喂出去。
// 只在**另有 text/html** 时才让位，避免把 `<Foo object at 0x...>` 这种唯一
// 表示也给否掉。
func isDegenerateRepr(v string) bool {
	s := strings.TrimSpace(v)
	if strings.Contains(s, "\n") {
		return false // 多行的一定是真内容，不是占位 repr
	}
	return degenerateRepr.MatchString(s)
}

// pickResultText 从多 MIME 结果里挑出最适合内联给模型的纯文本。
func pickResultText(result map[string]string) string {
	_, hasHTML := result["text/html"]
	for _, mime := range plainMIME {
		if v, ok := result[mime]; ok && strings.TrimSpace(v) != "" {
			if hasHTML && isDegenerateRepr(v) {
				continue // 占位 repr 让位给 HTML
			}
			return truncate(stripANSI(v), maxResultRunes)
		}
	}
	// 没有纯文本备选时退而求其次：HTML 大概率是 DataFrame 的表格。
	if html, ok := result["text/html"]; ok && strings.TrimSpace(html) != "" {
		// 没有可用纯文本时才加这句前缀；若有纯文本被判为占位 repr 而让位，
		// 不加前缀——那时 HTML 就是正经内容，加前缀会让模型以为是降级品。
		if _, plain := result["text/plain"]; !plain {
			return "[富展示仅有 HTML] " + truncate(html, maxHTMLRunes)
		}
		return truncate(html, maxHTMLRunes)
	}
	return ""
}

// resultNote 生成补充提示：主要处理「有产出但没法内联」的情形。
func resultNote(result map[string]string) string {
	if len(result) == 0 {
		return ""
	}
	// 二进制 / 不可内联的输出不能悄悄丢掉，否则模型以为自己的绘图代码没生效。
	var others []string
	for mime := range result {
		if !isPlainMIME(mime) && mime != "text/html" {
			others = append(others, mime)
		}
	}
	if len(others) == 0 {
		return ""
	}
	sort.Strings(others)
	// 不再建议 savefig：图片类 MIME 已经自动落盘（见 ExecResult.Saved）。
	// 走到这里的都是连落盘规则都没覆盖的罕见 MIME，如实说明即可。
	return fmt.Sprintf("注意：本次还有既无法内联、也无法落盘的输出 [%s]。",
		strings.Join(others, ", "))
}

func isPlainMIME(mime string) bool {
	for _, m := range plainMIME {
		if m == mime {
			return true
		}
	}
	return false
}

// truncate 按 rune 截断（不能按字节：中文/emoji 会被切成半个码点）。
func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return fmt.Sprintf("%s\n...[结果截断：仅前 %d 字符，原共 %d 字符]", string(r[:maxRunes]), maxRunes, len(r))
}

// indent 给多行文本加两空格缩进，让「谁的输出」在视觉上归属于各自的标题。
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n")
}
