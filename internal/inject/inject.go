package inject

// 上下文注入附件（2026-09-11）：/inject 的持久层。
// base64 parts 一次性写入 data.dir/inject/<id>.json 侧车文件（会话 json 只存
// inject_ref 引用，不膨胀）；compose 组装请求时经本模块解析成真实 parts
// （读文件 + 内存缓存）。侧车写入后不可变 → 解析确定性 → KV 前缀逐 token 稳定。
//
// 载入回退（2026-09-13）：compose 时载入侧车失败（文件缺失/内容损坏/为空）
// → 移除该附件（删侧车 + onFail 从附件列表摘除）→ 返回**确定性**错误文字存根
// （文本由 id+原因固定生成，无时间戳无随机数，按 id 缓存）→ LLM 看到报错。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"okhuman/internal/types"
)

type Resolver struct {
	dir       string
	onFail    func(id, reason string)
	mu        sync.Mutex
	cache     map[string][]types.ContentPart
	stubCache map[string][]types.ContentPart
}

// NewResolver 构造引用解析器（绑定 inject 目录）：id → parts（带缓存）；
// 载入失败 → 移除 + 确定性错误存根。
func NewResolver(dir string, onFail func(id, reason string)) *Resolver {
	return &Resolver{dir: dir, onFail: onFail, cache: map[string][]types.ContentPart{}, stubCache: map[string][]types.ContentPart{}}
}

// Resolve id → parts。永不返回 nil（失败时返回确定性错误存根）。
func (r *Resolver) Resolve(id string) []types.ContentPart {
	r.mu.Lock()
	defer r.mu.Unlock()
	if hit, ok := r.cache[id]; ok {
		return hit
	}
	if hit, ok := r.stubCache[id]; ok {
		return hit
	}
	p := filepath.Join(r.dir, id+".json")
	var reason string
	if _, err := os.Stat(p); err != nil {
		reason = "侧车文件缺失"
	} else {
		data, err := os.ReadFile(p)
		if err != nil {
			reason = "侧车内容损坏或为空"
		} else {
			var v []interface{}
			if err := json.Unmarshal(data, &v); err != nil || len(v) == 0 {
				reason = "侧车内容损坏或为空"
			} else {
				parts, ok := parseParts(v)
				if !ok {
					reason = "侧车内容损坏或为空"
				} else {
					r.cache[id] = parts
					return parts
				}
			}
		}
	}
	if reason != "" {
		DropInjectFile(r.dir, id)
		if r.onFail != nil {
			r.onFail(id, reason)
		}
		stub := []types.ContentPart{{Type: "text", Text: fmt.Sprintf("（附件 %s 载入失败，已移除：%s）", id, reason)}}
		r.stubCache[id] = stub
		fmt.Printf("[inject] %s 载入失败（%s）→ 已移除，报错已回传 LLM\n", id, reason)
		return stub
	}
	return nil
}

func parseParts(v []interface{}) ([]types.ContentPart, bool) {
	parts := make([]types.ContentPart, 0, len(v))
	for _, p := range v {
		po, ok := p.(map[string]interface{})
		if !ok {
			return nil, false
		}
		t, _ := po["type"].(string)
		switch t {
		case "text":
			s, ok := po["text"].(string)
			if !ok {
				return nil, false
			}
			parts = append(parts, types.ContentPart{Type: "text", Text: s})
		case "image_url":
			iu, ok := po["image_url"].(map[string]interface{})
			url, ok2 := iu["url"].(string)
			if !ok || !ok2 {
				return nil, false
			}
			parts = append(parts, types.ContentPart{Type: "image_url", ImageURL: &types.ImageURL{URL: url}})
		default:
			return nil, false
		}
	}
	return parts, true
}

// ClearCache 清缓存（/drop 或 /reset 后调用；传 id 只清该条）
func (r *Resolver) ClearCache(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" {
		delete(r.cache, id)
		delete(r.stubCache, id)
	} else {
		r.cache = map[string][]types.ContentPart{}
		r.stubCache = map[string][]types.ContentPart{}
	}
}

// PersistDemotion 4xx 降级成功回写（2026-09-19）：侧车内容替换为文本提示。
// 降级本是请求级（Compose 变体，不动侧车）——对“持久坏附件”（内容损坏的图片，
// 每次 LLM 调用必 400），下轮请求原样恢复坏 part，每轮都先 4xx 再降级。
// 回写后后续 Resolve 拿到提示，根治。返回侧车是否存在并被改写。
func PersistDemotion(dir, id string) bool {
	p := filepath.Join(dir, id+".json")
	if _, err := os.Stat(p); err != nil {
		return false
	}
	stub := []types.ContentPart{{Type: "text", Text: fmt.Sprintf("附件 %s 因 LLM 4xx 降级为文本提示，原附件未发送", id)}}
	b, err := json.Marshal(stub)
	if err != nil {
		return false
	}
	return os.WriteFile(p, b, 0o644) == nil
}

// DropInjectFile 删侧车文件 + 清缓存；返回是否真删了文件
func DropInjectFile(dir, id string) bool {
	if err := os.Remove(filepath.Join(dir, id+".json")); err == nil {
		return true
	}
	return false
}

// InjectFileSize 侧车文件字节数（/attachments 列表展示用）
func InjectFileSize(dir, id string) int {
	fi, err := os.Stat(filepath.Join(dir, id+".json"))
	if err != nil {
		return 0
	}
	return int(fi.Size())
}
