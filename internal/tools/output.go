package tools

// 输出封顶缓冲：工具无关的公共设施，任何元工具都可复用。

import (
	"bytes"
	"fmt"
)

// maxToolOutputBytes 单流（stdout/stderr 各自）输出封顶（2026-09-14 审计 H3 修）
const maxToolOutputBytes = 32 << 20

// cappedBuffer 保留前 n 字节；超出部分丢弃但持续读取（管道保持排空、子进程
// 永不阻塞在写管道上），并记录被丢弃字节数供截断说明用。
type cappedBuffer struct {
	buf  bytes.Buffer
	cap  int
	over int64
}

func newCappedBuffer(n int) *cappedBuffer { return &cappedBuffer{cap: n} }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	// 记录原始长度：必须原样返回，以满足 io.Writer 契约（io.Copy 严格要求
	// n == len(p)，否则报 ErrShortWrite）。
	//
	// 修复（2026-10-04 实证）：旧实现在截断分支里 `p = p[room:]` 后
	// `return len(p)`，返回的是【剩余】长度而非原始长度——实测 cap=5 写 10 字节
	// 返回 5、写 6 字节返回 1。Unix 侧因 os/exec 容忍 ErrShortWrite 而"侥幸正常"，
	// 但契约是坏的：任何严格调用方（如 io.Copy 直连）都会出错。
	orig := len(p)
	room := c.cap - c.buf.Len()
	if room >= orig {
		return c.buf.Write(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
	}
	// 丢弃的字节数 = 原始长度 - 实际容纳量
	c.over += int64(orig - maxInt(room, 0))
	return orig, nil
}

// maxInt 取较大值（Go 1.21 的 builtin max 在此保守起见自实现，避免版本依赖）。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (c *cappedBuffer) String() string {
	if c.over == 0 {
		return c.buf.String()
	}
	// 说明放开头：agent 的 ResultLimit 截断保留头部（模型据此知道丢弃量）；
	// 尾部会被切掉。
	return fmt.Sprintf("[输出截断：仅保留前 %d 字节，另有 %d 字节丢弃（共约 %d 字节）]\n",
		c.cap, c.over, int64(c.cap)+c.over) + c.buf.String()
}
