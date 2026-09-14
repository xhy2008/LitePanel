// Package auth 负责单用户密码、会话与认证中间件。
package auth

import (
	"crypto/rand"
	"errors"
	"math/big"

	"golang.org/x/crypto/bcrypt"
)

const (
	// BcryptCost 设计 5.7 节约定 cost=12。
	BcryptCost = 12

	initialPasswordLen = 20
)

// 初始密码字符集去掉了易混淆的字符（0/O、1/l/I）。
const (
	setLower = "abcdefghijkmnopqrstuvwxyz"
	setUpper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	setDigit = "23456789"
	setSym   = "!@#$%^&*-_=+"
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

// GenerateInitialPassword 生成一次性初始密码：
// 保证覆盖 4 类字符、长度 ≥16、 CSPRNG 随机。
func GenerateInitialPassword() string {
	sets := []string{setLower, setUpper, setDigit, setSym}
	out := make([]byte, 0, initialPasswordLen)
	// 先每类取一个，保证字符类数量。
	for _, s := range sets {
		out = append(out, s[mustRand(len(s))])
	}
	all := setLower + setUpper + setDigit + setSym
	for len(out) < initialPasswordLen {
		out = append(out, all[mustRand(len(all))])
	}
	// Fisher–Yates 打散，避免前四位固定为“每类一个”的模式。
	for i := len(out) - 1; i > 0; i-- {
		j := mustRand(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func mustRand(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// CSPRNG 不可用时没有安全的回退，直接 panic 比生成弱密码好。
		panic("auth: CSPRNG 不可用: " + err.Error())
	}
	return int(v.Int64())
}
