package ipython

// ipython 元工具的入口与包级状态。
//
// 依赖方向：本包被 internal/tools 引用，**不得反向 import tools**（循环导入）。
// 因此 tools.numArg 这类工具函数在本包有一份同义实现（见 numArg 注释）。
//
// 设计取向：启用与否由配置显式决定（tools.ipython_python）。未配置时
// **工具表不含本工具**，现有部署的工具契约与 KV 前缀逐字不变。

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// minTimeoutSec 超时下限。给 1 秒以下毫无意义（进程握手都不够），
	// 但完全拒绝会让模型反复重试同一个注定失败的参数。
	minTimeoutSec = 1

	// fallBackDefaultSec 未接线 Configure 时的兜底超时。
	fallBackDefaultSec = 60
)

var (
	enabled    atomic.Bool
	pythonBin  atomic.Pointer[string]
	defMS      atomic.Int64
	maxMS      atomic.Int64
	sessionKey atomic.Pointer[string]

	manager = NewManager()
)

// Setup 配置并按需启用本工具。
//
// python 为空串 → 禁用（并关掉已在跑的内核）；
// python 为 "auto" → 自动探测第一个带 IPython 的解释器；
// 其余 → 当作解释器路径直接校验。
//
// 返回 error 只表示「未能启用」；因未配置而禁用不是错误（返回 nil）。
// 调用方应当打日志而不是因此中断启动。
func Setup(python string, defaultMS, maxMs int) error {
	p := strings.TrimSpace(python)
	if p == "" {
		enabled.Store(false)
		manager.ShutdownAll()
		return nil
	}

	switch p {
	case "auto":
		found, err := DetectPython()
		if err != nil {
			enabled.Store(false)
			manager.ShutdownAll()
			return err
		}
		p = found
	default:
		if _, err := VerifyPython(p); err != nil {
			enabled.Store(false)
			manager.ShutdownAll()
			return fmt.Errorf("指定的 Python 不可用或未装 IPython（%s）: %w", p, err)
		}
	}

	defMS.Store(int64(defaultMS))
	maxMS.Store(int64(maxMs))

	// 解释器换了 → 旧内核是用老二进制起的，必须全部作废。
	if old := pythonBin.Load(); old != nil && *old != p {
		manager.ShutdownAll()
	}
	pythonBin.Store(&p)
	enabled.Store(true)
	return nil
}

// Enabled 本工具是否已启用且探测通过（工具表是否注册本工具）。
func Enabled() bool { return enabled.Load() }

// PythonPath 当前生效的解释器路径（日志/观测用）。
func PythonPath() string {
	if p := pythonBin.Load(); p != nil {
		return *p
	}
	return ""
}

func pythonPath() string { return PythonPath() }

// SetSessionKey 设置当前会话的内核隔离键。
//
// 上层在会话切换时调用；不调用则整个进程共享一个内核（单会话部署的正确行为）。
// 之所以做成包级状态而非给 Exec 加参数：ExecuteTool 的签名是稳定的对外契约，
// 改签名要动所有调用方与测试，收益不匹配。
func SetSessionKey(k string) {
	v := k
	sessionKey.Store(&v)
}

func currentSessionKey() string {
	if p := sessionKey.Load(); p != nil {
		return *p
	}
	return ""
}

// ResetSession 会话切换：作废旧会话的内核并换用新的隔离键。
//
// 上层必须在每个「新会话」边界（/reset）调用。**不调用就不是"退化成共享内核"
// 那么温和**——旧会话的变量会原封不动活在内核里，模型以为自己从零开始，
// 实际 x / df 全在，属于最难排查的那类静默状态污染。
//
// 2026-10-05 接线前的事实：SetSessionKey 全仓无人调用，/reset 不清内核。
func ResetSession(key string) {
	if old := currentSessionKey(); old == key {
		return // 同一会话重复切换：不动内核，避免白白打断在跑的 cell
	}
	manager.Recycle(currentSessionKey()) // 旧内核随会话清除（内部会 Shutdown）
	SetSessionKey(key)
}

// Shutdown 停止全部内核（进程清理钩子）。
func Shutdown() { manager.ShutdownAll() }

