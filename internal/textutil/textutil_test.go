package textutil

import "testing"

// 口径本身：UTF-16 code units ≠ 字节数 ≠ rune 数。三方审查都指出混用会让上下文预算在
// 中文下差 3 倍（汉字：字节 3、rune 1、UTF-16 1），所以这条不变式必须有测试钉住。
func TestUTF16Counting(t *testing.T) {
	for _, c := range []struct {
		s    string
		want int
	}{
		{"", 0}, {"abc", 3}, {"你好", 2}, {"你好世界", 4},
		{"\U0001F600", 2}, // emoji：rune 计 1，UTF-16 计 2（代理对）
		{"a\U0001F600b", 4},
	} {
		if got := UTF16Len(c.s); got != c.want {
			t.Errorf("UTF16Len(%q) = %d，期望 %d", c.s, got, c.want)
		}
	}
}

// 截断不得切断代理对（切一半会留下孤立代理项，前端渲染成坏字符）。
// n=4 正好容纳 a(1)+emoji(2)+b(1)，所以不截；n=2/3 只能到 "a"。
func TestUTF16SliceKeepsSurrogatePairs(t *testing.T) {
	s := "a\U0001F600b"
	cases := []struct {
		n    int
		want string
	}{
		{-1, ""}, {0, ""}, {1, "a"}, {2, "a"}, {3, "a\U0001F600"}, {4, s}, {5, s}, {9, s},
	}
	for _, c := range cases {
		if got := UTF16Slice(s, c.n); got != c.want {
			t.Errorf("UTF16Slice(%q,%d) = %q，期望 %q", s, c.n, got, c.want)
		}
	}
}

// Truncate 的第二个返回值只在真截过时为 true（调用方据此加省略标记）。
func TestUTF16TruncateFlag(t *testing.T) {
	if got, cut := UTF16Truncate("你好", 5); got != "你好" || cut {
		t.Errorf("未超长不该截：%q cut=%v", got, cut)
	}
	// 汉字在 BMP 内每字 1 个单元（字节才是 3）：截到 3 得前三个字。
	if got, cut := UTF16Truncate("你好世界", 3); got != "你好世" || !cut {
		t.Errorf("截断结果 %q cut=%v，期望 你好世/true", got, cut)
	}
	// n<=0 是遗留语义：不截断。context/manager.go 注释把这称作“静默失效”，
	// 是否改成按预算置空属行为改动，这里先钉住现状。
	if got, cut := UTF16Truncate("abc", 0); got != "abc" || cut {
		t.Errorf("n<=0 当前语义应原样返回（钉住现状）：%q/%v", got, cut)
	}
}

// 块长按 UTF-16 算，且不切断代理对（夹具的流式增量要与客户端口径一致）。
func TestUTF16Chunks(t *testing.T) {
	got := UTF16Chunks("abcd", 4)
	if len(got) != 1 || got[0] != "abcd" {
		t.Errorf("4 单元一块不该切： %#v", got)
	}
	got = UTF16Chunks("abcdef", 4)
	if len(got) != 2 || got[0] != "abcd" || got[1] != "ef" {
		t.Errorf("切块错位： %#v", got)
	}
	got = UTF16Chunks("a\U0001F600b", 2) // a(1) + emoji(2) + b(1)
	if len(got) != 2 || got[0] != "a\U0001F600" || got[1] != "b" {
		t.Errorf("把代理对切开了或计数错： %#v", got)
	}
}
