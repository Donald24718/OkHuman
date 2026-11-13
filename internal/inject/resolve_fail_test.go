// Resolve 的失败路径此前没有任何测试覆盖（把收口里的删除或回调改坏，全套测试仍绿）。
// 这里钉三件事：回给 LLM 一段说明文本、坏侧车被删掉、onFail 收到原因；
// 以及同一个 Resolver 再问一次时走缓存、不重复回调。
package inject

import (
	"okhuman/internal/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func textOf(p []types.ContentPart) string {
	var s string
	for _, q := range p {
		s += q.Text
	}
	return s
}

func TestResolveFailureCleansUpAndReports(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		expect string
	}{
		{"不是json", "{ 不是 json", "侧车内容损坏或为空"},
		{"空数组", "[]", "侧车内容损坏或为空"},
		{"空文件", "", "侧车内容损坏或为空"},
		{"没有这个侧车", "\x00缺文件", "侧车文件缺失"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			f := filepath.Join(d, "inj-9.json")
			if tc.body != "\x00缺文件" {
				if err := os.WriteFile(f, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var gotID, gotReason string
			r := NewResolver(d, func(id, reason string) { gotID, gotReason = id, reason })
			parts := r.Resolve("inj-9")
			if len(parts) != 1 {
				t.Fatalf("应回一段说明文本，实际 %d 段", len(parts))
			}
			txt := textOf(parts)
			if !strings.Contains(txt, "载入失败") || !strings.Contains(txt, tc.expect) {
				t.Errorf("说明文本不对: %q（应含 %q）", txt, tc.expect)
			}
			if _, err := os.Stat(f); !os.IsNotExist(err) {
				t.Errorf("坏侧车应被删掉，仍在: err=%v", err)
			}
			if gotID != "inj-9" || gotReason != tc.expect {
				t.Errorf("onFail 没收到正确信息: id=%q reason=%q（应为 inj-9 / %q）", gotID, gotReason, tc.expect)
			}
			// 再问一次：同一个 Resolver 走 stub 缓存，文本一致且不再回调
			calls := 0
			r.onFail = func(string, string) { calls++ }
			if again := textOf(r.Resolve("inj-9")); again != txt || calls != 0 {
				t.Errorf("第二次应回同一段说明且不再回调: 文本一致=%v 回调=%d", again == txt, calls)
			}
		})
	}
}
