package auth

import (
	"regexp"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if h == "correct horse battery staple" {
		t.Fatal("哈希结果不能是明文")
	}
	if err := CheckPassword(h, "correct horse battery staple"); err != nil {
		t.Fatalf("正确密码应通过校验: %v", err)
	}
}

func TestWrongPasswordRejected(t *testing.T) {
	h, _ := HashPassword("s3cret-passw0rd!")
	if err := CheckPassword(h, "wrong"); err == nil {
		t.Fatal("错误密码应校验失败")
	}
	// 损坏的哈希不应 panic，应返回错误。
	if err := CheckPassword("not-a-bcrypt-hash", "x"); err == nil {
		t.Fatal("损坏的哈希应报错")
	}
}

// cost=12 在目标机上是百毫秒级；太快说明 cost 被调低了。
func TestHashCostIsTwelve(t *testing.T) {
	start := time.Now()
	h, err := HashPassword("timing-check-1234")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if got := CostOf(h); got != 12 {
		t.Fatalf("bcrypt cost 应为 12, got %d", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("哈希耗时异常: %v", elapsed)
	}
	t.Logf("cost=12 耗时 %v", elapsed)
}

func TestInitialPasswordIsRandomAndStrong(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		p := GenerateInitialPassword()
		if len(p) < 16 {
			t.Fatalf("初始密码长度应 ≥16, got %d (%q)", len(p), p)
		}
		classes := 0
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(`[a-z]`), regexp.MustCompile(`[A-Z]`),
			regexp.MustCompile(`[0-9]`), regexp.MustCompile(`[^a-zA-Z0-9]`),
		} {
			if re.MatchString(p) {
				classes++
			}
		}
		if classes < 3 {
			t.Fatalf("初始密码应至少包含 3 类字符, got %d 类 (%q)", classes, p)
		}
		if seen[p] {
			t.Fatalf("两次生成的初始密码相同: %q", p)
		}
		seen[p] = true
	}
}
