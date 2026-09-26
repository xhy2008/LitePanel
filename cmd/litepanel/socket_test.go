package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain 把装配层的集成测试圈进私有 tmux socket。
//
// cmd/litepanel 里有用例走真 tmux（终端 WS 桥接、启动对账），它们会
// new-session / kill-session。没有隔离时这些动作打在机器上真实使用的 tmux
// server 上 —— 测试能删掉开发者正挂着的终端会话。
//
// 单独建文件而不是塞进 wiring_release_test.go：那个文件带 //go:build !debug，
// 放这里会让 -tags debug 那一侧没有 TestMain（go test 两套构建标签都得绿是
// 本仓库的硬要求）。
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("tmux"); err != nil {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "litepanel-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "建测试 socket 目录失败:", err)
		os.Exit(1)
	}
	os.Setenv("TMUX_TMPDIR", dir)
	os.Unsetenv("TMUX")

	code := m.Run()

	exec.Command("tmux", "kill-server").Run() // 只关私有 socket 上的
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestSocketIsolation 钉住隔离本身。
//
// 这一条在本包尤其重要：测试里启动的桥接继承的是进程环境里的 TMUX_TMPDIR，
// 环境变量一丢，面板就会连到真实 server，届时 kill-session 杀的是真会话。
func TestSocketIsolation(t *testing.T) {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" || !filepath.IsAbs(dir) {
		t.Fatalf("测试没跑在私有 socket 上: TMUX_TMPDIR=%q", dir)
	}
	if filepath.Base(filepath.Dir(dir)) == "run" {
		t.Fatalf("疑似共用 socket: %s", dir)
	}
}
