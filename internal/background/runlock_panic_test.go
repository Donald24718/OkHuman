package background

import (
	"errors"
	"testing"
	"time"
)

// C（2026-10-18）：fn panic 后运行锁必须归还——否则实例永久不回话。
func TestRunLockReleasedAfterPanic(t *testing.T) {
	l := NewRunLock()
	done := make(chan struct{})
	go func() {
		defer func() { _ = recover(); close(done) }()
		_ = l.Run(func() error { panic("boom") })
	}()
	<-done
	if l.Running() {
		t.Fatal("panic 后 Running() 仍为 true（锁未释放）")
	}
	res := make(chan error, 1)
	go func() { res <- l.Run(func() error { return errors.New("next") }) }()
	select {
	case err := <-res:
		if err == nil || err.Error() != "next" {
			t.Fatalf("第二次 Run 返回异常：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("panic 后第二次 Run 阻塞（令牌未归还）")
	}
}

// panic 时 idleWaiters 也必须被唤醒（否则 WaitIdle 永久挂住）。
func TestRunLockWaitIdleWokenAfterPanic(t *testing.T) {
	l := NewRunLock()
	go func() {
		defer func() { _ = recover() }()
		_ = l.Run(func() error { panic("boom") })
	}()
	time.Sleep(50 * time.Millisecond) // 让 run 起来
	done := make(chan struct{})
	go func() { l.WaitIdle(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("panic 后 WaitIdle 未被唤醒")
	}
}
