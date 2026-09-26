package termws

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain 把桥接的集成测试圈进私有 tmux socket。
//
// 桥接测试用真 tmux（control mode 的握手、%output 扇出、window-size 传播
// 都没法用假对象验），而测试会 new-session / kill-session。没有隔离时这些
// 动作打在机器上真实使用的 tmux server 上 —— 轻则把开发者正挂着的会话删了，
// 重则与别的包并发跑时互相 kill-server，报出"error connecting to socket"
// 这种看起来像并发 bug 的错。
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("tmux"); err != nil {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "termws-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "建测试 socket 目录失败:", err)
		os.Exit(1)
	}
	os.Setenv("TMUX_TMPDIR", dir)
	os.Unsetenv("TMUX")

	code := m.Run()

	exec.Command("tmux", "kill-server").Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestSocketIsolation 钉住隔离本身。
func TestSocketIsolation(t *testing.T) {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" || !filepath.IsAbs(dir) {
		t.Fatalf("测试没跑在私有 socket 上: TMUX_TMPDIR=%q", dir)
	}
	if filepath.Base(filepath.Dir(dir)) == "run" {
		t.Fatalf("疑似共用 socket: %s", dir)
	}
}
