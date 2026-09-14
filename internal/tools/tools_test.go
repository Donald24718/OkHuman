package tools

import (
	"strings"
	"testing"
)

// cappedBuffer 行为（2026-09-14 审计 H3 修）：前 n 字节保留、超出丢弃但计数、
// 管道持续排空（Write 永不报错、永不阻塞）。
func TestCappedBufferWithinCap(t *testing.T) {
	c := newCappedBuffer(10)
	if n, err := c.Write([]byte("12345")); err != nil || n != 5 {
		t.Fatalf("write1: n=%d err=%v", n, err)
	}
	if s := c.String(); s != "12345" {
		t.Fatalf("未超限时内容应为原文，got %q", s)
	}
}

func TestCappedBufferOverCap(t *testing.T) {
	c := newCappedBuffer(6)
	c.Write([]byte("abcdef")) // 恰好填满
	c.Write([]byte("XYZ"))    // 全部超出
	s := c.String()
	if !strings.Contains(s, "abcdef") {
		t.Fatalf("正文丢失: %q", s)
	}
	if !strings.Contains(s, "输出截断") || !strings.Contains(s, "丢弃") {
		t.Fatalf("缺截断说明: %q", s)
	}
	// 说明在正文之前（保头部截断可存活）
	if strings.Index(s, "输出截断") > strings.Index(s, "abcdef") {
		t.Fatalf("截断说明应在正文之前: %q", s)
	}
	if c.over != 3 {
		t.Fatalf("over=%d want 3", c.over)
	}
	// 继续写仍不报错（管道排空语义）
	if n, err := c.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("超限后 write: n=%d err=%v", n, err)
	}
	if c.over != 7 {
		t.Fatalf("over=%d want 7", c.over)
	}
}

func TestCappedBufferPartialOver(t *testing.T) {
	c := newCappedBuffer(4)
	c.Write([]byte("123456")) // 4 保留 + 2 丢弃
	s := c.String()
	// 说明在前、正文在后
	if strings.Index(s, "输出截断") > strings.Index(s, "1234") {
		t.Fatalf("截断说明应在正文之前: %q", s)
	}
	if !strings.HasSuffix(s, "1234") {
		t.Fatalf("保留正文 1234 缺失: %q", s)
	}
	if c.over != 2 {
		t.Fatalf("over=%d want 2", c.over)
	}
}
