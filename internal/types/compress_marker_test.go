package types

import "testing"

// 生成方写进解析方不认的字符，会造出一个取不回 scope 的标记 → 压缩请求静默走普通路径。
func TestCompressMarkerScopeRoundTrip(t *testing.T) {
	for _, sc := range []string{"history", "Hist ory-x", "中文", "", "!!!"} {
		m := CompressMarkerOf(sc)
		got, ok := CompressScopeOf(m)
		if !ok {
			t.Errorf("scope=%q 生成的标记解析不回来：%q", sc, m)
			continue
		}
		if got != SafeScope(sc) || got == "" {
			t.Errorf("scope=%q  round-trip 不一致：got=%q want=%q", sc, got, SafeScope(sc))
		}
	}
	if SafeScope("") != "custom" || SafeScope("中文") != "custom" {
		t.Errorf("空/无可用字符应回落到 custom：%q %q", SafeScope(""), SafeScope("中文"))
	}
}
