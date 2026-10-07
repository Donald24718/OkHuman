package ipython

// 内核池：按会话隔离 + 闲置回收。
//
// 为什么要隔离：ipython 的核心卖点是「变量跨调用保留」，而不同会话的
// 变量空间必须互不可见——否则 A 会话的 df 会被 B 会话看到，既是信息泄漏
// 也是难以排查的串扰。
//
// 为什么要回收：一个常驻 IPython 进程即便空转也占几十 MB 常驻内存，
// 长跑的 agent 若每来一个新 session 就留一个，进程数会单调增长。

import (
	"sort"
	"sync"
	"time"
)

// 内核池的两个水位。
const (
	// maxKernels 同时存活的内核数上限。超出时淘汰最久未使用的。
	// 这是防御性的：sessionKey 若来自外部输入，放任增长等于给内存放水。
	maxKernels = 8

	// idleTTL 闲置多久后关掉该内核（10 分钟不足以打断一次正常的多轮对话，
	// 但能让隔夜的会话不再占着进程）。
	idleTTL = 10 * time.Minute
)

type entry struct {
	k        *Kernel
	lastUsed time.Time
}

// KernelManager 一个 Go 进程内的全部 IPython 内核。
type KernelManager struct {
	mu      sync.Mutex
	kernels map[string]*entry
}

// NewManager 创建空内核池。
func NewManager() *KernelManager {
	return &KernelManager{kernels: make(map[string]*entry)}
}

// Get 取会话的内核；不存在则创建。
//
// key 通常来自 SetSessionKey 注入的会话标识；为空串时退化为「整个进程共享
// 一个内核」，对单会话部署形态是正确行为。
func (m *KernelManager) Get(key string) *Kernel {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.reapLocked(time.Now())

	if e, ok := m.kernels[key]; ok {
		e.lastUsed = time.Now()
		e.k.mu.Lock()
		alive := e.k.cmd != nil && !e.k.dead.Load()
		e.k.mu.Unlock()
		if alive {
			return e.k
		}
		// 进程已死（崩溃或上次硬杀）：丢弃该条目，下面重建。
		delete(m.kernels, key)
	}

	if len(m.kernels) >= maxKernels {
		m.evictOldestLocked()
	}

	k := NewKernel(pythonPath())
	m.kernels[key] = &entry{k: k, lastUsed: time.Now()}
	return k
}

// Recycle 作废会话的内核（硬杀后重建时用：进程已经没了，只是清账）。
func (m *KernelManager) Recycle(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.kernels[key]; ok {
		e.k.Shutdown()
		delete(m.kernels, key)
	}
}

// ShutdownAll 关闭全部内核（进程退出前的清理）。
func (m *KernelManager) ShutdownAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range m.kernels {
		e.k.Shutdown()
		delete(m.kernels, key)
	}
}

// reapLocked 关掉闲置超时的内核。必须在持锁时调用。
func (m *KernelManager) reapLocked(now time.Time) {
	for key, e := range m.kernels {
		if now.Sub(e.lastUsed) > idleTTL {
			e.k.Shutdown()
			delete(m.kernels, key)
		}
	}
}

// evictOldestLocked 淘汰最久未使用的内核（水位保护）。必须在持锁时调用。
func (m *KernelManager) evictOldestLocked() {
	var oldest string
	var oldestAt time.Time
	for key, e := range m.kernels {
		if oldest == "" || e.lastUsed.Before(oldestAt) {
			oldest, oldestAt = key, e.lastUsed
		}
	}
	if oldest != "" {
		m.kernels[oldest].k.Shutdown()
		delete(m.kernels, oldest)
	}
}

// Keys 当前存活的会话 key（调试/观测用，按字典序返回以保证确定性）。
func (m *KernelManager) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.kernels))
	for k := range m.kernels {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
