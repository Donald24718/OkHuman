// 锁序守卫（源码形状检查，不是并发测试）。
//
// 反向边是活的：Manager.Compose 持 context 管理器的锁 → 解析附件 → inject 侧车不可用时
// 回调 onFail，而 onFail 取 AppState 的 state.mu（reconfigureResolver）。所以 server 这边
// 任何"持 state.mu 再去看会话"的写法都会和它凑成死锁环，且没有超时可救。
// 2026-11-13 四方审计就是在 broadcastEvent 里抓到了这样一处（StartLen 在锁内读会话）。
package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNoSessionReadUnderStateMu(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	lockRe := regexp.MustCompile(`state\.mu\.Lock\(\)`)
	unlockRe := regexp.MustCompile(`state\.mu\.Unlock\(\)`)
	takenRe := regexp.MustCompile(`\.CM\(\)|\.Session\(\)`)
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		inLock := false
		for i, line := range strings.Split(string(src), "\n") {
			if !inLock {
				if lockRe.MatchString(line) {
					inLock = true
				}
				continue
			}
			if takenRe.MatchString(line) {
				bad = append(bad, f+":"+itoa(i+1)+" 持 state.mu 时取会话："+strings.TrimSpace(line))
			}
			if unlockRe.MatchString(line) {
				inLock = false
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("需要在取 state.mu 之前先把会话读完（否则与 inject onFail 反向成环）：\n  %s", strings.Join(bad, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
