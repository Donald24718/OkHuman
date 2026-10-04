package background

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCapBytes_UTF8Boundary 回归：capBytes 必须在任意截断点都返回合法 UTF-8。
//
// 背景（2026-10-04 实证）：旧实现
//
//	cut := s[:n]
//	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
//	    cut = cut[:len(cut)-1]
//	}
//
// 判断方向错误——检查"最后一个字节是否 rune 起始"，剥掉一字节后新的结尾
// 可能仍是续字节。实测 capBytes("中文abc", 1..6) 全部返回非法 UTF-8。
// 正确做法：从边界 n 往回退，直到 s[n] 是 rune 起始字节（或 n==0）。
func TestCapBytes_UTF8Boundary(t *testing.T) {
	cases := []struct {
		name string
		s    string
	}{
		{"纯ASCII", "abcdefgh"},
		{"纯中文", "中文测试内容"},
		{"中英混合", "中文abc测试xyz"},
		{"emoji", "🌿🌱🌳🌲"},
		{"中英emoji混合", "a中🌿b文🌱c"},
		{"CJK扩展B", "𠀀𠀁𠀂"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 扫描所有可能的截断点 n（含 0 与 len）
			for n := 0; n <= len(tc.s); n++ {
				got := capBytes(tc.s, n)
				if !utf8.ValidString(got) {
					t.Errorf("capBytes(%.20q, %d) 返回非法 UTF-8: % x（应为合法 UTF-8 前缀）",
						tc.s, n, []byte(got))
					continue
				}
				// 必须是不超过 n 字节的前缀
				if len(got) > n {
					t.Errorf("capBytes(%.20q, %d) 返回长度 %d > n（越界）", tc.s, n, len(got))
				}
				// 必须是原串前缀
				if !strings.HasPrefix(tc.s, got) {
					t.Errorf("capBytes(%.20q, %d) 不是原串前缀: %q", tc.s, n, got)
				}
				// 应尽量贴近 n（回退不超过 3 字节，因 UTF-8 最长 4 字节）
				if n-len(got) >= 4 {
					t.Errorf("capBytes(%.20q, %d) 回退过度：返回 %d 字节", tc.s, n, len(got))
				}
			}
		})
	}
}

// TestCapBytes_ExactBoundaryValues 精确值断言（回归锁定已知错误输出）。
func TestCapBytes_ExactBoundaryValues(t *testing.T) {
	s := "中文abc" // 中(3)文(3)a(1)b(1)c(1) = 9 字节
	want := map[int]string{
		0: "",
		1: "",
		2: "",
		3: "中",
		4: "中",
		5: "中",
		6: "中文",
		7: "中文a",
		8: "中文ab",
		9: "中文abc",
	}
	for n, w := range want {
		if got := capBytes(s, n); got != w {
			t.Errorf("capBytes(%q, %d) = %q，期望 %q", s, n, got, w)
		}
	}
}

// TestCapBytes_LimitCap 封顶上限正确性（256KB 场景）。
func TestCapBytes_LimitCap(t *testing.T) {
	t.Logf("maxStoredResultBytes = %d", maxStoredResultBytes)
	// ASCII：精确截到上限
	ascii := strings.Repeat("x", maxStoredResultBytes+1000)
	if got := capBytes(ascii, maxStoredResultBytes); len(got) != maxStoredResultBytes {
		t.Errorf("ASCII 应截到 %d，实际 %d", maxStoredResultBytes, len(got))
	}
	// 中文：不超过上限且合法
	cn := strings.Repeat("中", maxStoredResultBytes/3+100)
	got := capBytes(cn, maxStoredResultBytes)
	if len(got) > maxStoredResultBytes {
		t.Errorf("中文截断越界: %d > %d", len(got), maxStoredResultBytes)
	}
	if !utf8.ValidString(got) {
		t.Errorf("中文截断产生非法 UTF-8")
	}
}

// TestCapBytes_NoTruncation 未超限时原样返回。
func TestCapBytes_NoTruncation(t *testing.T) {
	for _, s := range []string{"", "a", "中文", strings.Repeat("中", 10)} {
		if got := capBytes(s, len(s)); got != s {
			t.Errorf("len(s) 时不应截断: got=%q want=%q", got, s)
		}
		if got := capBytes(s, len(s)+100); got != s {
			t.Errorf("限额大于长度时不应截断: got=%q want=%q", got, s)
		}
	}
}
