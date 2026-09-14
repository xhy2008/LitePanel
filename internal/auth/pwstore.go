package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"litepanel/internal/store"
)

// ErrNoPassword 表示尚未设置任何密码（首次启动场景）。
var ErrNoPassword = errors.New("尚未设置密码")

const (
	keyPasswordHash = "password_hash"
	keyMustChangePw = "must_change_password"
)

// CurrentPassword 返回密码哈希；未设置时返回 ErrNoPassword。
func CurrentPassword(db *store.DB) (string, error) {
	var v string
	err := db.SqlDB().QueryRow(`SELECT value FROM settings WHERE key=?`, keyPasswordHash).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoPassword
	}
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", ErrNoPassword
	}
	return v, nil
}

// SetPassword 直接写入哈希。
func SetPassword(db *store.DB, hash string) error {
	return upsert(db, keyPasswordHash, hash)
}

// SetInitialPassword 写入一次性初始密码并标记需要改密。
func SetInitialPassword(db *store.DB, plain string) error {
	hash, err := HashPassword(plain)
	if err != nil {
		return err
	}
	if err := SetPassword(db, hash); err != nil {
		return err
	}
	return upsert(db, keyMustChangePw, "1")
}

// ChangePassword 是用户主动改密：写入新哈希并清除改密标记。
func ChangePassword(db *store.DB, plain string) error {
	if len(plain) < 8 {
		return errors.New("新密码至少 8 位")
	}
	hash, err := HashPassword(plain)
	if err != nil {
		return err
	}
	if err := SetPassword(db, hash); err != nil {
		return err
	}
	return upsert(db, keyMustChangePw, "0")
}

// MustChangePassword 报告当前是否处于“初始密码尚未更换”状态。
func MustChangePassword(db *store.DB) (bool, error) {
	set, err := PasswordIsSet(db)
	if err != nil {
		return false, err
	}
	if !set {
		return true, nil
	}
	var v string
	err = db.SqlDB().QueryRow(`SELECT value FROM settings WHERE key=?`, keyMustChangePw).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

func PasswordIsSet(db *store.DB) (bool, error) {
	_, err := CurrentPassword(db)
	if errors.Is(err, ErrNoPassword) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func upsert(db *store.DB, key, value string) error {
	_, err := db.SqlDB().Exec(
		`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("写入设置 %s: %w", key, err)
	}
	return nil
}
