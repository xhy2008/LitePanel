//go:build !debug

package main

import (
	"io"
	"log"
	"testing"

	"litepanel/internal/logx"
)

// main 的接线承诺（§12.1 / D9）：发布构建里标准 log 与 logx 双双静默。
// 启动横幅等诊断走标准 log 包，如果只把 logx 做成空操作而忘了
// log.SetOutput(io.Discard)，发布产物照样会一路打印 —— 这个测试
// 盯的就是 main 而不是包内部。
func TestMainWiringSilentInRelease(t *testing.T) {
	if logx.Enabled {
		t.Fatal("release 构建 logx.Enabled 应为 false")
	}
	wireLogging(false) // main 启动时执行的日志接线
	if log.Writer() != io.Discard {
		t.Errorf("发布构建标准 log 应指向 io.Discard, got %v", log.Writer())
	}
}
