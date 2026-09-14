package auth

import (
	"errors"
	"path/filepath"
	"testing"

	"litepanel/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPasswordStoreRoundTrip(t *testing.T) {
	db := openDB(t)
	if _, err := CurrentPassword(db); !errors.Is(err, ErrNoPassword) {
		t.Fatalf("初始应报 ErrNoPassword, got %v", err)
	}
	if err := SetPassword(db, "hash-1"); err != nil {
		t.Fatal(err)
	}
	got, err := CurrentPassword(db)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hash-1" {
		t.Fatalf("got %q", got)
	}
	if err := SetPassword(db, "hash-2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := CurrentPassword(db); got != "hash-2" {
		t.Fatalf("覆盖失败, got %q", got)
	}
}

// 首次设置密码后必须标记需要改密（设计 5.7：一次性初始密码强制改密）。
func TestMustChangePasswordFlag(t *testing.T) {
	db := openDB(t)
	must, err := MustChangePassword(db)
	if err != nil {
		t.Fatal(err)
	}
	if !must {
		t.Fatal("无密码时应为 true")
	}
	if err := SetInitialPassword(db, "init1234"); err != nil {
		t.Fatal(err)
	}
	if must, _ := MustChangePassword(db); !must {
		t.Fatal("初始密码设置后仍应要求改密")
	}
	if err := ChangePassword(db, "new-password-8"); err != nil {
		t.Fatal(err)
	}
	if must, _ := MustChangePassword(db); must {
		t.Fatal("用户主动改密后不应再要求改密")
	}
}
