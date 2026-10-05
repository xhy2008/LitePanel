package auth

import (
	"sync"
	"time"
)

// LoginLimiter 按 IP 统计登录失败次数（设计 5.7：默认 5 次锁 10 分钟）。
type LoginLimiter struct {
	mu       sync.Mutex
	now      func() time.Time
	maxFails int
	window   time.Duration
	entries  map[string]*failEntry
}

type failEntry struct {
	fails     int
	firstFail time.Time
	lockedU   time.Time // 锁定到期时间；零值表示未锁定
}

// NewLoginLimiter 创建限流器。maxFails/window 来自设置页。
func NewLoginLimiter(now func() time.Time, maxFails int, window time.Duration) *LoginLimiter {
	if now == nil {
		now = time.Now
	}
	if maxFails < 1 {
		maxFails = 5
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	return &LoginLimiter{now: now, maxFails: maxFails, window: window, entries: map[string]*failEntry{}}
}

// Allow 报告该 IP 现在是否还允许尝试登录。
func (l *LoginLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[ip]
	if e == nil {
		return true
	}
	if !e.lockedU.IsZero() {
		if l.now().Before(e.lockedU) {
			return false
		}
		// 锁定期已过：重新计。
		delete(l.entries, ip)
	}
	// 失败计数窗口滑出后清零。
	if l.now().Sub(e.firstFail) > l.window {
		delete(l.entries, ip)
	}
	return true
}

// Fail 记录一次失败，达到阈值即锁定。
func (l *LoginLimiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[ip]
	if e == nil || now.Sub(e.firstFail) > l.window {
		e = &failEntry{firstFail: now}
		l.entries[ip] = e
	}
	e.fails++
	if e.fails >= l.maxFails {
		e.lockedU = now.Add(l.window)
	}
}

// SetPolicy 热改锁定阈值与窗口（设置页）。
//
// 读 maxFails/window 的几处（Allowed/Fail/Prune）都已经持 l.mu，所以这里
// 直接在同一把锁下改即可，不需要额外的原子类型。
//
// 非法值回落到构造时的默认而不是拒绝：这个值来自库里可能留下的旧数字，
// 而"锁定阈值被设成 0 = 任何人都能无限次试密码"是安全问题，绝不能因为
// 库里一个坏数字就放行；同样窗口也不能是 0。与 NewLoginLimiter 的兜底一致。
//
// 注意：改阈值**不会**清掉已有的失败计数。已经处于锁定中的 IP 仍按它当初
// 被锁时算出的 lockedU 到期——把窗口调小就立刻解锁所有正在被爆破的 IP，
// 是这条设置最不想要的副作用。
func (l *LoginLimiter) SetPolicy(maxFails int, window time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if maxFails < 1 {
		maxFails = 5
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	l.maxFails = maxFails
	l.window = window
}

// Reset 在登录成功后清零该 IP 的计数。
func (l *LoginLimiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

// RetryAfter 返回锁定剩余时间；ok=false 表示当前未锁定。
func (l *LoginLimiter) RetryAfter(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[ip]
	if e == nil || e.lockedU.IsZero() {
		return 0, false
	}
	left := e.lockedU.Sub(l.now())
	if left <= 0 {
		return 0, false
	}
	return left, true
}

// Prune 清掉所有已失效条目，避免伪造 IP 把内存撑爆。
func (l *LoginLimiter) Prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for ip, e := range l.entries {
		staleWindow := now.Sub(e.firstFail) > l.window
		if e.lockedU.IsZero() {
			if staleWindow {
				delete(l.entries, ip)
			}
			continue
		}
		if !e.lockedU.After(now) && staleWindow {
			delete(l.entries, ip)
		}
	}
}

// Size 返回当前记录的 IP 数（供测试与设置页观测）。
func (l *LoginLimiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
