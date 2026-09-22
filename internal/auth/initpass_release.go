//go:build !debug

package auth

import (
	"crypto/rand"
	"math/big"
)

const (
	initialPasswordLen = 20

	// 初始密码字符集去掉了易混淆的字符（0/O、1/l/I）。
	setLower = "abcdefghijkmnopqrstuvwxyz"
	setUpper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	setDigit = "23456789"
	setSym   = "!@#$%^&*-_=+"
)

// GenerateInitialPassword 生成一次性初始密码（release 构建）：
// 保证覆盖 4 类字符、长度 ≥16、 CSPRNG 随机。
// 调试构建不经过这里 —— 见 initpass_debug.go 里的固定弱值。
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
