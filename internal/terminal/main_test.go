package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestMain 把集成测试圈进私有 tmux socket。
//
// 不做隔离的话，测试里的 kill-session / 会话命名都会打到机器上真实正在
// 用的 tmux server（开发机上往往真挂着一个面板会话），而且 CI 与本机
// 行为会不一致。TMUX_TMPDIR 是 tmux 自己认的 socket 目录，子进程全部
// 继承，比 -L socket 名更省事（Attach 起的 tmux -CC 也走同一目录）。
func TestMain(m *testing.M) {
	if _, err := exec.LookPath(testBin); err != nil {
		// 没 tmux：集成测试整包 skip，但纯函数测试（解码/分帧）照跑。
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "tmxterm-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "建测试 socket 目录失败:", err)
		os.Exit(1)
	}
	os.Setenv("TMUX_TMPDIR", dir)
	// 清掉可能的父会话标记，防止被误判为"已在 tmux 内"
	os.Unsetenv("TMUX")

	code := m.Run()

	// 只关这个私有 socket 上的 server，绝不碰机器上共用的那个。
	exec.Command(testBin, "kill-server").Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestSocketIsolation 钉住隔离本身：如果哪天有人删了 TestMain 的
// TMUX_TMPDIR，这条会先红，免得集成测试悄悄在真实 server 上跑。
func TestSocketIsolation(t *testing.T) {
	tmuxReady(t)
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" || !filepath.IsAbs(dir) {
		t.Fatalf("测试没跑在私有 socket 上: TMUX_TMPDIR=%q", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("TMUX_TMPDIR 指向不存在的目录: %v", err)
	}
	// 而且必须是临时目录，不是 Termux 的共用 socket 目录
	if filepath.Base(filepath.Dir(dir)) == "run" {
		t.Fatalf("疑似共用 socket: %s", dir)
	}
	_ = time.Second
}
