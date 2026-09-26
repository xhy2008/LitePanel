package terminal

// 「会话怎么死的」判定测试（用户 2026-09 裁决的执行基础）：
//   退出码 0 的尸体 = 正常退出 → 上层自动删行、不留痕迹
//   退出码非 0 的尸体 = 异常退出 → 保留、显示遗言、只能手动删
//   名字凭空消失 / server 连不上 = 异常/未知 → 保留、什么都不动
//
// 全部对真 tmux 跑。判据只能是 list-panes 的尸检结果：实测（dev/deathprobe）
// 三种死法的 control 事件流完全相同（%sessions-changed → %exit 空参数），
// 事后 list-sessions 也只说"名字不在"。

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runTMUX 是测试搭场景用的裸 tmux。
func runTMUX(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", args...).CombinedOutput()
	t.Logf("tmux %v -> %v (%s)", args, err, strings.TrimSpace(string(out)))
	return string(out)
}

// makeCorpse 搭出一个"shell 已退出"的会话。remain-on-exit 手动设 ——
// 这个场景必须独立于 CreateSession 的实现来搭，否则 T1a 的测试正确性
// 反过来依赖 T1b（CreateSession 带不带这个选项）还没写的代码。
func makeCorpse(t *testing.T, name, exitCmd string) {
	t.Helper()
	runTMUX(t, "new-session", "-d", "-s", name)
	runTMUX(t, "set-option", "-t", name, "remain-on-exit", "on")
	// 等 shell 真正就绪再送命令：new-session 返回时 shell 还在 fork，
	// 早到的 send-keys 会打进虚空（这台板子上实测发生过）。
	waitPrompt(t, name)
	runTMUX(t, "send-keys", "-t", name, exitCmd, "Enter")
	// 等尸体出现而不是 sleep 赌运气：轮询 pane_dead。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		out := runTMUXQuiet(t, "list-panes", "-t", "="+name,
			"-F", "#{pane_dead}")
		if strings.TrimSpace(out) == "1" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等不到 %s 变成尸体", name)
}

func runTMUXQuiet(t *testing.T, args ...string) string {
	t.Helper()
	out, _ := exec.Command("tmux", args...).CombinedOutput()
	return string(out)
}

func waitPrompt(t *testing.T, name string) {
	t.Helper()
	runTMUX(t, "send-keys", "-t", name, "true", "Enter")
	time.Sleep(300 * time.Millisecond)
}

func TestListPaneStatuses(t *testing.T) {
	tmuxReady(t)

	runTMUX(t, "new-session", "-d", "-s", "lp-200") // 活会话
	defer runTMUX(t, "kill-session", "-t", "lp-200")
	makeCorpse(t, "lp-201", "exit 0")
	defer runTMUX(t, "kill-session", "-t", "lp-201")
	makeCorpse(t, "lp-202", "exit 7")
	defer runTMUX(t, "kill-session", "-t", "lp-202")
	// 非前缀会话：面板的会话按 lp-<id> 命名，别人的会话不许混进结果 ——
	// map 的键会直接当"面板登记的会话"来查，混进来就是拿陌生会话的死活
	// 替我们的记录作决定。
	runTMUX(t, "new-session", "-d", "-s", "someone-else")
	defer runTMUX(t, "kill-session", "-t", "someone-else")

	got, err := ListPaneStatuses("tmux", TmuxPrefix)
	if err != nil {
		t.Fatalf("ListPaneStatuses: %v", err)
	}
	if _, ok := got["someone-else"]; ok {
		t.Errorf("非前缀会话混进了结果: %v", got)
	}
	if s := got["lp-200"]; s.Dead {
		t.Errorf("活会话被判成死的: %+v", s)
	}
	if s := got["lp-201"]; !s.Dead || s.ExitStatus != 0 {
		t.Errorf("exit 0 的尸体应是 Dead+status 0，得 %+v", s)
	}
	if s := got["lp-202"]; !s.Dead || s.ExitStatus != 7 {
		t.Errorf("exit 7 的尸体应带退出码 7，得 %+v", s)
	}
}

// 分屏会话的规矩：只要还有一个 pane 活着就不算死；都死了取**最大**退出码
// —— 一个窗口的 exit 0 不许抹掉另一个窗口的异常 3，判定的错向必须是
// "多当成异常要人看"而不是"少当成异常自动删"。
func TestListPaneStatusesMultiPane(t *testing.T) {
	tmuxReady(t)
	runTMUX(t, "new-session", "-d", "-s", "lp-203")
	defer runTMUX(t, "kill-session", "-t", "lp-203")
	runTMUX(t, "set-option", "-t", "lp-203", "remain-on-exit", "on")
	waitPrompt(t, "lp-203")
	runTMUX(t, "split-window", "-t", "lp-203")
	waitPrompt(t, "lp-203")
	runTMUX(t, "send-keys", "-t", "lp-203.0", "exit 3", "Enter")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.TrimSpace(runTMUXQuiet(t, "list-panes", "-t", "lp-203.0",
			"-F", "#{pane_dead}")) == "1" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 一个 pane 死了、另一个还在敲命令：会话必须算活的。
	got, err := ListPaneStatuses("tmux", TmuxPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if s := got["lp-203"]; s.Dead {
		t.Fatalf("还有一个 pane 活着就被判死: %+v", s)
	}

	runTMUX(t, "send-keys", "-t", "lp-203.1", "exit 0", "Enter")
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.TrimSpace(runTMUXQuiet(t, "list-panes", "-t", "lp-203.1",
			"-F", "#{pane_dead}")) == "1" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	got, err = ListPaneStatuses("tmux", TmuxPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if s := got["lp-203"]; !s.Dead || s.ExitStatus != 3 {
		t.Errorf("全死后应取最大退出码 3（0 不许抹掉异常），得 %+v", s)
	}
}

// 没有 server 必须**报错**，而不是回空 map。
//
// 这是整套规则的地基："空 map"和"问不到"必须分得开。混成一回事的话，
// TMUX_TMPDIR 指错目录就等于"所有会话都不在了"—— 上层会把每一行都当成
// 消失的异常会话处理（哪怕再谨慎也是全员掉线），更糟的是历史上这个混淆
// 曾经把"全部标死"写成默认行为。
func TestListPaneStatusesNoServer(t *testing.T) {
	tmuxReady(t)
	t.Setenv("TMUX_TMPDIR", t.TempDir()) // 保证这里没有任何 server

	_, err := ListPaneStatuses("tmux", TmuxPrefix)
	if err == nil {
		t.Fatal("没有 server 时必须报错，不许回空结果")
	}
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("没 server 要能被认出来（上层据此判「未知」），得 %v", err)
	}
}
