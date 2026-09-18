//go:build debug

package main

// tuneRuntime 在调试构建里是显式空操作：排障时要的是可观察性，
// 不是资源压榨（pprof、GC 时序都保持工具链默认行为）。
func tuneRuntime() {}
