// 总结注入头里的"覆盖 seq a-b"每轮都发给模型，值错就是每轮都在骗它（第四轮 N6）。
package context

import "testing"

func TestBatchSeqRange(t *testing.T) {
	// 累计 11 条、会话现存 11 条、压最旧 9 条 → 真实区间 1..9（旧公式给 3..11）
	if from, to := batchSeqRange(11, 11, 9); from != 1 || to != 9 {
		t.Errorf("首轮压缩：应为 1-9，实际 %d-%d", from, to)
	}
	// 已压过一轮：累计 11、现存 4 → 现存覆盖 8..11，最旧 2 条是 8..9。
	// 旧公式（seq-B+1..seq）在这里给 10..11 —— 看着也像"最后两条"，所以一直没被发现。
	if from, to := batchSeqRange(11, 4, 2); from != 8 || to != 9 {
		t.Errorf("二次压缩：应为 8-9，实际 %d-%d", from, to)
	}
}
