package context

import (
	"testing"

	"okhuman/internal/llm"
	"okhuman/internal/types"
)

// 注销只影响自己这一条。
//
// 旧实现用 reflect.ValueOf(fn).Pointer() 判断"是不是同一个监听器"。实测（本机 go1.24，
// 2026-11-12）：同一个函数字面量产生的两个闭包会被编译器克隆、代码指针**不同** —— 所以
// 第一版用"两个闭包"这种写法在旧实现下也过，测不出问题（靠变异检验才发现）（我第一版就是这样，小邻做变异检验才戳穿）。
// 真会撞的是：**同一个 func 值注册两次**、以及**同一个方法取两次方法值**（x.H）——它们的
// 代码指针相同，旧实现注销其一会把另一条一起删掉。下面两组是钉得住的形状。
func newListenerManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(llm.NewFakeLLM(nil), Cfg{CharsPerToken: 1.5}, "sys", t.TempDir())
}

// 形状 A：同一个 func 值注册两次。
func TestUnregisterSameValueTwiceKeepsTheOther(t *testing.T) {
	m := newListenerManager(t)
	var hits int
	f := func(*types.SessionState, int) { hits++ }
	cancelA := m.AddPersistListener(f)
	m.AddPersistListener(f) // 同一个值

	cancelA()
	m.emitPersist()
	if hits != 1 {
		t.Fatalf("注销一条后另一条必须还在：hits=%d，期望 1（旧 reflect 实现会给 0）", hits)
	}
}

// 形状 B：同一个方法的两次方法值。
type counter struct{ n int }

func (c *counter) H(*types.SessionState, int) { c.n++ }

func TestUnregisterMethodValueKeepsTheOther(t *testing.T) {
	m := newListenerManager(t)
	c := &counter{}
	cancelA := m.AddPersistListener(c.H)
	m.AddPersistListener(c.H)

	cancelA()
	m.emitPersist()
	if c.n != 1 {
		t.Fatalf("注销一个方法值后另一个必须还在：n=%d，期望 1（旧 reflect 实现会给 0）", c.n)
	}
}

// 重复注销幂等，且不影响别人。
func TestDoubleUnregisterIsIdempotent(t *testing.T) {
	m := newListenerManager(t)
	var hits int
	cancel := m.AddCompressDeltaListener(func(string, types.Delta) { hits++ })
	dup := m.AddCompressDeltaListener(func(string, types.Delta) { hits += 10 })
	cancel()
	cancel()
	m.emitCompressDelta("history", types.Delta{})
	if hits != 10 {
		t.Fatalf("重复注销影响了别人：hits=%d（期望 10）", hits)
	}
	dup()
	m.emitCompressDelta("history", types.Delta{})
	if hits != 10 {
		t.Fatalf("两条都注销后不该再有回调：%d", hits)
	}
}
