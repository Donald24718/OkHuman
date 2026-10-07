package main

import "testing"

func TestHttpQueryEscapeUTF8(t *testing.T) {
	cases := []struct{ in, want string }{
		{"中文", "%E4%B8%AD%E6%96%87"},
		{"naïve", "na%C3%AFve"}, {"café", "caf%C3%A9"},
		{"emoji 🔥", "emoji%20%F0%9F%94%A5"},
		{"a b-c.d~e!f(g)", "a%20b-c.d~e!f(g)"},
		{"caf%C3%A9", "caf%25C3%25A9"}, // 已转义的 % 再转义（对齐 JS encodeURIComponent）
	}
	for _, c := range cases {
		if got := httpQueryEscape(c.in); got != c.want {
			t.Errorf("httpQueryEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