// Exec 执行一次 IPython 代码。
//
// 返回契约与 bash 工具一致：**用户代码出错、被中断、甚至内核崩溃都算成功
// 返回**（把情况写进输出文本交给模型判断），只有「起不来/发不出去」这类
// 宿主侧故障才返回 error。理由同 bash 的非零退出码——模型需要看到 traceback。
func Exec(args map[string]interface{}) (string, error) {
	if !enabled.Load() {
		return "", errors.New("ipython 工具未启用：请在配置里设置 tools.ipython_python 并安装 ipython")
	}
	code, ok := args["code"]
	if !ok {
		return "", errors.New("缺少参数 code")
	}
	src, ok := code.(string)
	if !ok || strings.TrimSpace(src) == "" {
		// 空代码会以一个无意义的 Out[N] 静默"成功"，令模型误判已执行。
		return "", errors.New("参数 code 必须为非空字符串")
	}

	timeout := timeoutFrom(args)
	key := currentSessionKey()
	k := manager.Get(key)

	res, err := k.Exec(src, timeout)
	if err != nil {
		manager.Recycle(key) // 起不来的内核没理由留着
		return "", err
	}
	if res.Killed || res.Lost {
		// 进程已不在了：清账，下次调用会重建一个干净内核。
		manager.Recycle(key)
	}
	// 图片/PDF 在这里落盘：内核层只管协议，格式层管文本，落盘是宿主职责。
	saveArtifacts(&res)
	return FormatResult(res, timeout), nil
}

// timeoutFrom 取超时：缺省用配置的默认上限，并夹到 [minTimeoutSec, maxMS]。
//
// 夹上限而不是报错：模型提出 9999 秒多半是想表达"尽量久"，拒绝它只会让它
// 重试；如实按配置上限执行并在输出里说明，才是有效反馈。
func timeoutFrom(args map[string]interface{}) time.Duration {
	defSec, maxSec := appliedLimits(defMS.Load(), maxMS.Load())
	d := time.Duration(defSec) * time.Second

	v, ok := args["timeout_seconds"]
	if !ok {
		return d
	}
	f, ok := numArg(v)
	if !ok || f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return d
	}
	if f > float64(maxSec) {
		f = float64(maxSec)
	}
	if f < minTimeoutSec {
		f = minTimeoutSec
	}
	return time.Duration(f * float64(time.Second))
}

// appliedLimits 把毫秒配置整理成秒（供输出文案与夹逼共用）。
func appliedLimits(defRaw, maxRaw int64) (defSec, maxSec int) {
	defSec = fallBackDefaultSec
	maxSec = fallBackDefaultSec
	if defRaw > 0 {
		defSec = int(defRaw / 1000)
	}
	if maxRaw > 0 {
		maxSec = int(maxRaw / 1000)
	}
	if maxSec < defSec {
		maxSec = defSec
	}
	if defSec < minTimeoutSec {
		defSec = minTimeoutSec
	}
	return defSec, maxSec
}

// DefaultSec / MaxSec 暴露给 tools 包生成工具描述（保证描述数字与实际夹逼同源）。
func DefaultSec() int {
	d, _ := appliedLimits(defMS.Load(), maxMS.Load())
	return d
}

// MaxSec 见 DefaultSec。
func MaxSec() int {
	_, m := appliedLimits(defMS.Load(), maxMS.Load())
	return m
}

// numArg 把任意数值收敛成 float64。
//
// 为什么不复用 tools.numArg：tools 依赖本包，反向 import 会成环。
// 这里只覆盖 LLM 可能给出的类型（JSON 解码恒为 float64，外加原生 int 家族
// 与 json.Number），语义与 tools.numArg 一致。
func numArg(v interface{}) (float64, bool) {
	if v == nil {
		return 0, false
	}
	const maxSafe = 1 << 53
	if n, ok := v.(json.Number); ok {
		if f, err := n.Float64(); err == nil {
			return f, true
		}
		return 0, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i := rv.Int()
		if i > maxSafe || i < -maxSafe {
			return 0, false
		}
		return float64(i), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := rv.Uint()
		if u > maxSafe {
			return 0, false
		}
		return float64(u), true
	}
	// strconv 兜底：部分调用方（如自定义 JSON 解码）会传字符串数字。
	switch s := v.(type) {
	case string:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}
