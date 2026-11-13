package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	ctxmgr "okhuman/internal/context"
	"okhuman/internal/types"
)

// validateContentParts OpenAI parts 校验（非空数组；每 part 须为带非空 type 的 JSON 对象——
// 透传管道，part 类型语义属于 LLM 端点，主程序不逐类型识别）
func validateContentParts(v interface{}) []types.ContentPart {
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]types.ContentPart, 0, len(arr))
	for _, p := range arr {
		o, ok := p.(map[string]interface{})
		if !ok {
			return nil
		}
		part, err := types.FromPartMap(o)
		if err != nil {
			return nil
		}
		out = append(out, part)
	}
	return out
}

func safeName(name string) bool {
	return safeNameRe.MatchString(name) && !strings.HasPrefix(name, ".") && !strings.Contains(name, "..")
}

// tryReadJSON 读 JSON body（失败 → nil，与 TS c.req.json().catch(() => null) 等价）
func tryReadJSON(r *http.Request) map[string]interface{} {
	var m map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&m); err != nil {
		return nil
	}
	return m
}

// lastN 取末尾 n 条（空 → 空数组，非 nil，JSON 输出 [] 而非 null）
func lastN[T any](xs []T, n int) []T {
	if len(xs) <= n {
		out := make([]T, 0, len(xs))
		return append(out, xs...)
	}
	out := make([]T, 0, n)
	return append(out, xs[len(xs)-n:]...)
}

// sessionHasInjectRef inject_ref 是否仍在会话里（2026-09-12）：在 = 侧车必须保留
// （KV 前缀稳定）；不在 = 已被压缩吃掉，清掉安全（无匹配断）。
func sessionHasInjectRef(cm *ctxmgr.Manager, id string) bool {
	// 快照取一次（2026-09-14 审计修：旧代码每行调两次 Session() 取活引用，
	// 两次不一致时索引越界 panic）
	msgs := cm.Session().Messages
	for i := range msgs {
		for _, p := range types.AsParts(msgs[i].Content) {
			if p.Type == "inject_ref" && p.Ref == id {
				return true
			}
		}
	}
	return false
}

func jsonStr(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func strOf(v interface{}) string {
	s, _ := v.(string)
	return s
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// maxBodyBytes 请求体上限（2026-09-14 审计修：旧实现无上限，恶意/失控客户端
// 可灌 GB 级 JSON 撑爆内存；256MB 已远大于任何正常请求——/inject 的大图 base64
// 也在其内）
