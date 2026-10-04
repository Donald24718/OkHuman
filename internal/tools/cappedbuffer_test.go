package tools

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestCappedBuffer_SatisfiesIOWriterContract 回归：cappedBuffer 作为 io.Writer，
// Write 必须返回【原始】len(p)，即使内容被截断丢弃。
//
// 背景（2026-10-04 实证）：旧实现
//
//	if room > 0 { c.buf.Write(p[:room]); p = p[room:] }
//	c.over += int64(len(p))
//	return len(p), nil        // ← 返回被重新切片后的长度
//
// 实测 cap=5 写 10 字节 → 返回 5（应 10）。io.Copy 因此返回 ErrShortWrite。
// Unix 侧靠 os/exec 容忍、Windows 侧靠 copyHandleTo 不看返回值而"侥幸正常"，
// 但契约是坏的：任何严格调用方都会出错。
func TestCappedBuffer_SatisfiesIOWriterContract(t *testing.T) {
	cases := []struct {
		cap  int
		data string
	}{
		{5, "1234567890"},                // 跨越边界（旧实现返回 5）
		{5, "12345"},                     // 正好填满
		{5, "123456"},                    // 超 1 字节
		{100, strings.Repeat("A", 1000)}, // 大幅超出
		{0, "abc"},                       // cap=0 极端
		{10, "短"},                        // 多字节 + 未超
		{1, "中文测试"},                      // 多字节 + 超
	}
	for _, tc := range cases {
		c := newCappedBuffer(tc.cap)
		n, err := c.Write([]byte(tc.data))
		if err != nil {
			t.Errorf("cap=%d 写 %d 字节: Write 返回意外 error: %v", tc.cap, len(tc.data), err)
		}
		if n != len(tc.data) {
			t.Errorf("cap=%d 写 %d 字节: Write 返回 n=%d，必须等于 len(p)=%d（io.Writer 契约）",
				tc.cap, len(tc.data), n, len(tc.data))
		}
	}
}

// TestCappedBuffer_IOCopyNoShortWrite io.Copy 是 os/exec 的内部机制，必须不报错。
func TestCappedBuffer_IOCopyNoShortWrite(t *testing.T) {
	c := newCappedBuffer(100)
	src := bytes.NewReader(bytes.Repeat([]byte("A"), 1000))
	n, err := io.Copy(c, src)
	if err != nil {
		t.Errorf("io.Copy 返回错误 %v（cappedBuffer 违反 io.Writer 契约）", err)
	}
	if n != 1000 {
		t.Errorf("io.Copy 应报告写入 1000（全部被接受），实际 %d", n)
	}
	if c.buf.Len() != 100 {
		t.Errorf("应保留前 100 字节，实际 %d", c.buf.Len())
	}
	if c.over != 900 {
		t.Errorf("over 应 = 900，实际 %d", c.over)
	}
}

// TestCappedBuffer_OverAccounting over 计数精确（含分片写入）。
func TestCappedBuffer_OverAccounting(t *testing.T) {
	c := newCappedBuffer(10)
	// 先写满
	c.Write([]byte("0123456789"))
	// 分片写超限内容
	c.Write([]byte("aaaaa")) // over 5
	c.Write([]byte("bbb"))   // over 8
	if c.over != 8 {
		t.Errorf("over 应 = 8，实际 %d", c.over)
	}
	if c.buf.Len() != 10 {
		t.Errorf("buf 应保持 10，实际 %d", c.buf.Len())
	}
	// 全部未超限时 over 必须为 0
	c2 := newCappedBuffer(100)
	c2.Write([]byte("short"))
	if c2.over != 0 {
		t.Errorf("未超限时 over 应 = 0，实际 %d", c2.over)
	}
}

// TestCappedBuffer_StringWithTruncationNote 截断说明文本正确。
func TestCappedBuffer_StringWithTruncationNote(t *testing.T) {
	c := newCappedBuffer(5)
	c.Write([]byte("1234567890"))
	out := c.String()
	if !strings.Contains(out, "输出截断") {
		t.Errorf("超限时应含截断说明: %q", out)
	}
	if !strings.HasSuffix(out, "12345") {
		t.Errorf("正文应为前 5 字节: %q", out)
	}
	// 未超限时不应有说明
	c2 := newCappedBuffer(100)
	c2.Write([]byte("ok"))
	if strings.Contains(c2.String(), "输出截断") {
		t.Errorf("未超限不应有说明: %q", c2.String())
	}
}
