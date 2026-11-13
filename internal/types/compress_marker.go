package types

import "regexp"

// 压缩轮的标记前缀：生成方是 context（发压缩请求），解析方是 llm/fake（夹具据此走压缩
// 分支）。以前两边各写一份正则，改文案只改一处 → 夹具静默走普通路径，靠夹具的压缩测试
// 全部假过（2026-11-12 审查）。scope 不再写死白名单：以生成方为准。
const CompressMarkerPrefix = "[压缩:"

// CompressMarkerOf 生成压缩指令前缀。
func CompressMarkerOf(scope string) string { return CompressMarkerPrefix + scope + "]" }

var compressMarkerRe = regexp.MustCompile(`^\s*\` + CompressMarkerPrefix + `([a-z0-9]+)\]`)

// CompressScopeOf 从一条消息文本里取压缩 scope；不是压缩请求 → ("", false)。
func CompressScopeOf(s string) (string, bool) {
	m := compressMarkerRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}
