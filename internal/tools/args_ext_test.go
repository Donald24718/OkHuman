package tools

import (
	"errors"
	"testing"
)

// setRandHook 安装测试 hook 并在测试结束时还原为 nil。
// hook 存于 atomic.Value，读写并发安全（对应 randHex 内的 Load）。
func setRandHook(t *testing.T, h func([]byte) (int, error)) {
	t.Helper()
	randHexTestHook.Store(h)
	t.Cleanup(func() { randHexTestHook.Store((func([]byte) (int, error))(nil)) })
}

// TestRandHexFallbackOnError 覆盖 crypto/rand 失败兜底分支——旧实现直接调用
// crypto/rand.Read，该分支无法被单测触达。
//
// 断言设计：必须能区分"hook 真的生效"与"hook 静默失效"（atomic.Value 类型断言
// 失败会静默忽略，见 TestRandHexNormalPathUsesHook）。因此这里用 hookFired 标志
// 显式确认注入生效，再校验兜底产物——否则 hook 失效时本测试会假绿。
func TestRandHexFallbackOnError(t *testing.T) {
	hookFired := false
	setRandHook(t, func(b []byte) (int, error) {
		hookFired = true
		return 0, errors.New("entropy unavailable")
	})
	got := randHex(4)
	if !hookFired {
		t.Fatal("hook 未生效（断言失败被静默忽略），本测试对兜底分支无效")
	}
	if got == "" {
		t.Fatal("兜底路径不应返回空串")
	}
	// 兜底串必须非空且是合法十六进制（回归：时钟回拨致 UnixNano 为负时
	// %x 会输出带 '-' 的非法 hex，已用 uint64 包裹修掉）。
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("兜底串含非 hex 字符 %q in %q", c, got)
		}
	}
}

// TestRandHexFallbackNegativeNanos 回归背景：兜底串原为内联 `%x` + UnixNano()，
// 时钟回拨到 1970 前时 UnixNano 为负，`%x` 会输出带 '-' 的**非法 hex**
// （实测 -1234567890 → "-499602d2"）。用 uint64 包裹修正。
//
// 该断言此前是**假的**：内联实现让测试只能走真实时钟（恒为正），
// 实测"去掉 uint64 全部测试仍通过"。抽成 randHexFallback(nanos) 后可直接喂负数。
func TestRandHexFallbackNegativeNanos(t *testing.T) {
	got := randHexFallback(-1234567890)
	if got == "" {
		t.Fatal("兜底串不应为空")
	}
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("负 UnixNano 产生了非法 hex 字符 %q in %q（uint64 包裹失效）", c, got)
		}
	}
	// 正数也应是合法 hex（基本形态）
	for _, c := range randHexFallback(1234567890) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("正 UnixNano 产生非法 hex：%q", randHexFallback(1234567890))
		}
	}
}

// TestRandHexNormalPathUsesHook 确认 hook 生效时其注入的字节被采纳。
func TestRandHexNormalPathUsesHook(t *testing.T) {
	called := false
	setRandHook(t, func(b []byte) (int, error) {
		called = true
		for i := range b {
			b[i] = 0xab
		}
		return len(b), nil
	})
	got := randHex(2)
	if !called {
		t.Fatal("hook 未被调用")
	}
	if got != "abab" {
		t.Errorf("hook 注入的字节未被采纳：%q", got)
	}
}

// TestRandHexHookRestores 确认 hook 清理后恢复真实随机源（防测试互相污染）。
func TestRandHexHookRestores(t *testing.T) {
	setRandHook(t, func(b []byte) (int, error) { return 0, errors.New("x") })
	_ = randHex(4)
	randHexTestHook.Store((func([]byte) (int, error))(nil))
	// 还原后两次调用应不同（真实随机源生效）
	if a, b := randHex(8), randHex(8); a == b {
		t.Errorf("hook 清理后随机源未恢复：%q == %q", a, b)
	}
}
