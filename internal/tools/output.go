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
	room := c.cap - c.buf.Len()
	if room >= len(p) {
		return c.buf.Write(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
		p = p[room:]
	}
	c.over += int64(len(p))
	return len(p), nil
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
