//go:build debug

package auth

import "testing"

// 调试构建：初始密码固定为 "12345"。
// 用途：dev/run.sh 的 DEBUG=1 场景下，真机/集成测试不用从日志里
// 抠随机密码（它可能含 $、*、& 等 shell 会展开的字符，极难在脚本里
// 安全引用）。仅在 debug 构建生效 —— release 构建仍走 CSPRNG 强随机
// （见 TestInitialPasswordIsRandomAndStrong，只随 release 编译），
// 生产机的初始密码绝不可能是这个弱值。
func TestDebugBuildUsesFixedInitialPassword(t *testing.T) {
	if got := GenerateInitialPassword(); got != DebugInitialPassword {
		t.Fatalf("debug 构建初始密码应为固定值 %q, got %q", DebugInitialPassword, got)
	}
	// 固定值必须是纯数字、且能直接进 shell 脚本（无 shell 元字符）。
	for _, c := range DebugInitialPassword {
		if c < '0' || c > '9' {
			t.Fatalf("debug 固定密码应只含数字, got %q", DebugInitialPassword)
		}
	}
}
