package tools

// 参数与随机工具函数：工具无关的公共设施，任何元工具都可复用。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"sync/atomic"
	"time"
)

// maxSafeInt64 / maxSafeUint64 是 float64 能无损表示的最大整数（2^53）。
// 超过它的整数转 float64 会丢精度，numArg 宁可拒绝也不静默截断。
//
// 边界含等号：2^53 本身可被 float64 精确表示，允许通过；判断必须用
// `> maxSafeInt64`（严格大于），不可改成 `>=`，否则会把合法边界值也拒掉。
const (
	maxSafeInt64  = int64(1) << 53
	maxSafeUint64 = uint64(1) << 53
)

// numArg 数值参数取数：把调用方给的值收敛成 float64。
//
// JSON 解码（json.Unmarshal 到 interface{}）恒为 float64，但 Go 原生调用方
// 可能传任意整数类型——只认少数几种会把其余类型静默忽略（退回默认值），
// 行为与调用方意图相反。这里按 kind 覆盖全部整数/浮点家族，并兼容
// json.Decoder.UseNumber() 产生的 json.Number。
//
// 拒绝的情形（返回 (0, false)）：nil、非数值类型、NaN、±Inf，
// 以及超出 2^53 的整数（float64 无法精确表示，拒绝优于截断）。
func numArg(v interface{}) (float64, bool) {
	if v == nil {
		return 0, false
	}
	// UseNumber 场景：数字是 json.Number（底层 string），需显式解析。
	//
	// 必须整数优先：json.Number.Float64() 会把 2^53 之上的整数**静默截断**
	// （"9007199254740993" → ...992），与下面 reflect.Int64 分支的"拒绝优于截断"
	// 自相矛盾。先用 Int64() 精确解析、复用同一套 2^53 守卫。
	//
	// 注意：json.Number 只提供 Int64()/Float64()，**没有 Uint64()**（Go 标准库
	// 限制）。超 int64 的正整数需自parse：仅当字面量全为数字时才按 uint64 解析，
	// 解析失败（含小数点/指数/负号/超 uint64）再退回 Float64。
	// 顺序不可调换：整数优先，浮点兜底。
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			if i > maxSafeInt64 || i < -maxSafeInt64 {
				return 0, false
			}
			return float64(i), true
		}
		if u, ok := parseUintLiteral(string(n)); ok {
			if u > maxSafeUint64 {
				return 0, false
			}
			return float64(u), true
		}
		f, err := n.Float64()
		if err != nil || !isFinite(f) {
			return 0, false
		}
		// 浮点字面量：值为整数且超安全范围时同样拒绝（与整数分支口径一致）。
		if f == math.Trunc(f) && (f > float64(maxSafeInt64) || f < -float64(maxSafeInt64)) {
			return 0, false
		}
		return f, true
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if !isFinite(f) {
			return 0, false // NaN / ±Inf 参与后续计算会产出不可预期结果
		}
		return f, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i := rv.Int()
		if i > maxSafeInt64 || i < -maxSafeInt64 {
			return 0, false // 超出 float64 无损范围
		}
		return float64(i), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := rv.Uint()
		if u > maxSafeUint64 {
			return 0, false
		}
		return float64(u), true
	}
	return 0, false
}

// isFinite 报告 f 既不是 NaN 也不是 ±Inf。
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// parseUintLiteral 尝试把 JSON 数字字面量按 uint64 解析。
// 仅接受纯数字（可选前导零），任何小数点/指数/负号/字母都返回 false，
// 交给调用方的 Float64 兜底——保证不误判浮点或负数。
func parseUintLiteral(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	u, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false // 超出 uint64 范围
	}
	return u, true
}

