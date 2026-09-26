package quickcmd

// 注入器测试：全部对真 tmux 跑。
//
// 这里刻意不用假会话。注入这件事的风险全在"字节到底怎么落到 tty 上"：
// send-keys 的 -l/-H/-t 语义、hex 分词、Enter 是不是真被当成回车 —— 拿假
// 对象断言"我调了 SendText"，实现把 hex 换成字面量、把引号吞掉，测试照样绿，
// 而用户在屏幕上看见的是一条被改过的命令。断言一律从 tmux 那一侧取
// （capture-pane），不看被测代码自己记的账。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"litepanel/internal/store"
	"litepanel/internal/terminal"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/qc.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func hasTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(terminal.DefaultBin); err != nil {
		t.Skipf("没有 tmux: %v", err)
	}
}

func newInjector(t *testing.T, quiet int) (*Injector, *terminal.Service) {
	t.Helper()
	hasTmux(t)
	svc := terminal.NewService(openDB(t), terminal.DefaultBin)
	in := NewInjector(svc, quiet)
	return in, svc
}

// capture 直接问 tmux 要屏幕内容：独立观测量，不经过注入器。
func capture(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command(terminal.DefaultBin, "capture-pane", "-p", "-S", "-",
		"-t", name+":0.0").CombinedOutput()
	if err != nil {
		return "" // 会话没了：让调用方的断言去报"没看到 X"
	}
	return string(out)
}

// 注入的会话必须在测试结束时真的杀掉：tmux server 是整个测试进程共享的，
// 库里泄漏的会话会让后面随便一个用例撞上 duplicate session。
func trackSession(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() { _ = terminal.KillSession(terminal.DefaultBin, name) })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// ---------- 测试 ----------

// 注入不是"把字符串写进 tmux"就完事：命令必须真的被执行。
// 断言目标是算术结果 r42 —— 只有 shell 真把 echo 当命令跑了才会出现，
// 仅仅回显在屏幕上的文本不可能造出它（tty 回显的是命令本身）。
func TestInjectWritesCommandThenEnter(t *testing.T) {
	in, svc := newInjector(t, DefaultBusyWindow)
	ctx := context.Background()

	res, err := in.Run(ctx, Command{Name: "算一下", Command: "echo r$((6*7))"})
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID <= 0 {
		t.Fatalf("没返回 session_id: %+v", res)
	}
	name := terminal.TmuxName(res.SessionID)
	trackSession(t, name)

	waitFor(t, "命令在 tmux 里执行", func() bool {
		return strings.Contains(capture(t, name), "r42")
	})
	// 会话行必须是 Service 建的（列表里看得见、能被终端页接管），
	// 而不是注入器绕过 Service 自己 new-session 出来的野会话。
	items, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range items {
		if it.ID == res.SessionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入用的会话 %d 不在会话列表里: %+v", res.SessionID, items)
	}
}

// 快捷命令的内容是用户手打的，什么都有。任何一层"帮忙解释一下"都会让
// 命令变形 —— 而变形的命令是会执行错的命令。
//
// 每个样本都用"执行后才会出现的结果"做断言目标：
// 单/双引号、反斜杠、$ 变量与命令替换、分号、重定向、中文。
func TestInjectUsesHexSendKeys(t *testing.T) {
	in, _ := newInjector(t, DefaultBusyWindow)
	ctx := context.Background()

	cases := []struct {
		why  string
		cmd  string
		want string
	}{
		// want 一律含"执行时才算得出来"的部分，所以命令行的回显不可能命中它。
		{"单引号", "printf '%s' 'a$b `c` ;d' | sed 's/^/Q/'", "Qa$b `c` ;d"},
		{"双引号+$", `echo "w=$(echo in)"`, "w=in"},
		{"反斜杠", `printf 'x\ty\n' | tr '\t' '#'`, "x#y"},
		{"分号与管道", `echo one; echo two | tr a-z A-Z`, "TWO"},
		// 重定向写到 mktemp 里，而不是当前目录：会话继承测试进程的
		// CWD（= 包目录），写相对路径会把产物落到仓库里。
		{"重定向再读回", `f=$(mktemp); echo z$((3*4)) >"$f"; cat "$f"; rm "$f"`, "z12"},
		{"中文", `echo 中文_$(echo OK)`, "中文_OK"},
		// 唯一的 hex / 字面量分界（实测）：文本以 - 开头时，`send-keys -l --…`
		// 会去解析成 tmux 自己的选项然后 rc=1（“unknown flag -d”）。
		// 快捷命令的内容是用户自己粘的自由文本，存一段参数/续行不是新鲜事，
		// 而 hex 表示下根本不存在“开头那个字符像不像选项”这个问题。
		{"以减号开头", `-v 2>/dev/null; echo DASH_OK`, "DASH_OK"},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			res, err := in.Run(ctx, Command{Name: c.why, Command: c.cmd})
			if err != nil {
				t.Fatal(err)
			}
			name := terminal.TmuxName(res.SessionID)
			trackSession(t, name)
			waitFor(t, "看到 "+c.want+"（命令: "+c.cmd+"）", func() bool {
				return strings.Contains(capture(t, name), c.want)
			})
		})
	}
}

