package auth

import (
	"path/filepath"
	"testing"
	"time"

	"litepanel/internal/store"
)

func newTTLStore(t *testing.T, ttl time.Duration) (*SessionStore, *fakeClock) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := &fakeClock{now: time.Unix(1700000000, 0)}
	return NewSessionStore(db, clk.Now, ttl), clk
}

// 新会话按当前 TTL 落库。
func TestSessionTTLUsedOnIssue(t *testing.T) {
	s, clk := newTTLStore(t, 2*time.Hour)
	tok, err := s.Issue("ua", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := s.Validate(tok)
	if err != nil || !ok {
		t.Fatalf("刚签发的会话应有效: %v %v", ok, err)
	}
	// 过了 TTL 之内仍然有效、过了就失效（按当前值判定）。
	clk.now = clk.now.Add(90 * time.Minute)
	if _, ok, _ := s.Validate(tok); !ok {
		t.Error("90 分钟时（TTL 2h）应仍有效")
	}
	clk.now = clk.now.Add(45 * time.Hour) // 远超 TTL
	if _, ok, _ := s.Validate(tok); ok {
		t.Error("远超 TTL 后会话应失效")
	}
}

// 改 TTL 必须让**没有在用的既有会话**也立刻收紧。
//
// Validate 每次都会按当前 TTL 滑动续期，所以"正在被使用"的会话会自然收敛；
// 但没人访问的会话只看它落库时的 expires_at。用户把有效期从 30 天调到 1 天
// 的动机往往正是"怀疑别人握着我的登录态"——那种场合下"只有对方在线时才
// 生效"等于没生效，所以这条不只是洁癖。
func TestSetTTLTightensIdleSessions(t *testing.T) {
	s, clk := newTTLStore(t, 30*24*time.Hour)
	tok, err := s.Issue("ua", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	// 时间推进到第 5 天：30 天有效期下仍有效。
	clk.now = clk.now.Add(5 * 24 * time.Hour)
	if _, ok, _ := s.Validate(tok); !ok {
		t.Fatal("前置条件：应先有效")
	}
	// 让它变成"闲置"状态：不再访问它，直接把时间推到第 6 天。
	clk.now = clk.now.Add(24 * time.Hour)

	n, err := s.SetTTL(1 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应报告收紧了 1 条既有会话，得 %d", n)
	}
	// 第 6 天 + 1 小时之后再校验：必须已失效。
	clk.now = clk.now.Add(2 * time.Hour)
	if _, ok, _ := s.Validate(tok); ok {
		t.Error("调到 1 小时后，闲置的旧会话必须立刻失效（否则这条设置形同虚设）")
	}
}

// 调大 TTL 不得让已过期的会话复活，也不得延长既有会话的寿命。
// 否则"改大设置"会变成一种提权路径（把已踢掉的会话再放回来）。
func TestSetTTLNeverExtends(t *testing.T) {
	s, _ := newTTLStore(t, 1*time.Hour)
	tok, _ := s.Issue("ua", "1.2.3.4")
	// 先把它调到很大（在过期之前）：既有会话的 expires_at 不该被推后。
	if _, err := s.SetTTL(365 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	// 落库的过期时间应仍是"签发时刻 + 1h"那一版（SetTTL 只收紧不延长）。
	var expires int64
	if err := s.db.SqlDB().QueryRow(`SELECT expires_at FROM sessions`).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	wantMax := time.Unix(1700000000, 0).Add(1 * time.Hour).Unix()
	if expires > wantMax {
		t.Errorf("调大 TTL 把既有会话续到了 %d，超过原来的 %d", expires, wantMax)
	}
	_ = tok
}

// 非法 TTL 报错而不是悄悄接受。
// "0 = 永不过期"这个语义绝不能出现在会话有效期上。
func TestSetTTLRejectsNonPositive(t *testing.T) {
	s, _ := newTTLStore(t, time.Hour)
	for _, bad := range []time.Duration{0, -time.Second} {
		if _, err := s.SetTTL(bad); err == nil {
			t.Errorf("TTL %v 应被拒", bad)
		}
	}
	// 被拒时当前 TTL 不该被改坏。
	if got := s.TTL(); got != time.Hour {
		t.Errorf("非法调用不应改动 TTL，现在是 %v", got)
	}
}

// 坏值（库里留下的 0/负数）读取时回落到安全默认。
// 两个方向都不能选：0 会让所有会话立即失效（把用户当场踢下线，他还以为
// 密码错了），负数/无穷大则是"永不过期"这个反向安全漏洞。
func TestSessionTTLInvalidFallsBack(t *testing.T) {
	s, _ := newTTLStore(t, time.Hour)
	s.ttlNanos.Store(0)
	if got := s.TTL(); got != 30*24*time.Hour {
		t.Errorf("0 应回落到 30 天默认，得 %v", got)
	}
	s.ttlNanos.Store(-1)
	if got := s.TTL(); got != 30*24*time.Hour {
		t.Errorf("负值应回落到默认，得 %v", got)
	}
}

// 改锁定阈值后，新的失败按新阈值锁定。
func TestLimiterSetPolicy(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1700000000, 0)}
	l := NewLoginLimiter(clk.Now, 5, 10*time.Minute)
	// 先来 2 次失败：旧阈值 5 下不该锁。
	l.Fail("1.1.1.1")
	l.Fail("1.1.1.1")
	if ok := l.Allow("1.1.1.1"); !ok {
		t.Fatal("前置条件：2 次失败不应锁定")
	}
	// 把阈值调到 3：第 3 次失败就该锁。
	l.SetPolicy(3, 10*time.Minute)
	l.Fail("1.1.1.1")
	if ok := l.Allow("1.1.1.1"); ok {
		t.Error("阈值调到 3 之后，第 3 次失败应锁定")
	}
}

// 已有的失败计数**不能**因为改设置而被清掉。
// 反过来做等于"把窗口调小就解锁所有正在被爆破的 IP"，正是最糟的时机。
func TestLimiterSetPolicyKeepsFailures(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1700000000, 0)}
	l := NewLoginLimiter(clk.Now, 3, 10*time.Minute)
	l.Fail("2.2.2.2")
	l.Fail("2.2.2.2")
	l.SetPolicy(5, 30*time.Minute)
	l.Fail("2.2.2.2") // 累计 3 次；新阈值 5 下不锁，但计数必须是 3 而不是 1
	if ok := l.Allow("2.2.2.2"); !ok {
		t.Fatal("阈值 5 下 3 次失败不该锁")
	}
	l.Fail("2.2.2.2")
	l.Fail("2.2.2.2") // 第 5 次
	if ok := l.Allow("2.2.2.2"); ok {
		t.Error("累计到 5 次应锁定（说明计数被保留了下来）")
	}
}

// 非法阈值回落到默认而不是放行。
// 阈值为 0 的字面意思是"允许 0 次失败"，但任何把它当"无限制"解读的实现
// 都会变成"任何人都能无限试密码"——安全项的兜底必须朝更安全的一侧。
func TestLimiterSetPolicyInvalid(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1700000000, 0)}
	l := NewLoginLimiter(clk.Now, 3, time.Minute)
	l.SetPolicy(0, 0)
	// 兜底成 5 次：第 4 次失败不应锁。
	for i := 0; i < 4; i++ {
		l.Fail("3.3.3.3")
	}
	if ok := l.Allow("3.3.3.3"); !ok {
		t.Error("非法阈值应兜底成默认 5 次")
	}
	for i := 0; i < 1; i++ {
		l.Fail("3.3.3.3")
	}
	if ok := l.Allow("3.3.3.3"); ok {
		t.Error("第 5 次应锁")
	}
}
