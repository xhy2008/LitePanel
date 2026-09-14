package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestLoginLimiterLocksAfterFiveFailures(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	l := NewLoginLimiter(clk.Now, 5, 10*time.Minute)

	for i := 0; i < 5; i++ {
		if !l.Allow("10.0.0.1") {
			t.Fatalf("第 %d 次尝试前应允许", i+1)
		}
		l.Fail("10.0.0.1")
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("5 次失败后应锁定")
	}
	if _, ok := l.RetryAfter("10.0.0.1"); !ok {
		t.Fatal("锁定时应能查到剩余时间")
	}
	// 其他 IP 不受影响。
	if !l.Allow("10.0.0.2") {
		t.Fatal("锁定不应影响其他 IP")
	}
}

func TestLoginLimiterRecoversAfterWindow(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	l := NewLoginLimiter(clk.Now, 5, 10*time.Minute)
	for i := 0; i < 5; i++ {
		l.Fail("10.0.0.1")
	}
	clk.Advance(9 * time.Minute)
	if l.Allow("10.0.0.1") {
		t.Fatal("锁定窗口未过，仍应拒绝")
	}
	clk.Advance(2 * time.Minute)
	if !l.Allow("10.0.0.1") {
		t.Fatal("锁定窗口过后应恢复")
	}
}

// 成功登录要清零失败计数，否则长期会误锁正常用户。
func TestLoginLimiterResetOnSuccess(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	l := NewLoginLimiter(clk.Now, 5, 10*time.Minute)
	for i := 0; i < 4; i++ {
		l.Fail("10.0.0.1")
	}
	l.Reset("10.0.0.1")
	for i := 0; i < 5; i++ {
		l.Fail("10.0.0.1")
	}
	clk.Advance(time.Second)
	if l.Allow("10.0.0.1") {
		t.Fatal("重置后再次累计 5 次应重新锁定")
	}
}

// 失败计数不应因大量伪造 IP 而无限增长。
func TestLoginLimiterPrunesStaleEntries(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	l := NewLoginLimiter(clk.Now, 5, 10*time.Minute)
	for i := 0; i < 500; i++ {
		l.Fail(ipOf(i))
	}
	clk.Advance(11 * time.Minute)
	l.Prune()
	if got := l.Size(); got != 0 {
		t.Fatalf("过期条目应被清理, 剩余 %d", got)
	}
}

func ipOf(i int) string {
	return fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256)
}