// ---------- 忙判定 ----------

// shell 在前台跑着东西的时候注入，等于把字符串塞进正在跑的程序 stdin。
//
// 这个用例特意用 sleep：它**一个字节都不输出**。设计方案写的判据是
// "最近 busy_window 内仍有持续输出"，对 sleep 60 完全不成立 —— 照那行字
// 实现，面板会认为会话空闲，然后把命令喂给 sleep，用户看到的是"点了没反应，
// sleep 结束后命令行里多了句奇怪的话"。所以必须额外看前台命令。
//
// 断言用 pane_current_command 从 tmux 直接取：不看注入器自己的判断，
// 否则就是在用 bug 的视角验证 bug。
func TestBusySessionCausesNewSession(t *testing.T) {
	in, svc := newInjector(t, DefaultBusyWindow)
	ctx := context.Background()

	first, err := in.Run(ctx, Command{Name: "预热", Command: "echo 热身完"})
	if err != nil {
		t.Fatal(err)
	}
	name := terminal.TmuxName(first.SessionID)
	trackSession(t, name)
	waitFor(t, "预热命令跑完", func() bool {
		return strings.Contains(capture(t, name), "热身完")
	})

	// 让会话忙起来：前台挂一个 60 秒的 sleep
	run(t, name, "sleep 60")
	waitFor(t, "tmux 报告前台命令变成 sleep", func() bool {
		return foreground(t, name) == "sleep"
	})
	// 再等满静默窗口，把“最近有输出”这条判据排除掉。
	//
	// 必须等在这里而不是等更早：回显“sleep 60”这一行本身就是一次输出。
	// 不等的话“新建会话”是输出判据顺手带来的，把前台判据整个删掉
	// 测试照样绿（变异检查发现了这个假绿）—— 而本用例要验的正是
	// “sleep 这种零输出的前台进程能不能被认出来”。
	time.Sleep(time.Duration(DefaultBusyWindow+2) * time.Second)

	second, err := in.Run(ctx, Command{Name: "趁忙插入", Command: "echo 我该在新会话里"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.IsNewSession {
		t.Fatalf("会话正忙（sleep 60 在前台），却还是注入了原会话: %+v", second)
	}
	if second.SessionID == first.SessionID {
		t.Fatal("IsNewSession=true 却返回了同一个 id")
	}
	newName := terminal.TmuxName(second.SessionID)
	trackSession(t, newName)
	waitFor(t, "命令在新会话里执行", func() bool {
		return strings.Contains(capture(t, newName), "我该在新会话里")
	})

	// 原会话里的 sleep 不该被打断
	if fg := foreground(t, name); fg != "sleep" {
		t.Fatalf("原会话的前台进程被换掉了（%q → %q）：注入打断了正在跑的东西", "sleep", fg)
	}
	items, _ := svc.List(ctx)
	if len(items) != 2 {
		t.Fatalf("应该会多出一个是会话，现在 %d 个: %+v", len(items), items)
	}
}

// 空闲会话要复用。只做"忙就新建"是不行的：每点一次快捷命令就开一个
// tmux 会话，点十次之后侧栏里全是 lp-11…lp-20，而用户要找的是自己那一个。
func TestIdleSessionIsReused(t *testing.T) {
	in, _ := newInjector(t, DefaultBusyWindow)
	ctx := context.Background()

	first, err := in.Run(ctx, Command{Name: "第一条", Command: "echo 第一条"})
	if err != nil {
		t.Fatal(err)
	}
	name := terminal.TmuxName(first.SessionID)
	trackSession(t, name)
	if !first.IsNewSession {
		t.Fatal("一个会话都没有时必然是新建")
	}
	waitFor(t, "第一条跑完", func() bool {
		return strings.Contains(capture(t, name), "第一条")
	})
	// 等 3 秒（窗口 2 秒）：前台早已回到 shell。TestBusyWindowIsConfigurable
	// 用同一个场景只把窗口改成 30 秒，那边必须判忙。
	time.Sleep(3 * time.Second)

	second, err := in.Run(ctx, Command{Name: "第二条", Command: "echo 第二条"})
	if err != nil {
		t.Fatal(err)
	}
	if second.IsNewSession || second.SessionID != first.SessionID {
		t.Fatalf("空闲会话没复用: %+v", second)
	}
	waitFor(t, "第二条在同一个会话里执行", func() bool {
		return strings.Contains(capture(t, name), "第二条")
	})
}

// 窗口可配：同一个场景（一条快命令跑完 3 秒后）只改 quiet，结论必须跟着变。
//
// 这条存在的意义是钉住“窗口真的被读进去了”：硬编码 2 秒的实现
// 在配置里改成任何值都看不出区别，而用户感受是“设置了没生效”。
// 与 TestIdleSessionIsReused 是同一场景的两个 quiet 取值，两边一起看
// 才能排除“复用了 / 新建了”其实被别的判据左右。
func TestBusyWindowIsConfigurable(t *testing.T) {
	svc := terminal.NewService(openDB(t), terminal.DefaultBin)
	in := NewInjector(svc, 30) // 窗口 30 秒
	ctx := context.Background()

	first, err := in.Run(ctx, Command{Name: "快命令", Command: "echo 只输出一次"})
	if err != nil {
		t.Fatal(err)
	}
	name := terminal.TmuxName(first.SessionID)
	trackSession(t, name)
	waitFor(t, "命令跑完", func() bool { return strings.Contains(capture(t, name), "只输出一次") })
	// 与 TestIdleSessionIsReused 同样等 3 秒：前台早已回到 shell，
	// 区别只在窗口 2 还是30 秒。
	time.Sleep(3 * time.Second)

	second, err := in.Run(ctx, Command{Name: "插一脚", Command: "echo 另开一个"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.IsNewSession {
		t.Fatalf("静默窗口 %d 秒内刚有输出，仍判为空闲: %+v", 30, second)
	}
	trackSession(t, terminal.TmuxName(second.SessionID))
}

// ---------- 工具 ----------

func stat(p string) (os.FileInfo, error) { return os.Stat(p) }

func mkdirAll(p string) error { return os.MkdirAll(p, 0o755) }

// run 用 tmux 自己起一个前台进程，让会话"忙"起来。故意不经注入器：
// 被测代码不该参与制造它自己的测试前提。
func run(t *testing.T, name, cmd string) {
	t.Helper()
	if err := send(name, cmd, true); err != nil {
		t.Fatal(err)
	}
}

// foreground 取 tmux 眼里的前台命令名（pane_current_command）。
func foreground(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command(terminal.DefaultBin, "display-message", "-p",
		"-t", name+":0.0", "#{pane_current_command}").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// 命令可以带一个"顺便在这儿跑"的目录（quick_commands.cwd）。
//
// 断言 pwd 的结果而不是屏幕上有没有 cd：注入本身会回显命令文本，
// 看到 "cd ~/xxx" 说明不了目录真的切过去了。
func TestOptionalCwdIsApplied(t *testing.T) {
	in, _ := newInjector(t, DefaultBusyWindow)
	ctx := context.Background()

	dir := t.TempDir()
	res, err := in.Run(ctx, Command{Name: "在指定目录", Command: "pwd >cwd_probe.txt; cat cwd_probe.txt", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	name := terminal.TmuxName(res.SessionID)
	trackSession(t, name)
	waitFor(t, "pwd 输出等于目标目录", func() bool {
		return strings.Contains(capture(t, name), dir)
	})
	// 文件必须落在那个目录里，而不是会话的初始目录
	if _, err := stat(dir + "/cwd_probe.txt"); err != nil {
		t.Fatalf("命令没在 cwd 里执行: %v", err)
	}
}

// 带空格与引号的目录名不该改变"cd 到哪里"。
func TestCwdWithSpacesIsQuoted(t *testing.T) {
	in, _ := newInjector(t, DefaultBusyWindow)
	ctx := context.Background()

	dir := t.TempDir() + "/有 空格 和'quote"
	if err := mkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	res, err := in.Run(ctx, Command{Name: "怪目录", Command: "pwd >p.txt", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	name := terminal.TmuxName(res.SessionID)
	trackSession(t, name)
	waitFor(t, "cd 到含空格引号的目录", func() bool {
		_, err := stat(dir + "/p.txt")
		return err == nil
	})
}

// 前端标"可投/忙"的判据必须与注入时用的判据是**同一份**。
//
// 如果 HTTP 层自己拿 PaneState 再判一次，quiet 窗口就有了第二个主人：
// 设置页把窗口从 2 秒调到 10 秒，注入侧读配置、展示侧写死 2 秒，用户看到
// 四个标签都显示"空闲"，点下去却每次都被另开一个新会话 —— 面板上没有任何
// 一处报错，最容易被误判成"这功能坏了"。所以对外只暴露算好的结论。
func TestInjectorBusySharesVerdictWithRun(t *testing.T) {
	in, svc := newInjector(t, DefaultBusyWindow)
	ctx := context.Background()

	idle, err := svc.Create(ctx, terminal.SessionInput{Title: "空闲那个"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Delete(ctx, idle.ID)
	busy, err := svc.Create(ctx, terminal.SessionInput{Title: "忙那个"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Delete(ctx, busy.ID)

	run(t, busy.TmuxName, "sleep 40")
	waitFor(t, "忙那个的前台变成 sleep", func() bool {
		return foreground(t, busy.TmuxName) == "sleep"
	})
	// 等满静默窗口：两个会话刚刚都画过提示符/回显过命令，那本身就是输出。
	// 不等的话"空闲"这条永远不成立，测的就只是"谁输出得更早"。
	time.Sleep(time.Duration(DefaultBusyWindow+2) * time.Second)

	states, err := in.Busy(ctx, []int64{idle.ID, busy.ID, 999999})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("tmux 里查不到的会话不该编出状态来: %+v", states)
	}
	if states[idle.ID].Busy {
		t.Errorf("空闲会话被判忙: %+v", states[idle.ID])
	}
	if !states[busy.ID].Busy {
		t.Errorf("跑着 sleep 的会话该判忙: %+v", states[busy.ID])
	}
	// 展示字段：前端 tooltip 要写"前台是 sleep"，只给一个布尔没法解释
	if states[busy.ID].Foreground == "" || states[busy.ID].Foreground == states[busy.ID].ShellName {
		t.Errorf("foreground 该是真实前台命令: %+v", states[busy.ID])
	}

	// 同一批会话走 Run：必须挑中那个空闲的（这才叫同一份判据）
	res, err := in.Run(ctx, Command{Name: "挑空闲", Command: "echo 挑中了"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Delete(ctx, res.SessionID)
	if res.SessionID != idle.ID {
		t.Fatalf("该投进空闲会话 id=%d, got %+v", idle.ID, res)
	}
}

// 一个会话都问不到时报错，而不是回一张空表。
//
// 空表在 HTTP 层会画成"所有会话都空闲"，而真原因可能是 tmux 刚被关掉 ——
// 用户会往空闲会话里投命令，然后什么都没发生。
func TestInjectorBusyFailsWhenNothingAnswered(t *testing.T) {
	in, _ := newInjector(t, DefaultBusyWindow)
	if _, err := in.Busy(context.Background(), []int64{999998, 999997}); err == nil {
		t.Fatal("一个会话都问不到时该报错，不能回空表")
	}
	// 空输入不报错也不查：页面刚打开、一个会话都没有是正常状态
	states, err := in.Busy(context.Background(), nil)
	if err != nil {
		t.Fatalf("空输入不该报错: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("空输入该回空表: %+v", states)
	}
}

// 展示侧必须用**配置里的**窗口，不能自己拿一个常量。
//
// 上面那条只证明了"两处结论一致"，而两处都写死同一个常量时它照样绿 ——
// 那正是设置页要改的那个值丢失的方式：用户在设置里把"判定忙的时间窗口"
// 调到 30 秒，注入侧照 30 秒建新会话，标签却按 2 秒显示"空闲"，两边
// 永远对不上，且没有任何一处报错。
func TestBusyReflectsConfiguredWindow(t *testing.T) {
	in, svc := newInjector(t, 30)
	ctx := context.Background()

	first, err := in.Run(ctx, Command{Name: "刚跑过", Command: "echo 刚跑完"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Delete(ctx, first.SessionID)
	waitFor(t, "命令跑完", func() bool {
		return strings.Contains(capture(t, terminal.TmuxName(first.SessionID)), "刚跑完")
	})
	// 越过默认窗口（2 秒）但仍在配置的 30 秒之内：
	// 只有真的读 in.quiet，这里才会判忙。
	time.Sleep(3 * time.Second)

	states, err := in.Busy(ctx, []int64{first.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if !states[first.SessionID].Busy {
		t.Fatalf("窗口配成 30 秒时，3 秒前刚有输出的会话该判忙: %+v", states)
	}
	// 同一会话在默认窗口下就是空闲 —— 证明上面那条红来自配置，
	// 而不是"怎么问都忙"。
	plain, _ := NewInjector(svc, DefaultBusyWindow).Busy(ctx, []int64{first.SessionID})
	if plain[first.SessionID].Busy {
		t.Fatalf("默认 2 秒窗口下该判空闲: %+v", plain)
	}
}

// 快捷命令等效于用户手打：注入时一次 send-keys 就是命令原文，
// 不许掺任何标注字节。
//
// 设计方案 16.3 原文写的是"注入并标注「来自快捷命令」"，2026-09 裁决改为
// 不改字节流、当作用户输入等效处理。因为那条标注只有两种实现，两种都更糟：
// 往 pane 里多打一行带颜色的注释，它会进 shell 历史、混进 cat 的输出、被
// less/vim 当成真输入吃掉；给 xterm 加高亮则要求面板改写 tty 字节流。
//
// 观测量：PATH 前面挡一个记录 argv 的 tmux 包装脚本，底下 exec 真 tmux。
// 不用 capture-pane 找"屏幕上没有标注字样"—— pane 上有整行 PS1 回显，
// 长得跟标注很像，那种断言会被回显糊过去；也不用 shell history：
// ~/.bash_history 跨会话共享，"历史里只有我这两条"从来就不成立。
// 包装脚本给出的是"到底发了什么参数"，逐字精确，且命令照样真的执行。
func TestInjectSendsNothingButTheCommand(t *testing.T) {
	in, _ := newInjector(t, DefaultBusyWindow)
	logged := withRecordingTmux(t)
	ctx := context.Background()

	cases := []struct {
		why  string
		cmd  Command
		want []string // 期望的、逐字等于原文的注入内容（不含 Enter）
	}{
		{"无 cwd", Command{Name: "看盘", Command: "df -h"}, []string{"df -h"}},
		// cwd 是唯一一条额外文本，且必须是这一条：它由 quoteShell 包过，
		// 除此之外不许有任何"顺便打一行说明"。
		{"带 cwd", Command{Name: "看盘", Command: "df -h", Cwd: "/tmp/a b"},
			[]string{"cd '/tmp/a b'", "df -h"}},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			logged.Reset()
			res, err := in.Run(ctx, c.cmd)
			if err != nil {
				t.Fatal(err)
			}
			trackSession(t, terminal.TmuxName(res.SessionID))
			if got := logged.Injections(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("注入发的文本必须逐字等于命令本体\n实际=%q\n期望=%q", got, c.want)
			}
		})
	}
}

// ---------- 记录 argv 的 tmux 包装 ----------

type tmuxLog struct {
	path string
}

// withRecordingTmux 把目录里放一个名为 tmux 的包装脚本并挂到 PATH 最前面。
// 它把每次调用的参数记到文件里，然后 exec 真 tmux —— 会话、shell、tty 全是
// 真的，只是顺便留下"到底发了什么"。
func withRecordingTmux(t *testing.T) *tmuxLog {
	t.Helper()
	hasTmux(t)
	real, err := exec.LookPath(terminal.DefaultBin)
	if err != nil {
		t.Skipf("没有 tmux: %v", err)
	}
	dir := t.TempDir()
	log := &tmuxLog{path: filepath.Join(dir, "argv.log")}
	wrap := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >>" + log.path + "\n" +
		"printf '\\0' >>" + log.path + "\n" +
		"exec " + real + " \"$@\"\n"
	if err := os.WriteFile(wrap, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func (l *tmuxLog) Reset() { _ = os.Remove(l.path) }

// Injections 返回每次 `send-keys -l -H …` 解回来的原文（顺序即调用顺序）。
// Enter 那次调用不带 -l，天然不在里面；list-panes / new-session 之类
// 状态查询同理。
func (l *tmuxLog) Injections() []string {
	b, err := os.ReadFile(l.path)
	if err != nil {
		return nil
	}
	var out []string
	for _, call := range strings.Split(string(b), "\x00") {
		args := strings.Split(strings.TrimSuffix(call, "\n"), "\n")
		if len(args) < 4 || args[0] != "send-keys" {
			continue
		}
		var lit []string
		for i, a := range args {
			if a == "-H" {
				lit = args[i+1:]
				break
			}
		}
		if lit == nil {
			continue
		}
		buf := make([]byte, 0, len(lit))
		for _, h := range lit {
			v, err := strconv.ParseUint(h, 16, 8)
			if err != nil {
				out = append(out, "!无法解析的 hex:"+strings.Join(lit, " "))
				break
			}
			buf = append(buf, byte(v))
		}
		if len(buf) > 0 {
			out = append(out, string(buf))
		}
	}
	return out
}

// 分屏会话里主 pane 是尸体：List 说会话活着（另一个 pane 确实活着），
// 但注入目标 =name:0.0 是死的 —— send-keys 进死 pane 回 exit 0，
// 命令蒸发（实测）。注入器必须在候选阶段就看 pane_dead，否则用户
// 得到 success + 屏幕上一个字节都没变。
func TestInjectSkipsSessionWithDeadMainPane(t *testing.T) {
	in, svc := newInjector(t, DefaultBusyWindow)
	ctx := context.Background()

	meta, err := svc.Create(ctx, terminal.SessionInput{Title: "分屏尸"})
	if err != nil {
		t.Fatal(err)
	}
	name := meta.TmuxName
	trackSession(t, name)
	// 先起一个活着的副 pane（sleep），再让主 pane 的 shell 退出：
	// 副 pane 撑着会话不死，主 pane 留下尸体 —— 这才是真实里会出现
	// 的常驻形态（用户在分屏里关掉一个 shell）。
	// 顺序反过来不行：kill-pane / 最后一个 pane 死亡会带走整个会话，
	// 那时连尸体都读不到（实测等待超时就是这个原因）。
	run(t, name, "tmux split-window -t "+name+":0.0 'sleep 300'")
	waitFor(t, "出现第二个 pane", func() bool {
		out, _ := exec.Command(terminal.DefaultBin, "list-panes", "-t", name+":0",
			"-F", "#{pane_index}").CombinedOutput()
		return strings.Contains(string(out), "1")
	})
	run(t, name, "exit")
	waitFor(t, "主 pane 变尸体", func() bool {
		out, _ := exec.Command(terminal.DefaultBin, "list-panes", "-t", name+":0.0",
			"-F", "#{pane_dead}").CombinedOutput()
		return strings.TrimSpace(strings.Split(string(out), "\n")[0]) == "1"
	})

	// 必须等满静默窗口再投：split/exit 的回显本身就是"最近有输出"。
	// 不等的话"跳过"是忙判定顺手带来的，把 Dead 判据删掉测试照样绿
	// （假绿教训在 TestBusySessionCausesNewSession 里记过一次）。
	time.Sleep(time.Duration(DefaultBusyWindow+2) * time.Second)
	res, err := in.Run(ctx, Command{Name: "别投尸体", Command: "echo 不许蒸发"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsNewSession {
		t.Fatalf("主 pane 已死，必须另开会话而不是投进死 pane: %+v", res)
	}
	// 死 pane 上不许出现注入的字节（capture 尸体是读得到的）
	if strings.Contains(capture(t, name), "不许蒸发") {
		t.Fatal("字节进了死 pane —— 界面报成功，命令蒸发")
	}
}
