//go:build debug

package main

import "testing"

// 调试构建不动任何运行时参数：排障时要的是可观察性（pprof、低 GC
// 停顿带来的时序稳定），资源压榨反而添乱。
func TestDebugRuntimeUntouched(t *testing.T) {
	before := gcPercent()
	tuneRuntime()
	if got := gcPercent(); got != before {
		t.Errorf("调试构建不该动 GOGC: %d → %d", before, got)
	}
}
