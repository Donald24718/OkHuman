package types

import (
	"regexp"
	"strings"
)

// 压缩轮的标记前缀：生成方是 context（发压缩请求），解析方是 llm/fake（夹具据此走压缩
// 分支）。以前两边各写一份正则，改文案只改一处 → 夹具静默走普通路径，靠夹具的压缩测试
// 全部假过（2026-11-12 审查）。scope 不再写死白名单：以生成方为准。
const CompressMarkerPrefix = "[压缩:"

// CompressMarkerOf 生成压缩指令前缀。scope 先按解析方的字符集收敛：生成方写进别的字符
// （空格、中文、大写）会造出一个**解析不回来**的标记，压缩请求就静默走普通路径
// （2026-11-13 审计 #24）。
func CompressMarkerOf(scope string) string { return CompressMarkerPrefix + SafeScope(scope) + "]" }

// SafeScope 把任意 scope 收敛成 compressMarkerRe 认得的形状（空 → "custom"）。
func SafeScope(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "custom"
	}
	return string(out)
}

var compressMarkerRe = regexp.MustCompile(`^\s*\` + CompressMarkerPrefix + `([a-z0-9]+)\]`)

// CompressScopeOf 从一条消息文本里取压缩 scope；不是压缩请求 → ("", false)。
func CompressScopeOf(s string) (string, bool) {
	m := compressMarkerRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}
