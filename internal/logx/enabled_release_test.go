//go:build !debug

package logx

import "testing"

// 发布构建（默认，无 debug 标签）：日志能力必须编译期为 false。
// 这是 D9/§12.1 的根：调用点是空操作、中间件挂载点被 DCE 整段消除，
// 全都以这个常量为依据。
func TestEnabledFalseInRelease(t *testing.T) {
	if Enabled {
		t.Fatal("release 构建下 logx.Enabled 必须为 false")
	}
}
