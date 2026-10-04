//go:build windows

package tools

import "testing"

// TestIsSignaledExitThreeSources 锁定 isSignaledExit 的三源判定（阶段 3 实测修正）。
//
// 本测试是"变异测试抓得住"的守卫：若有人把三源退化为旧式 `code >= 0x80000000`，
// 表里 Git Bash 信号码与 terminatedByUsCode 两行会立即失败。
func TestIsSignaledExitThreeSources(t *testing.T) {
	cases := []struct {
		name string
		code int
		want bool
	}{
		// 自然退出：0..255，全部不算"死于信号"。
		{"自然退出 0", 0, false},
		{"自然退出 1", 1, false},
		{"自然退出 42", 42, false},
		{"自然退出 255", 255, false},

		// ① 高位 NTSTATUS。
		{"Ctrl-C 0xC000013A", 0xC000013A, true},
		{"崩溃 0xC0000005", 0xC0000005, true},
		{"高位边界 0x80000000", 0x80000000, true},

		// ② Git Bash 信号码 signo<<8（探针 P6b 实测）。
		{"SIGINT(2)→0x200", 0x200, true},
		{"SIGKILL(9)→0x900", 0x900, true},
		{"SIGSEGV(11)→0xB00", 0xB00, true},
		{"SIGTERM(15)→0xF00", 0xF00, true},
		{"信号边界 signo=64→0x4000", 0x4000, true},

		// ② 的**反例**：低字节非零 → 不是信号码（不可由 kill 产生）。
		{"低字节非零 0x901", 0x901, false},
		{"低字节非零 0x101", 0x101, false},
		{"低字节非零 0xFF", 0xFF, false},
		// ② 的**反例**：signo 越界（>64）。
		{"signo 越界 0x4100", 0x4100, false},

		// ③ 我们的专属终止码（唯一不可伪造）。
		{"terminatedByUsCode", terminatedByUsCode, true},

		// 等待失败哨兵。
		{"等待失败 0xFFFFFFFF", -1, true}, // int(-1) → uint32 0xFFFFFFFF
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSignaledExit(c.code); got != c.want {
				t.Errorf("isSignaledExit(%#x) = %v, want %v", uint32(c.code), got, c.want)
			}
		})
	}
}

// TestTerminatedByUsCodeNotForgeable 用真实 bash 验证 65535 无法被脚本伪造。
// 这是"三源"设计的地基：若脚本能造 65535，陷阱①②就会被绕过。
func TestTerminatedByUsCodeNotForgeable(t *testing.T) {
	// exit 65535 应被 bash 截断到低 8 位（255），而非 65535。
	info, _, _ := runReal(t, "#!/bin/bash\nexit 65535\n", 10)
	if uint32(info.Code) == terminatedByUsCode {
		t.Fatalf("exit 65535 竟然得到 terminatedByUsCode（截断失效，哨兵可被伪造）: %+v", info)
	}
	if info.Code != 255 {
		t.Errorf("exit 65535 期望被截断为 255，得 %d", info.Code)
	}
	if info.Signaled {
		t.Errorf("exit 65535 不应被判为 signaled: %+v", info)
	}

	// exit 2304（=0x900，SIGKILL 的信号码）应被截断为 0，不可伪造信号码。
	info2, _, _ := runReal(t, "#!/bin/bash\nexit 2304\n", 10)
	if info2.Signaled {
		t.Errorf("exit 2304 不应被判为 signaled（截断后为 0）: %+v", info2)
	}
}

// TestKillSelfIsNotTimeout 陷阱②守卫：命令自杀（kill -9 $$）必须 signaled=true
// 但 killed=false → 不报超时（这是双条件存在的理由之一）。
//
// 注意：本用例走 fakeExecutor 无法覆盖（isSignaledExit 是平台真实逻辑），
// 故用真实 bash。它是"退出码 0x900 → signaled"这一路径的端到端证据。
func TestKillSelfIsNotTimeout(t *testing.T) {
	info, _, _ := runReal(t, "#!/bin/bash\nkill -9 $$\n", 10)
	t.Logf("info=%+v", info)
	if !info.Signaled {
		t.Errorf("kill -9 $$ 应 signaled=true（退出码 0x900）: %+v", info)
	}
	if info.Killed {
		t.Errorf("命令自杀时 killed 应为 false（我们没杀它）: %+v", info)
	}
	if info.Reason == ExitTimedOut {
		t.Errorf("命令自杀不得报超时: %+v", info)
	}
}

// TestTerminateKillCodeSentinel 锁定"我们杀活进程 → 退出码为 terminatedByUsCode"。
// 这是陷阱①与真超时可区分的地基（见 isSignaledExit 注释）。
func TestTerminateKillCodeSentinel(t *testing.T) {
	info, _, _ := runReal(t, "#!/bin/bash\nsleep 300 &\nsleep 300\n", 2)
	t.Logf("info=%+v", info)
	if uint32(info.Code) != terminatedByUsCode {
		t.Errorf("超时杀灭后退出码应为哨兵 %#x，得 %#x", terminatedByUsCode, uint32(info.Code))
	}
	if !info.Killed || !info.Signaled {
		t.Errorf("真超时应 killed+signaled: %+v", info)
	}
	if info.Reason != ExitTimedOut {
		t.Errorf("期望 Reason=timedout，得 %s", info.Reason)
	}
	if info.Degraded != "" {
		t.Errorf("级 1 预绑定应可用，不应有降级说明: %q", info.Degraded)
	}
}
