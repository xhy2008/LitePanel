//go:build debug

package logx

import "testing"

// 调试构建：日志能力必须为 true，且级别可调。
func TestEnabledTrueInDebug(t *testing.T) {
	if !Enabled {
		t.Fatal("debug 构建下 logx.Enabled 必须为 true")
	}
}
