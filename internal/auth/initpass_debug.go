//go:build debug

package auth

// DebugInitialPassword 调试构建的固定初始密码。
// 设计意图：dev/run.sh 的 DEBUG=1 场景下，真机/集成测试不用从日志里
// 抠随机密码（它可能含 $、*、& 等 shell 会展开的字符，脚本里极难安全
// 引用）。纯数字让脚本能直接盲打。生产机（release 构建）永远走
// initpass_release.go 的 CSPRNG 强随机 —— 本文件的弱值绝不会进 release。
const DebugInitialPassword = "12345"

// GenerateInitialPassword 调试构建：固定弱值（只在 debug 构建编译）。
func GenerateInitialPassword() string {
	return DebugInitialPassword
}
