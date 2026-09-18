//go:build !debug

package main

import (
	"os"
	"runtime/debug"
)

// tuneRuntime 是发布构建的运行时资源默认值（见 runtime_release_test.go 的理由）。
// 只在 main 开头调一次；测试直接调用它来验证行为。
func tuneRuntime() {
	if _, ok := os.LookupEnv("GOGC"); !ok {
		debug.SetGCPercent(50)
	}
	if _, ok := os.LookupEnv("GOMEMLIMIT"); !ok {
		debug.SetMemoryLimit(64 << 20)
	}
}
