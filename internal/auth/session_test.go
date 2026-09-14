package auth

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litepanel/internal/store"
)

// fakeClock 让过期测试不必真等 TTL（实施计划 1.3 / M6-T5 同源做法）。
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newSessionStore(t *testing.T) (*SessionStore, *fakeClock) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clk := &fakeClock{now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	return NewSessionStore(db, clk.Now, 7*24*time.Hour), clk
}

func TestSessionIssueAndValidate(t *testing.T) {
	s, _ := newSessionStore(t)
	tok, err := s.Issue("Mozilla/5.0", "100.64.0.5")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 32 {
		t.Fatalf("token 长度应 ≥32 字符, got %d", len(tok))
	}
	got, ok, err := s.Validate(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("刚签发的 token 应有效")
	}
	if got.LastIP != "100.64.0.5" || got.UserAgent != "Mozilla/5.0" {
		t.Fatalf("会话元信息不符: %+v", got)
	}
}

func TestValidateRejectsGarbage(t *testing.T) {
	s, _ := newSessionStore(t)
	for _, tok := range []string{"", "not-a-token", strings.Repeat("a", 64)} {
		if _, ok, err := s.Validate(tok); err != nil || ok {
			t.Fatalf("非法 token %q 应被拒绝 (ok=%v err=%v)", tok, ok, err)
		}
	}
}

func TestSessionExpiresAfterTTL(t *testing.T) {
	s, clk := newSessionStore(t)
	active, _ := s.Issue("ua", "ip")
	idle, _ := s.Issue("ua", "ip")

	clk.Advance(7*24*time.Hour - time.Minute)
	// active 被校验过一次，滑动续期后仍有效。
	if _, ok, err := s.Validate(active); err != nil || !ok {
		t.Fatalf("TTL 内应仍有效 (ok=%v err=%v)", ok, err)
	}
	// idle 从未被碰过，超过 TTL 后必须失效。
	clk.Advance(2 * time.Minute)
	if _, ok, err := s.Validate(idle); err != nil || ok {
		t.Fatalf("超过 TTL 后应失效 (ok=%v err=%v)", ok, err)
	}
}

// 滑动续期：每次校验通过都推后过期时间。
func TestSessionSlidesExpiry(t *testing.T) {
	s, clk := newSessionStore(t)
	tok, _ := s.Issue("ua", "ip")

	clk.Advance(6 * 24 * time.Hour)
	if _, ok, _ := s.Validate(tok); !ok {
		t.Fatal("第 6 天应仍有效")
	}
	clk.Advance(6 * 24 * time.Hour) // 距签发 12 天，但距上次校验 6 天
	if _, ok, _ := s.Validate(tok); !ok {
		t.Fatal("滑动续期后应仍有效")
	}
}

func TestRevokeAllInvalidatesEveryToken(t *testing.T) {
	s, _ := newSessionStore(t)
	var toks []string
	for i := 0; i < 3; i++ {
		tk, err := s.Issue("ua", "ip")
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, tk)
	}
	if err := s.RevokeAll(); err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if _, ok, _ := s.Validate(tk); ok {
			t.Fatal("RevokeAll 后 token 应失效")
		}
	}
}

func TestRevokeSingle(t *testing.T) {
	s, _ := newSessionStore(t)
	a, _ := s.Issue("ua", "ip")
	b, _ := s.Issue("ua", "ip")
	if err := s.Revoke(a); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Validate(a); ok {
		t.Fatal("被吊销的 token 应失效")
	}
	if _, ok, _ := s.Validate(b); !ok {
		t.Fatal("另一个 token 不应受影响")
	}
}

// 设计要求：DB 里只存 SHA-256，绝不存明文 token。
func TestTokenHashStoredNotPlaintext(t *testing.T) {
	s, clock := newSessionStore(t)
	tok, _ := s.Issue("ua", "10.0.0.1")

	// 直接查库：明文不该出现在任何地方。
	rows, err := s.db.SqlDB().Query(`SELECT token_hash, user_agent, last_ip FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var h, ip string
		var ua *string
		if err := rows.Scan(&h, &ua, &ip); err != nil {
			t.Fatal(err)
		}
		found = true
		if strings.Contains(h, tok) || h == tok {
			t.Fatal("数据库里出现了明文 token")
		}
		if len(h) != 64 {
			t.Fatalf("token_hash 应为 SHA-256 的 64 位十六进制, got %d", len(h))
		}
	}
	if !found {
		t.Fatal("sessions 表里应有记录")
	}
	_ = clock
}

// 过期记录要被清理，不能无限堆积。
func TestCleanupRemovesExpired(t *testing.T) {
	s, clk := newSessionStore(t)
	tok, _ := s.Issue("ua", "ip")
	clk.Advance(8 * 24 * time.Hour)

	n, err := s.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应清理 1 条过期会话, got %d", n)
	}
	var cnt int
	if err := s.db.SqlDB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Fatalf("过期会话应已从库里删除, 剩余 %d", cnt)
	}
	_ = tok
}

// TTL 可从设置改（这里直接构造不同 TTL 的 store）。
func TestTTLIsConfigurable(t *testing.T) {
	db := func(t *testing.T) *store.DB {
		d, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		return d
	}
	clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	s := NewSessionStore(db(t), clk.Now, time.Hour)
	tok, _ := s.Issue("ua", "ip")
	clk.Advance(2 * time.Hour)
	if _, ok, _ := s.Validate(tok); ok {
		t.Fatal("自定义 1 小时 TTL 应生效")
	}
}
