package terminal

// pane 状态查询：快捷命令选会话时要用（设计 5.3 的"忙"判定），
// 前端的 GET /api/commands/busy 也读它。
//
// 放在 terminal 包而不是 quickcmd：这是 tmux 的交互知识（哪个 format 给
// 什么、pane_pid 到底指向谁），快捷命令只需要"忙不忙"这个结论。

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// skipNoTmux：没有 tmux 的机器（CI 的精简容器）整组跳过，
// 而不是报一个和代码无关的失败。
func skipNoTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(DefaultBin); err != nil {
		t.Skipf("没有 tmux: %v", err)
	}
}

// 前台跑着别的程序 = 忙。
//
// 判据是 pane_current_command 和 pane_pid 那个进程的 comm **不一致**，
// 而不是"pane_current_command 不等于 bash"：后者把 shell 的名字写死了，
// 换成 zsh/fish 的机器上会永远判成忙（每次点快捷命令都另开一个会话，
// 而用户完全不知道为什么）。pane_pid 是 pane 里最上层那个进程，
// 在 shell 提示符下就是 shell 本身，所以它自己就是基准。
func TestPaneBusyForeground(t *testing.T) {
	skipNoTmux(t)
	name := "lp-panestate-fg"
	_ = KillSession(DefaultBin, name)
	if out, err := exec.Command(DefaultBin, "new-session", "-d", "-s", name,
		"-x", "80", "-y", "24").CombinedOutput(); err != nil {
		t.Fatalf("造会话失败: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, name) })
	// 等满静默窗口：刚建好的会话里 shell 刚画完提示符，那本身就是“刚刚有输出”，
	// 按设计（判定保守）就该判忙。拿这个时刻验“空闲”是在测前提，不是测代码。
	time.Sleep(time.Duration(probeQuiet+1) * time.Second)

	st, err := PaneStateOf(DefaultBin, name)
	if err != nil {
		t.Fatal(err)
	}
	if st.ShellName == "" {
		t.Fatalf("取不到 shell 名（pane_pid/comm 读失败？）: %+v", st)
	}
	if st.Busy(probeQuiet, time.Now()) {
		t.Fatalf("静默窗口已过、前台是 shell，却仍判为忙: %+v", st)
	}

	// 外部命令：pane_current_command 会变成 sleep
	//
	// 这里不再插一段“echo 跑完后应当转空闲”的断言：echo 刚输出完，
	// 按判据 2（最近有输出就算忙）它就是忙，那才是设计要的语义。
	typeprobe(t, name, "sleep 40")
	waitForPane(t, name, "sleep")
	st, err = PaneStateOf(DefaultBin, name)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Busy(probeQuiet, time.Now()) {
		t.Fatalf("前台是 sleep 却判为空闲: %+v", st)
	}
	// 反过来：把 quiet 取 0（只剩前台判据）也必须判忙，
	// 否则 sleep 60 这种零输出的前台进程就只能靠输出窗口挡住。
	if !st.Busy(0, time.Now()) {
		t.Fatal("前台是 sleep，窗口取 0 后应当仍靠前台判据判忙")
	}
}

// 纯 shell 循环（echo 是 builtin，pane_current_command 一直是 bash）。
//
// 只看前台命令会判成空闲，于是把命令注入一个正在刷屏的会话里 —— 这正是
// 设计 5.3 写"最近仍有持续输出"这条判据要挡的场景，两条判据缺一不可。
func TestPaneBusyRecentOutput(t *testing.T) {
	skipNoTmux(t)
	name := "lp-panestate-out"
	_ = KillSession(DefaultBin, name)
	if out, err := exec.Command(DefaultBin, "new-session", "-d", "-s", name,
		"-x", "80", "-y", "24").CombinedOutput(); err != nil {
		t.Fatalf("造会话失败: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, name) })
	time.Sleep(600 * time.Millisecond)

	// 构造一个有输出、且前台命令仍是 shell 的场景
	typeprobe(t, name, "for i in $(seq 300); do echo line$i; done")
	time.Sleep(700 * time.Millisecond)

	st, err := PaneStateOf(DefaultBin, name)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Busy(30, time.Now()) {
		t.Fatalf("刚刚大量输出、窗口 30 秒，却判为空闲: %+v", st)
	}
	// 同一个状态，窗口取 0 秒 → 不该因为"输出过"而判忙。
	// 这条专门钉住窗口真的参与运算，而不是硬编码一个数。
	if st.Busy(0, time.Now()) {
		t.Fatalf("窗口 0 秒仍判忙（窗口没生效）: %+v", st)
	}
	// 时间往前推一小时：那次输出早已出窗
	if st.Busy(2, time.Now().Add(-time.Hour)) {
		t.Fatal("一小时前算 now 时那次输出不该还算忙")
	}
}

// 基准必须是 pane_pid 自己，不能是“bash”。
//
// 用 sh 起会话：如果实现把 shell 名写死成 bash，这里前台明明是 sh
// 却会判成忙 —— 这台机器上验不出来（默认就是 bash），但用户的真机器
// 上很可能装着 zsh/fish，写死的后果是那一侧“每次点快捷命令都另开一个
// 会话”，而完全看不出为什么。
func TestPaneBusyUsesActualShell(t *testing.T) {
	skipNoTmux(t)
	name := "lp-panestate-sh"
	_ = KillSession(DefaultBin, name)
	if out, err := exec.Command(DefaultBin, "new-session", "-d", "-s", name,
		"-x", "80", "-y", "24", "sh").CombinedOutput(); err != nil {
		t.Fatalf("造 sh 会话失败: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, name) })
	time.Sleep(time.Duration(probeQuiet+1) * time.Second)

	st, err := PaneStateOf(DefaultBin, name)
	if err != nil {
		t.Fatal(err)
	}
	if st.ShellName != "sh" {
		t.Fatalf("shell 名应是 sh, got %q", st.ShellName)
	}
	if st.Busy(probeQuiet, time.Now()) {
		t.Fatalf("sh 提示符下不该判忙（把 bash 写死就会在这里红）: %+v", st)
	}
}

// 问不到 tmux 时必须报错，不能返回"空闲"。
//
// 谎报空闲的后果是：往一个已经不存在的会话里注入，接口回 200 + 一个
// session_id，前端切过去发现是空白页。
func TestPaneStateMissingSession(t *testing.T) {
	skipNoTmux(t)
	if _, err := PaneStateOf(DefaultBin, "lp-绝对不存在的会话"); err == nil {
		t.Fatal("会话不存在时该报错，好让上层把它当成不在线")
	}
}

// ---------- 工具 ----------

// probeQuiet 是本文件里用例共用的静默窗口（秒）。
const probeQuiet = 2

func typeprobe(t *testing.T, name, cmd string) {
	t.Helper()
	args := append([]string{"send-keys", "-t", name + ":0.0", "-l", "-H"}, hexBytes(cmd)...)
	if out, err := exec.Command(DefaultBin, args...).CombinedOutput(); err != nil {
		t.Fatalf("send-keys: %v (%s)", err, out)
	}
	if out, err := exec.Command(DefaultBin, "send-keys", "-t", name+":0.0", "Enter").
		CombinedOutput(); err != nil {
		t.Fatalf("Enter: %v (%s)", err, out)
	}
}

func hexBytes(s string) []string {
	out := make([]string, 0, len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, fmt.Sprintf("%02x", s[i]))
	}
	return out
}

func foregroundNow(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command(DefaultBin, "display-message", "-p", "-t", name+":0.0",
		"#{pane_current_command}").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func waitForPane(t *testing.T, name, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if foregroundNow(t, name) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等前台命令变成 %q 超时（现在是 %q）", want, foregroundNow(t, name))
}

// 尸体不是空闲：pane_dead 必须读得出来。
//
// 实测（dev/hold）：send-keys 打进死 pane 返回退出码 0、什么都不发生
// —— 注入器若把尸体判成"能投"，用户点完得到"注入成功"，命令却蒸发在
// 空气里。PaneStateOf 对着尸体报的是 cmd=bash（shell 名字还挂在那儿），
// 光看 Foreground==Shell 完全看不出来，只能靠 pane_dead。
func TestPaneStateDeadCorpse(t *testing.T) {
	skipNoTmux(t)
	runTMUX(t, "new-session", "-d", "-s", "lp-299")
	defer runTMUX(t, "kill-session", "-t", "lp-299")
	runTMUX(t, "set-option", "-t", "lp-299", "remain-on-exit", "on")
	typeprobe(t, "lp-299", "true")
	runTMUX(t, "send-keys", "-t", "lp-299:.0", "exit 3", "Enter")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if out := strings.TrimSpace(runTMUXQuiet(t, "list-panes", "-t", "=lp-299",
			"-F", "#{pane_dead}")); strings.HasPrefix(out, "1") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	st, err := PaneStateOf(DefaultBin, "lp-299")
	if err != nil {
		t.Fatal(err) // 尸体读得到状态（不像消失会话那样报错）
	}
	if !st.Dead {
		t.Fatalf("尸体必须标 Dead（cmd=%q 骗过了忙判定）: %+v", st.Foreground, st)
	}
	if st.Busy(probeQuiet, time.Now()) {
		t.Log("尸体恰好也判忙（可接受），但 Dead 仍必须为真")
	}
}
