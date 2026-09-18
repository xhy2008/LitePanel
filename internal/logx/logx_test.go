package logx

import (
	"bytes"
	"strings"
	"testing"
)

// 这个文件对两种构建标签都必须通过。它断言的是"行为与 Enabled 一致"，
// 而 Enabled 本身由 tag 专属测试文件钉死（enabled_release_test.go /
// enabled_debug_test.go）。

func TestEmitConsistentWithEnabled(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf)

	Debug("d-%d", 1)
	Info("i-%d", 2)
	Warn("w-%d", 3)
	Error("e-%d", 4)

	out := buf.String()
	if Enabled {
		for _, want := range []string{"d-1", "i-2", "w-3", "e-4"} {
			if !strings.Contains(out, want) {
				t.Errorf("debug 构建应含 %q, got %q", want, out)
			}
		}
		return
	}
	if out != "" {
		t.Errorf("release 构建必须零输出, got %q", out)
	}
}

// release 构建里 Init/SetLevel 必须仍可调用（业务代码不分叉），只是无效。
func TestInitSetLevelAlwaysCallable(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf)
	SetLevel(LevelWarn)
	Info("suppressed-if-warn-level")
	if Enabled && strings.Contains(buf.String(), "suppressed") {
		t.Error("SetLevel(Warn) 后 Info 不应输出")
	}
	if !Enabled && buf.Len() != 0 {
		t.Errorf("release 下任何调用都不该写, got %q", buf.String())
	}
	Init(&buf) // 复位默认级别，避免影响其他用例（debug 构建）
	SetLevel(LevelDebug)
}
