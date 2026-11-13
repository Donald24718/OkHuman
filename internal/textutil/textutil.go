// Package textutil 字符数口径：全系统统一的 UTF-16 code units（对齐浏览器 JS
// text.length / WebSocket 前端的字符计数）。2026-11-12 四方审查前，同一口径在
// main/agent/context/server 各写了一份（共 7 处实现），改一处会漏三处，于是收在这里。
// 注意：这不是字节数也不是 rune 数——CJK 字节×3、代理对 rune×1 而 UTF-16 计 2。
// 上下文预算、截断、/status 的 chars 字段都必须走这里，混用会导致压缩触发点偏差。
package textutil

import "strings"

// UTF16Len 字符串的 UTF-16 code units 数。
func UTF16Len(s string) int {
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

// UTF16Slice 取前 n 个 UTF-16 code units（不切断代理对；n<=0 得空串）。
func UTF16Slice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i, r := range s {
		units := 1
		if r >= 0x10000 {
			units = 2
		}
		if count+units > n {
			return s[:i]
		}
		count += units
	}
	return s
}

// UTF16Truncate 截到 n 个 code units，返回结果与"是否真的截过"。
// 未超长时原样返回、第二个值为 false（调用方据此决定是否加省略标记）。
func UTF16Truncate(s string, n int) (string, bool) {
	if n <= 0 || UTF16Len(s) <= n {
		return s, false
	}
	return UTF16Slice(s, n), true
}

// UTF16Chunks 按 n 个 code units 切块（n<=0 视作 1）。用于模拟流式增量：块长要与客户端
// 按 UTF-16 看到的长度一致，且不会把代理对切两半。
func UTF16Chunks(s string, n int) []string {
	if n <= 0 {
		n = 1
	}
	var out []string
	cur := strings.Builder{}
	units := 0
	for _, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		cur.WriteRune(r)
		units += w
		if units >= n {
			out = append(out, cur.String())
			cur.Reset()
			units = 0
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