// requireNonEmptyStr 取非空字符串参数。
//
// 命名刻意带 NonEmpty：空字符串是合法 string，但本函数的用途（当前唯一调用点
// bash 的 command）需要"真的有内容"——空命令会以退出码 0 静默"成功"，
// 让 agent 误判命令已执行。因此拒绝空串是**有意行为**，不是类型检查的副作用。
// 需要区分"键缺失"与"值为空串"的调用方，请直接读 map 而非用本函数。
func requireNonEmptyStr(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string) // 双返回值断言：v 为 nil 时不 panic，安全
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// maxRandHexBytes randHex 允许的最大字节数。
//
// 为什么是 64：本函数只服务文件名去向（当前唯一调用 randHex(4)）。依据是
// **文件名词长余量**，不是精确计算——十六进制后每字节变 2 字符，64 字节
// = 128 字符；加上前缀 okhuman-bash-、UnixMilli（11 字符）与 .sh 后缀，
// 整名约 157 字符，仍低于常见文件系统 NAME_MAX=255（Linux 实测）。
// 实际调用 randHex(4) 只占 8 字符，64 是"宽松但有限"的取整值：既比实际
// 所需宽 16 倍（给未来调用方留空间），又足以挡住 1e9 这类误传值。
//
// 这不是安全边界，也不追求"恰好够用"：封顶只为挡住误传巨值（如 1e9）导致的
// 一次性大分配。调用方要多少给多少（在 64 以内），语义清晰。
const maxRandHexBytes = 64

// randHexTestHook 测试注入点：非 nil 时替代 crypto/rand.Read，
// 供单测覆盖失败兜底分支。生产环境恒为 nil。
//
// 用 atomic.Value 而非裸包级变量：并行测试（t.Parallel）或同包多测试并发跑时，
// 裸变量的读（randHex 内）与写（测试设置）会触发 DATA RACE。存 nil 表示未注入。
var randHexTestHook atomic.Value

// randHex 返回 n 字节的十六进制随机串（临时脚本文件名去重用）。
//
// 调用契约：n 是**代码内的可信常量**（如 randHex(4)），不是外部输入。
// 即便如此也不 panic：负长度归零、超 maxRandHexBytes 截断，保证任何入参都
// 返回可用字符串。crypto/rand 失败（系统熵源不可用，实际不会发生）时退回
// 时间戳——长度与正常路径不同，但唯一用途是文件名去重，调用方不得依赖长度。
func randHex(n int) string {
	if n < 0 {
		n = 0
	}
	if n > maxRandHexBytes {
		n = maxRandHexBytes
	}
	b := make([]byte, n)
	read := rand.Read
	if h, ok := randHexTestHook.Load().(func([]byte) (int, error)); ok && h != nil {
		read = h
	}
	if _, err := read(b); err != nil {
		return randHexFallback(time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// randHexFallback crypto/rand 失败时的兜底串（弱随机、仅供文件名去重）。
//
// ⚠️ 唯一性：**不保证**。nanos 仅纳秒精度，同一纳秒的并发调用会产生**完全相同**
// 的字符串——实测 100 个 goroutine 共享一个 nanos 时去重后只剩 1 种；即便各自
// 调 time.Now()，10000 次并发也只留下 7 种。这是**有意接受的取舍**：crypto/rand
// 失败意味着系统熵源已不可用，此时纠结去重强度是本末倒置——文件名撞了顶多
// 覆盖一个临时脚本（下次调用即重建），远轻于因熵源故障而崩溃。
// 若将来需要更强兜底，应引入原子计数器而非调整本函数。
//
// 独立成函数是为了**可测**：此前内联在 randHex 里，测试只能走真实时钟——
// 而正常时钟的 UnixNano 恒为正，导致 uint64 包裹这一修复点无法被验证
// （实测：去掉 uint64 后所有测试仍通过 = 该断言是假的）。抽出后可直接
// 喂负数验证 hex 合法性。
func randHexFallback(nanos int64) string {
	// uint64 包裹：时钟回拨到 1970 前时 UnixNano 为负，%x 会输出带 '-'
	// 的非法 hex（实测 -1234567890 → "-499602d2"）。uint64 保证恒合法。
	return fmt.Sprintf("%x", uint64(nanos))
}
