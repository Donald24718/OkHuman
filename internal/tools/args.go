package tools

// 参数与随机工具函数：工具无关的公共设施，任何元工具都可复用。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// numArg 数值参数取数：JSON 解码恒为 float64，但 Go 侧调用方可能传 int/int64——
// 只认 float64 会把这类值静默忽略（退回默认上限），行为与调用方意图相反。
func numArg(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func requireStr(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key]
	s, ok2 := v.(string)
	if !ok || !ok2 || s == "" {
		return "", false
	}
	return s, true
}

// randHex n 字节的十六进制随机串（临时脚本文件名去重用）
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()) // crypto/rand 失败兜底（理论上不会）
	}
	return hex.EncodeToString(b)
}
