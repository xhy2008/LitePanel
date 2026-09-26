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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	runTMUX(t, "send-keys", "-t", "="+name+":.0", exitCmd, "Enter")
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
	runTMUX(t, "send-keys", "-t", "="+name+":.0", "true", "Enter")
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

// CreateSession 必须自带 remain-on-exit —— 整条"退出码=死因"的规则
// 依赖尸体存在。这条测试刻意不问 show-options（那只能证明"选项被设过"），
// 而是走到底：真退出，然后要求尸体检得到。会话整个消失（没开选项时
// 的行为）在这里会红。
func TestCreateSessionLeavesCorpse(t *testing.T) {
	tmuxReady(t)
	name := fmt.Sprintf("lp-corpse-%d", time.Now().UnixNano())
	s := mustCreate(t, name, SessionOpts{})
	defer func() { _ = s.Close(); _ = KillSession(testBin, name) }()

	waitPrompt(t, name)
	runTMUX(t, "send-keys", "-t", "="+name+":.0", "exit 4", "Enter")

	deadline := time.Now().Add(8 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = strings.TrimSpace(runTMUXQuiet(t, "list-panes", "-t", "="+name,
			"-F", "#{pane_dead}|#{pane_dead_status}"))
		if strings.HasPrefix(last, "1|") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("CreateSession 建的会话退出后没留下尸体（pane_dead=1 + 退出码），最后看到 %q", last)
}

// Create 失败时必须把 tmux 侧"半成功"的会话一起清掉。
//
// 构造办法：拿一个包装脚本当真 bin —— new-session 照做，但 set-option
// 一律失败（= ArmCorpse 失败的真实场景之一）。这时 CreateSession 报错，
// 会话却已经躺在 tmux 里且没人登记过它：漏清的话它躲得过死因清理，
// 还会让下一次的 lp-<id> 直接 duplicate session。
func TestCreateFailureCleansHalfMadeSession(t *testing.T) {
	tmuxReady(t)
	dir := t.TempDir()
	wrap := filepath.Join(dir, "tmux-wrap.sh")
	body := "#!/bin/sh\nif [ \"$1\" = set-option ]; then echo 'boom' >&2; exit 1; fi\nexec tmux \"$@\"\n"
	if err := os.WriteFile(wrap, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t)
	svc := NewService(db, wrap)

	if _, err := svc.Create(context.Background(), SessionInput{Title: "残骸"}); err == nil {
		t.Fatal("set-option 失败时 Create 必须报错（ArmCorpse 是可失败依赖）")
	}
	// 新库 → rowid 1 → lp-1（名字可推算）
	name := TmuxName(1)
	out := runTMUXQuiet(t, "list-sessions", "-F", "#{session_name}")
	if strings.Contains(out, name) {
		t.Fatalf("创建失败后 tmux 里不该留下 %s（残骸没人管 + 名字被占死）:\n%s", name, out)
	}
	if items, _ := ListSessionsMeta(db); len(items) != 0 {
		t.Fatalf("创建失败后库里不该留行: %+v", items)
	}
}

// new-session 失败（名字被占）时，占名的会话是**别人的**，一票否决
// 不许清理逻辑碰它。这条钉住 defer 里 handoff 标志的另一半：没有它，
// “创建失败就杀同名会话”会把用户手工建/收编来的会话杀了 —— 比留残
// 严重得多。
func TestCreateSessionDuplicateDoesNotKillOther(t *testing.T) {
	tmuxReady(t)
	name := fmt.Sprintf("lp-dup-%d", time.Now().UnixNano())
	runTMUX(t, "new-session", "-d", "-s", name)
	defer runTMUX(t, "kill-session", "-t", name)

	if _, err := CreateSession(DefaultBin, name, SessionOpts{}); err == nil {
		t.Fatal("同名会话存在时 CreateSession 必须失败")
	}
	if !tmuxHas(name) {
		t.Fatal("创建失败时把占名的别人会话杀了 —— handoff 之前的路径不许清理")
	}
}
