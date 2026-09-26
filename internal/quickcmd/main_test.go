package quickcmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain 把集成测试圈进私有 tmux socket。
//
// 这个包的测试全部对真 tmux 跑（注入的字节必须落到真的 tty 上才算数），
// 而注入器会 kill-session、会 new-session。不做隔离时这些动作打在机器上
// 真实在用的 tmux server 上 —— 开发机上往往真挂着一个终端会话，测试能把
// 它删了。tmux 只认 TMUX_TMPDIR 这一个开关（子进程全部继承），比给每个
// exec.Command 拼 -L 省事且不会漏。
//
// 没这条的后果实测过一次：go test ./... 并发跑包时，另一个包起的 tmux
// server 被这里的 kill-server 收掉，报"error connecting to …（No such file
// or directory）"—— 看起来像是被测代码有并发 bug。
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("tmux"); err != nil {
		os.Exit(m.Run()) // 没 tmux：集成测试自己 skip，纯函数测试照跑
	}
	dir, err := os.MkdirTemp("", "quickcmd-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "建测试 socket 目录失败:", err)
		os.Exit(1)
	}
	os.Setenv("TMUX_TMPDIR", dir)
	os.Unsetenv("TMUX") // 免得被当成"已在 tmux 里"而拒绝 new-session

	code := m.Run()

	exec.Command("tmux", "kill-server").Run() // 只关私有 socket 上的
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestSocketIsolation 钉住隔离本身：删掉上面的 TMUX_TMPDIR 时这条先红，
// 免得集成测试悄悄在真实 server 上跑。
func TestSocketIsolation(t *testing.T) {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" || !filepath.IsAbs(dir) {
		t.Fatalf("测试没跑在私有 socket 上: TMUX_TMPDIR=%q", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("TMUX_TMPDIR 指向不存在的目录: %v", err)
	}
	if filepath.Base(filepath.Dir(dir)) == "run" {
		t.Fatalf("疑似共用 socket: %s", dir)
	}
}
