// Package auth 负责单用户密码、会话与认证中间件。
package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

const (
	// BcryptCost 设计 5.7 节约定 cost=12。
	BcryptCost = 12
)

// HashPassword 返回 bcrypt 哈希。
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword 校验明文与哈希是否匹配，不匹配返回错误。
func CheckPassword(hash, password string) error {
	if hash == "" {
		return errors.New("尚未设置密码")
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if err != nil {
		return errors.New("密码错误")
	}
	return nil
}

// CostOf 返回哈希中编码的 bcrypt cost，解析失败返回 0。
func CostOf(hash string) int {
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return 0
	}
	return cost
}
