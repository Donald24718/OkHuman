package tools

import (
	"encoding/json"
	"math"
	"testing"
)

// TestNumArgTypeCoverage 覆盖全部整数/浮点家族 + json.Number。
// 回归背景：旧实现只认 float64/float32/int/int64，int32/uint* 会被静默忽略
// （返回 (0,false)，调用方退回默认值）。
func TestNumArgTypeCoverage(t *testing.T) {
	cases := []struct {
		name   string
		in     interface{}
		want   float64
		wantOK bool
	}{
		{"float64", float64(1.5), 1.5, true},
		{"float32", float32(2.5), 2.5, true},
		{"int", int(3), 3, true},
		{"int8", int8(4), 4, true},
		{"int16", int16(5), 5, true},
		{"int32", int32(6), 6, true}, // 旧实现漏掉
		{"int64", int64(7), 7, true},
		{"uint", uint(8), 8, true},   // 旧实现漏掉
		{"uint8", uint8(9), 9, true}, // 旧实现漏掉
		{"uint16", uint16(10), 10, true},
		{"uint32", uint32(11), 11, true},
		{"uint64", uint64(12), 12, true},
		{"json.Number", json.Number("13"), 13, true}, // UseNumber 场景
		{"json.Number float", json.Number("2.5"), 2.5, true},
		{"nil", nil, 0, false},
		{"string", "abc", 0, false},
		{"bool", true, 0, false},
		{"slice", []int{1}, 0, false}, // reflect 不得把无意义类型当数值接受
	}
	for _, c := range cases {
		got, ok := numArg(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("%s: numArg(%v) = (%v,%v), want (%v,%v)", c.name, c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// TestNumArgRejectsNonFinite 回归背景：+Inf / 1e300 经 int() 在 amd64 上溢出为
// 最小 int64 → bash.go 的 `timeoutSec < 1` 兜底把超时**强制成 1 秒**（比调用方
// 意图短数个数量级）。numArg 必须在源头拒绝非有限值。
func TestNumArgRejectsNonFinite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, ok := numArg(v); ok {
			t.Errorf("numArg(%v) 应为 false（非有限值不可作为参数）", v)
		}
	}
	if _, ok := numArg(json.Number("NaN")); ok {
		t.Error("json.Number(\"NaN\") 应为 false")
	}
	if _, ok := numArg(json.Number("1e400")); ok {
		t.Error("json.Number(\"1e400\") 超出 float64 应为 false")
	}
}

// TestNumArgRejectsUnsafeIntPrecision 2^53 之上 float64 无法无损表示，拒绝优于截断。
func TestNumArgRejectsUnsafeIntPrecision(t *testing.T) {
	if _, ok := numArg(int64(1)<<53 + 1); ok {
		t.Error("2^53+1 应被拒绝（float64 无法精确表示）")
	}
	if _, ok := numArg(uint64(1)<<53 + 1); ok {
		t.Error("uint 2^53+1 应被拒绝")
	}
	// 边界内应通过
	if f, ok := numArg(int64(1) << 53); !ok || f != float64(int64(1)<<53) {
		t.Errorf("2^53 边界值应通过，got (%v,%v)", f, ok)
	}
}

// TestNumArgJSONNumberConsistentWithInt64 回归背景：json.Number 分支若直接
// Float64() 会把 2^53 之上的整数**静默截断**，而 int64 分支会**拒绝**——
// 同一个数值走两条分支行为相反，函数内部逻辑不自洽。两条路径必须同口径。
func TestNumArgJSONNumberConsistentWithInt64(t *testing.T) {
	const bigInt = int64(1)<<53 + 1 // 9007199254740993

	_, viaInt := numArg(bigInt)
	_, viaJSON := numArg(json.Number("9007199254740993"))
	if viaInt != viaJSON {
		t.Errorf("同一数值两条分支不一致：int64 接受=%v，json.Number 接受=%v", viaInt, viaJSON)
	}
	if viaInt || viaJSON {
		t.Error("2^53+1 两条分支都应拒绝")
	}

	// 大 uint64 经 json.Number 也应拒绝
	if _, ok := numArg(json.Number("18446744073709551615")); ok {
		t.Error("uint64 max 经 json.Number 应被拒绝")
	}
	// 浮点字面量形式且确实越界（值 > 2^53）的整数应被拒绝。
	// 注意 9.007199254740993e15 会被 float64 舍入到恰好 2^53（边界内，应通过）——
	// 因此这里用真正越界的 9.007199254740994e15。
	if _, ok := numArg(json.Number("9.007199254740994e15")); ok {
		t.Error("浮点形式的越界整数应被拒绝（整数性且 > 2^53）")
	}
	// 而舍入后恰为 2^53 的字面量属边界内，应通过
	if _, ok := numArg(json.Number("9.007199254740993e15")); !ok {
		t.Error("舍入到 2^53 的浮点字面量属边界内，应通过")
	}

	// 正常范围内 json.Number 各形态仍应通过
	for _, c := range []struct {
		in   string
		want float64
	}{
		{"13", 13},
		{"2.5", 2.5},
		{"-7", -7},
		{"1e3", 1000},
		{"9007199254740992", float64(int64(1) << 53)}, // 恰好 2^53，边界内
	} {
		if got, ok := numArg(json.Number(c.in)); !ok || got != c.want {
			t.Errorf("json.Number(%q) = (%v,%v), want (%v,true)", c.in, got, ok, c.want)
		}
	}
}

// TestRequireNonEmptyStr 空串拒绝是**有意行为**（空命令会静默"成功退出 0"）。
func TestRequireNonEmptyStr(t *testing.T) {
	m := map[string]interface{}{"a": "x", "empty": "", "n": 1, "nil": nil}
	if s, ok := requireNonEmptyStr(m, "a"); !ok || s != "x" {
		t.Errorf("正常值: got (%q,%v)", s, ok)
	}
	if _, ok := requireNonEmptyStr(m, "empty"); ok {
		t.Error("空串应被拒绝")
	}
	if _, ok := requireNonEmptyStr(m, "n"); ok {
		t.Error("非字符串应被拒绝")
	}
	if _, ok := requireNonEmptyStr(m, "nil"); ok {
		t.Error("nil 应被拒绝")
	}
	if _, ok := requireNonEmptyStr(m, "missing"); ok {
		t.Error("缺失键应被拒绝")
	}
}

// TestRandHexLengthAndRange 回归背景：断言正常路径长度 == 2n，
// 防止未来有人改动破坏调用方的长度假设。
func TestRandHexLengthAndRange(t *testing.T) {
	s := randHex(4)
	if len(s) != 8 {
		t.Fatalf("randHex(4) 长度应为 8，得 %d（%q）", len(s), s)
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("非十六进制字符 %q in %q", c, s)
		}
	}
	// 两次调用应不同（同纳秒并发碰撞是理论问题，这里只做基本去重校验）
	if a, b := randHex(8), randHex(8); a == b {
		t.Errorf("两次 randHex(8) 相同：%q（随机源异常）", a)
	}
}

// TestRandHexBoundaryNoPanic 回归背景：randHex(-1) 旧实现 panic
// makeslice: len out of range；巨值会一次性分配巨量内存。
func TestRandHexBoundaryNoPanic(t *testing.T) {
	if got := randHex(-1); len(got) != 0 {
		t.Errorf("randHex(-1) 应为空串（不 panic），得 %q", got)
	}
	if got := randHex(0); len(got) != 0 {
		t.Errorf("randHex(0) 应为空串，得 %q", got)
	}
	// 巨值应被 clamp，不得尝试分配 1e9 字节
	if got := randHex(1e9); len(got) != maxRandHexBytes*2 {
		t.Errorf("randHex(1e9) 应截断到 %d 字节的十六进制，得 %d", maxRandHexBytes, len(got))
	}
}
