// 会话文件"不存在"与"存在但读不动"必须分得开（2026-11-13 四方审计 R4/#15）：
// 后者以前也回 (nil,nil) → 整个会话静默归零，日志里一行都没有。
package persist

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSessionFileDistinguishesMissingFromCorrupt(t *testing.T) {
	dir := t.TempDir()
	if s, err := LoadSessionFile(dir); s != nil || err != nil {
		t.Errorf("无文件应回 (nil,nil)，实际 s=%v err=%v", s, err)
	}
	cases := map[string]string{
		"半截 JSON":      `{"no":1,"seq":2,"state":{"messages":`,
		"缺 state":      `{"no":1,"seq":2}`,
		"messages 非数组": `{"no":1,"seq":2,"state":{"messages":{}}}`,
		"role 不是字符串":   `{"no":1,"seq":2,"state":{"messages":[{"role":5,"content":"x"}]}}`,
	}
	for name, body := range cases {
		if err := os.WriteFile(filepath.Join(dir, SessionFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := LoadSessionFile(dir)
		if s != nil || err == nil {
			t.Errorf("%s：应回 (nil,error)，实际 s=%v err=%v", name, s, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, SessionFile),
		[]byte(`{"no":3,"seq":7,"state":{"messages":[{"role":"user","content":"在"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSessionFile(dir)
	if err != nil || s == nil || s.No != 3 || len(s.State.Messages) != 1 {
		t.Errorf("正常文件应能加载：s=%v err=%v", s, err)
	}
}
