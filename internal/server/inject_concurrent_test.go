// 并发 /inject 不得互相抹掉：把早前扫描的快照回写，会抹掉别人刚加的附件（第四轮 A2）。
package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"okhuman/internal/config"
	"okhuman/internal/llm"
)

func TestConcurrentInjectKeepsAll(t *testing.T) {
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	a := CreateAgentState(cfg, llm.NewFakeLLM(nil), "sys", []PromptFileInfo{}, true, "")
	st := NewAppState(cfg, t.TempDir(), a)
	ap := New(st, nil)
	srv := httptest.NewServer(ap)
	defer srv.Close()

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"content":[{"type":"text","text":"附件内容 %d"}]}`, i)
			r := httptest.NewRequest("POST", srv.URL+"/inject", strings.NewReader(body))
			srv.Config.Handler.ServeHTTP(httptest.NewRecorder(), r)
		}(i)
	}
	wg.Wait()
	st.mu.Lock()
	got := len(st.Injects)
	st.mu.Unlock()
	if got != n {
		t.Errorf("%d 个并发注入后列表应为 %d 条，实际 %d 条", n, n, got)
	}
}
