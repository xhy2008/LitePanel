package terminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 集成测试：全部对真 tmux server 跑。Termux 有 tmux 3.7c，CI 的 Ubuntu
// 也有；没有 tmux 的机器上整个文件跳过（不假装测过）。

const testBin = "tmux"

func tmuxReady(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(testBin); err != nil {
		t.Skip("没有 tmux，跳过集成测试")
	}
}

// newTestSession 建一个测试会话并在测试结束时清掉。
func newTestSession(t *testing.T, opts SessionOpts) *Session {
	t.Helper()
	return mustCreate(t, fmt.Sprintf("lp-test-%d", time.Now().UnixNano()), opts)
}

// mustCreate 建会话，失败重试一次。
//
// 重试的理由要说清：这台 Android 机器在跑整仓测试时会 OOM 掉刚起的 tmux
// server，报 "server exited unexpectedly" —— 那是环境压力，不是被测代码
// 的缺陷，让它隔 200ms 重来一次即可。**只在这里兜**：生产代码遇到 server
// 崩必须如实报错，不能学测试假装没事。
func mustCreate(t *testing.T, name string, opts SessionOpts) *Session {
	t.Helper()
	var last error
	for i := 0; i < 2; i++ {
		s, err := CreateSession(testBin, name, opts)
		if err == nil {
			t.Cleanup(func() { _ = s.Close(); _ = KillSession(testBin, name) })
			return s
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("CreateSession: %v", last)
	return nil
}

// expect 在截止时间内从事件流里找满足条件的第一个事件。
func expect(t *testing.T, s *Session, d time.Duration, what string,
	pred func(Event) bool) Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-s.Events():
			if pred(e) {
				return e
			}
		case <-deadline:
			t.Fatalf("等待%s超时", what)
		}
	}
}

func outputContains(sub string) func(Event) bool {
	return func(e Event) bool { return e.Kind == EvOutput && strings.Contains(e.Data, sub) }
}

// TestCreateThenOutputReachesSubscriber：create + attach 后，pane 里的
// 输出要作为**解码后的字节**到达事件流。
func TestCreateThenOutputReachesSubscriber(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{Cols: 80, Rows: 24})

	if err := s.SendText(context.Background(), "echo HELLO-CTL\n"); err != nil {
		t.Fatal(err)
	}
	e := expect(t, s, 5*time.Second, "echo 输出", outputContains("HELLO-CTL"))
	// 到达的必须是解码后的字节：转义残留会直接花屏
	if strings.Contains(e.Data, `\033`) || strings.Contains(e.Data, `\012`) {
		t.Fatalf("数据没解码: %q", e.Data)
	}
}

// TestSendTextLiteralBytes：引号、$、反斜杠、分号、中文必须原样进 pane。
// 这是快捷命令注入（D20）的生命线 —— 任何一层多做了解释，用户的命令
// 就不是用户的命令了。
func TestSendTextLiteralBytes(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{})

	lit := `A 'B' $C \D ; E 中文`
	// 让 pane 把它回显成 "MARK:<原样>"：经由 printf %s，不经受二次解释
	cmd := fmt.Sprintf("printf 'MARK:%%s|\\n' '%s'\n", strings.ReplaceAll(lit, "'", `'\''`))
	if err := s.SendText(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	// 注意要等执行产物：命令本身的回显里也有 "MARK:"，等它会假通过
	e := expect(t, s, 5*time.Second, "回显", outputContains("MARK:"+lit+"|"))
	if !strings.Contains(e.Data, "MARK:"+lit+"|") {
		t.Fatalf("字面字节变形: 要包含 %q, got %q", lit, e.Data)
	}
}

// TestANSISurvives：颜色与光标序列要作为原始控制字节到达（R3 的根）。
func TestANSISurvives(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{})
	if err := s.SendText(context.Background(), "printf '\\033[31mRED\\033[0m\\n'\n"); err != nil {
		t.Fatal(err)
	}
	// 等"带真实 ESC 的 RED"：命令回显里也有 RED 但没有 ESC 字节
	e := expect(t, s, 5*time.Second, "ANSI 输出", func(ev Event) bool {
		return ev.Kind == EvOutput && strings.Contains(ev.Data, "\x1b[31mRED")
	})
	if !strings.Contains(e.Data, "\x1b[31mRED\x1b[0m") {
		t.Fatalf("ESC 序列丢失/变形: %q", e.Data)
	}
}

// TestResizeAppliesToWindow：Resize 走 refresh-client -C（探针实测：
// 控制模式下窗口尺寸由客户端上报决定，改 PTY 没用）。
func TestResizeAppliesToWindow(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{Cols: 80, Rows: 24})
	if err := s.Resize(context.Background(), 137, 41); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out := tmuxOut(t, "display-message", "-t", s.Name(), "-p",
			"#{window_width}x#{window_height}")
		if strings.TrimSpace(out) == "137x41" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("resize 未生效，最终尺寸 %s",
		tmuxOut(t, "display-message", "-t", s.Name(), "-p", "#{window_width}x#{window_height}"))
}

// TestCaptureReplaysScrollback：capture 能拿回历史（含 ANSI），这是
// 重连回放的基础。探针实测 capture 数据是**原样字节**（不转义）。
func TestCaptureReplaysScrollback(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{HistoryLimit: 3000})
	mark := fmt.Sprintf("FIRST%d", time.Now().UnixNano()%100000)
	if err := s.SendText(context.Background(),
		fmt.Sprintf("printf '\\033[31m%s\\033[0m\\n'; for i in $(seq 1 300); do echo L$i; done\n", mark)); err != nil {
		t.Fatal(err)
	}
	// 等最后一条输出出现，确保前面的都进历史了
	expect(t, s, 8*time.Second, "300 行跑完", outputContains("L300"))

	text, err := s.CaptureAll(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !strings.Contains(text, mark) || !strings.Contains(text, "L300") {
		t.Fatalf("回放不完整: 有 %s=%v 有 L300=%v\ncapture 全文=%q", mark,
			strings.Contains(text, mark), strings.Contains(text, "L300"), headOf(text, 600))
	}
	if !strings.Contains(text, "\x1b[31m") {
		t.Fatalf("capture -e 应保留 ANSI，前 200 字节: %q", headOf(text, 200))
	}
}

// TestCaptureEmptyFreshSession：新会话 capture 不该报错（重连时序里
// capture 可能早于任何输出）。
func TestCaptureEmptyFreshSession(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{})
	if _, err := s.CaptureAll(context.Background()); err != nil {
		t.Fatalf("新会话 capture 失败: %v", err)
	}
}

// TestCloseKeepsTmuxSession：Close 只撤控制面板自己的连接，tmux 会话与
// 其中进程必须活着（D5：关浏览器/面板重启不杀会话）。
func TestCloseKeepsTmuxSession(t *testing.T) {
	tmuxReady(t)
	name := fmt.Sprintf("lp-keep-%d", time.Now().UnixNano())
	s := mustCreate(t, name, SessionOpts{})
	defer KillSession(testBin, name)

	if err := s.SendText(context.Background(), "echo BEFORE-CLOSE\n"); err != nil {
		t.Fatal(err)
	}
	expect(t, s, 5*time.Second, "输出", outputContains("BEFORE-CLOSE"))

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 会话必须还在
	if !sessionExists(testBin, name) {
		t.Fatal("Close 把 tmux 会话弄没了 —— 这违背 D5")
	}
	// 还在执行命令：重新开一个控制连接能看到旧输出（经 capture）
	s2, err := Attach(testBin, name)
	if err != nil {
		t.Fatalf("重新 Attach: %v", err)
	}
	defer s2.Close()
	text, err := s2.CaptureAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "BEFORE-CLOSE") {
		t.Fatal("重连后 capture 丢了历史")
	}
}

// shell 退出的契约（2026-09 随"退出码=死因"裁决改版）。
//
// 旧契约是"shell 退出 → %exit → Exited"。启用 remain-on-exit 之后
// 这条不再成立，而且**必须不成立**：会话退出后要留下带退出码的尸体
// 供死因判定与遗言回放（实测探针 dev/hold：exit 之后连接保持、
// capture 能读到遗言，随后 kill-session 才看到 %exit）。
//
// 所以"会话已结束"的观测通道换成了 pane_dead（ListPaneStatuses），
// 连接断开只表示"会话真的没了"。这里把两头都钉住：退出≠断开、
// 尸体上读得到遗言、kill 之后才断开。少了任何一头，前端要么对着
// 尸体以为程序还在跑，要么删除会话后 WS 永远挂在半开连接上。
func TestShellExitLeavesCorpseNotDisconnect(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{})
	ctx := context.Background()
	if err := s.SendText(ctx, "echo 遗言在此\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := s.SendText(ctx, "exit\n"); err != nil {
		t.Fatal(err)
	}
	name := s.Name()
	// 等尸体（真实 tmux 上的"会话已结束"信号）
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.HasPrefix(strings.TrimSpace(runTMUXQuiet(t,
			"list-panes", "-t", "="+name, "-F", "#{pane_dead}")), "1") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-s.Exited():
		t.Fatal("shell 退出（尸体还在）不该断开连接 —— 尸体要留着读遗言")
	default:
	}
	txt, err := s.CaptureAll(ctx)
	if err != nil || !strings.Contains(txt, "遗言在此") {
		t.Fatalf("尸体上必须读得到遗言: err=%v text=%q", err, txt)
	}
	if err := KillSession(testBin, name); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Exited():
	case <-time.After(6 * time.Second):
		t.Fatal("会话真没了（kill）之后必须发退出事件")
	}
}

// TestKillSessionRemoves：面板删除会话 = kill-session，tmux 里必须消失。
func TestKillSessionRemoves(t *testing.T) {
	tmuxReady(t)
	name := fmt.Sprintf("lp-kill-%d", time.Now().UnixNano())
	s := mustCreate(t, name, SessionOpts{})
	defer s.Close()
	if err := KillSession(testBin, name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if sessionExists(testBin, name) {
		t.Fatal("kill 之后会话还在")
	}
}

// TestListSessionsForReconcile：面板启动对账用 —— 列出现存 lp-* 会话。
// 探针实测控制客户端不能自带命令编号，所以列表走 CLI（tmux ls），
// 这不是偷懒，是唯一可靠的路。
func TestListSessionsForReconcile(t *testing.T) {
	tmuxReady(t)
	a := newTestSession(t, SessionOpts{})
	b := newTestSession(t, SessionOpts{})
	names, err := ListSessions(testBin, "lp-test-")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, s := range []*Session{a, b} {
		if !have[s.Name()] {
			t.Fatalf("ListSessions 漏了 %s，得到 %v", s.Name(), names)
		}
	}
	// 前缀过滤：非前缀的会话不得混进来
	for _, n := range names {
		if !strings.HasPrefix(n, "lp-test-") {
			t.Fatalf("前缀过滤失效: %v", names)
		}
	}
}

// TestHistoryLimitApplied：HistoryLimit 要真的生效。探针实测
// new-session **之前** set -g 与建完之后 set -t 都可行，这里钉住行为。
func TestHistoryLimitApplied(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{HistoryLimit: 1234})
	got := tmuxOut(t, "display-message", "-t", s.Name(), "-p", "#{history_limit}")
	if strings.TrimSpace(got) != "1234" {
		t.Fatalf("history-limit 应为 1234, got %q", got)
	}
}

// TestCommandErrorSurfaces：命令失败要让调用方拿到错误与 tmux 原文。
//
// 用「目标 pane 不存在」而不是「不存在的子命令」：实测未知子命令报的是
// "parse error: unknown command: xxx"，而**我们的参数被引用过**，拼出的
// 整行可能以引号开头，tmux 会把它当一个未知"命令名"，错误信息就会把整
// 行拼进去、很难读。pane 找不到是运行期错误，%error 内容干净。
func TestCommandErrorSurfaces(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{})
	_, err := s.Run(context.Background(), "capture-pane", "-p", "-e", "-S", "-1",
		"-t", "lp-nope-does-not-exist")
	if err == nil {
		t.Fatal("不存在的目标应报错")
	}
	if !strings.Contains(err.Error(), "can't find") {
		t.Fatalf("错误里应带 tmux 原文, got %v", err)
	}
	// 出错后连接必须还能用：状态机若没复位，之后的每条命令都会卡到超时
	if _, err := s.CaptureAll(context.Background()); err != nil {
		t.Fatalf("出错后 Capture 不再工作: %v", err)
	}
}

// TestReadySignal 钉住两件事：握手块结束的信号会来；Ready 之后**立刻**
// 发命令就能拿到响应（不用 sleep）。没有 Ready 时，先发出去的命令会被
// tmux 的握手块顶掉响应关联（见 corr），而 attach 前产生的输出也不会回
// 放给这条连接 —— 上层要靠它决定"从这一刻起才算实时流"。
func TestReadySignal(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{})
	select {
	case <-s.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("Ready 没信号：握手块检测失效")
	}
	if err := s.SendText(context.Background(), "echo AFTER-READY\n"); err != nil {
		t.Fatal(err)
	}
	expect(t, s, 5*time.Second, "Ready 后的输出", outputContains("AFTER-READY"))
}

// TestTwoControlClientsSameScreen：两个控制连接 attach 同一会话，
// 一个发的输出另一个也要收到（PC + 手机同屏的实现基础）。
func TestTwoControlClientsSameScreen(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{})
	s2, err := Attach(testBin, s.Name())
	if err != nil {
		t.Fatalf("第二个 Attach: %v", err)
	}
	defer s2.Close()

	// 两条连接都必须先就绪：control attach **不回放**历史输出（重连靠
	// capture），在 Ready 之前发的字这条连接永远看不到
	waitReady(t, s, "第一个连接")
	waitReady(t, s2, "第二个连接")

	mark := fmt.Sprintf("TWO-%d", time.Now().UnixNano()%100000)
	if err := s.SendText(context.Background(), "echo done-"+mark+"\n"); err != nil {
		t.Fatal(err)
	}
	// 等执行产物：命令自身的回显里也有 mark，等它两个连接都会"假通过"
	want := outputContains("done-" + mark)
	expect(t, s, 5*time.Second, "第一个连接收到", want)
	expect(t, s2, 5*time.Second, "第二个连接收到", want)
}

// TestReattachAfterServerRestart 不在这里测（要杀 tmux server，会牵连
// 同机其他测试）；对账逻辑本身由 ListSessions 覆盖。

func waitReady(t *testing.T, s *Session, who string) {
	t.Helper()
	select {
	case <-s.Ready():
	case <-time.After(5 * time.Second):
		t.Fatalf("%s 的 control 连接没就绪", who)
	}
}

func sessionExists(bin, name string) bool {
	out, err := exec.Command(bin, "has-session", "-t", name).Output()
	_ = out
	return err == nil
}

func tmuxOut(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(testBin, args...)
	// 集成测试统一用独立 socket？——不，Termux 上 TMUX_TMPDIR 已是私有
	// 目录，测试会话名带时间戳，不与人共用。
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v (%s)", args, err, out)
	}
	return string(out)
}

func headOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestReplayBytesRestoresCursor 钉住重连回放的最后一个字节。
//
// 只把 capture-pane 的文本喂给新终端是不够的：capture 每行都以换行结尾，
// 喂完之后客户端光标停在"最后一行之后的行首"，而 tmux 里光标其实停在
// 提示符中间（下一条命令还没回车）。两者不一致时，用户看到的是"画面正常，
// 但打一个字就整屏错位"。所以回放串必须以一条绝对定位（CUP）结尾。
func TestReplayBytesRestoresCursor(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{Cols: 80, Rows: 24})

	// 不带换行的输出：光标确定地停在行中间，而不是行首
	mark := fmt.Sprintf("TAIL%d", time.Now().UnixNano()%100000)
	if err := s.SendText(context.Background(), "printf "+mark); err != nil {
		t.Fatal(err)
	}
	expect(t, s, 5*time.Second, "无换行输出", outputContains(mark))

	// tmux 自己认为光标在哪（列 0 起、行 0 起）
	got := tmuxOut(t, "display-message", "-t", s.Name(), "-p",
		"#{cursor_x} #{cursor_y}")
	var wantX, wantY int
	if _, err := fmt.Sscanf(strings.TrimSpace(got), "%d %d", &wantX, &wantY); err != nil {
		t.Fatalf("读光标失败 %q: %v", got, err)
	}
	if wantX == 0 {
		t.Fatalf("测试前提不成立：光标应在行中间, got x=%d", wantX)
	}

	replay, err := s.ReplayBytes(context.Background())
	if err != nil {
		t.Fatalf("ReplayBytes: %v", err)
	}
	if !strings.Contains(string(replay), mark) {
		t.Fatal("回放串里少了屏幕内容")
	}
	// 结尾必须是 CUP 到 tmux 认为的位置（CUP 是 1 基，格式串要 +1）
	want := fmt.Sprintf("\x1b[%d;%dH", wantY+1, wantX+1)
	if !strings.HasSuffix(string(replay), want) {
		t.Fatalf("回放串结尾必须恢复光标到 %q, 实际尾部 %q", want,
			headOf(string(replay[len(replay)-40:]), 40))
	}
}

// TestReplayBytesResizesFirst：回放的行映射依赖"窗口行数 == 客户端行数"
// （pane 第 r 行才会落在客户端第 r 行）。所以 ReplayBytes 内部要先按
// 当前上报尺寸对齐，否则有历史时定位会整体偏几行。
func TestReplayBytesCapturesWholeScreen(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{Cols: 80, Rows: 24})
	if err := s.SendText(context.Background(), "clear; echo LINE-AFTER-CLEAR\n"); err != nil {
		t.Fatal(err)
	}
	expect(t, s, 5*time.Second, "clear 后的输出", outputContains("LINE-AFTER-CLEAR"))
	replay, err := s.ReplayBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(replay), "LINE-AFTER-CLEAR") {
		t.Fatal("回放缺内容")
	}
	// 行数必须凑满一屏：capture 会补满 pane_height 行，靠这一点
	// "最后一行 == pane 最后一行" 才成立，行号映射才对得上
	if n := strings.Count(strings.TrimSuffix(string(replay), "\r\n"), "\r\n"); n < 23 {
		t.Fatalf("回放不满一屏（%d 行），光标行号映射会偏: %q", n, headOf(string(replay), 120))
	}
}

// TestInterruptStopsForegroundCommand：Ctrl-C 必须能用。
//
// 这是"只支持普通命令"这个简化里**不能**跟着一起砍掉的部分：`apt update`
// 跑歪了、`ping` 忘了加 -c，都靠它停。而且它在 control mode 下不是免费的：
// 键盘上的 Ctrl-C 是字节 0x03，必须以原始控制字节 send-keys -H 送进去
// （送字面文本 "^C" 只会打出两个字符）。
//
// 怎么判定"真的中断了"而不是"看起来中断了"：让 shell 算一道算术题。
// 我们**发出的字符**里含 "r$((6*7))"，只有被 shell 执行后才会出现 "r42"。
// 若 Ctrl-C 没生效，sleep 仍占着前台，回车的字符只是被 tty 回显，永远不
// 会得到 r42。
func TestInterruptStopsForegroundCommand(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{})
	ctx := context.Background()

	// 先确认 pane 里的 shell 真的能执行命令。这一步不能省：tty 的 ECHO 是
	// 行规程做的，**shell 还没起来时输入也会被回显**，所以"看到 sleep 30
	// 被打出来"并不代表 bash 已经消费了这行。少了这个握手，Ctrl-C 会抢在
	// sleep 启动之前到达，测试就变成赌时序。
	if err := s.SendText(ctx, "echo ready$((1+1))\n"); err != nil {
		t.Fatal(err)
	}
	expect(t, s, 8*time.Second, "shell 可执行命令", outputContains("ready2"))

	if err := s.SendText(ctx, "sleep 30\n"); err != nil {
		t.Fatal(err)
	}
	// pane_current_command 由 tmux 维护：它变成 sleep 才说明 sleep 真的在跑
	waitPaneCommand(t, s.Name(), "sleep", 8*time.Second)

	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	if err := s.SendText(ctx, "echo r$((6*7))\n"); err != nil {
		t.Fatal(err)
	}
	expect(t, s, 8*time.Second, "shell 恢复提示符并执行新命令", outputContains("r42"))
}

// TestInterruptSendsControlByteNotText：确认送进去的是控制字节，不是字面
// 的 "^C" 两个字符。
//
// 不能在屏幕上找/排除 "^C" —— readline 收到 SIGINT 时**本来就会**在提示符
// 上打出 `^C`（实测确认），所以"屏幕里没有 ^C"是一条从一开始就不成立的断言，
// 它只会产出假失败。
//
// 换一个与渲染无关的观测量：让前台程序是 `cat > 文件`。
//   - 送字节 0x03：终端行规程（ISIG）拦下它，cat 收到 SIGINT 死掉，文件为空；
//   - 送字面文本 "^C"：cat 只会把这两个字符当普通输入写进文件。
//
// 于是"文件为空"就证明了走的是控制字节。cat 本身由 tmux 的
// #{pane_current_command} 确认已经起来，不靠猜时序。
func TestInterruptSendsControlByteNotText(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{})
	ctx := context.Background()

	if err := s.SendText(ctx, "echo ready$((1+1))\n"); err != nil {
		t.Fatal(err)
	}
	expect(t, s, 8*time.Second, "shell 可执行命令", outputContains("ready2"))

	target := filepath.Join(t.TempDir(), "cat-out")
	if err := s.SendText(ctx, fmt.Sprintf("cat > %s\n", target)); err != nil {
		t.Fatal(err)
	}
	waitPaneCommand(t, s.Name(), "cat", 8*time.Second)

	if err := s.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	waitPaneCommand(t, s.Name(), "bash", 8*time.Second)

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读不到 cat 的输出文件: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("Ctrl-C 被当成普通文本喂给了前台程序，文件里是 %q", data)
	}
}

// TestSecondClientSeesRunningCommandAndKeepsStreaming 直接编码需求原话：
// "我在电脑上执行了一个命令，换到手机上必须还能看到命令执行的情况"。
//
// "换到手机"在实现上就是**命令正在跑的时候新开一跳控制连接**：它必须先
// 看到已经打过的那些行（回放），还要继续收到之后的新行（实时流）。两个
// 条件缺一不可，所以分开断言，不写成"最后能看到就行"。
func TestSecondClientSeesRunningCommandAndKeepsStreaming(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{Cols: 80, Rows: 24})

	mark := fmt.Sprintf("T%d", time.Now().UnixNano()%100000)
	// 每 0.2 秒打一行、共 40 行：给第二条连接留出"中途接入"的窗口
	if err := s.SendText(context.Background(),
		fmt.Sprintf("for i in $(seq 1 40); do echo %s-$i; sleep 0.2; done\n", mark)); err != nil {
		t.Fatal(err)
	}
	// 确认已经打了前几行
	expect(t, s, 5*time.Second, "计数已开始", outputContains(mark+"-3"))

	late := mustAttach(t, s.Name(), "中途接入的设备")
	t.Cleanup(func() { _ = late.Close() })
	waitReady(t, late, "第二条连接")

	// 1) 回放：接入之前打的行必须在 ReplayBytes 里
	replay, err := late.ReplayBytes(context.Background())
	if err != nil {
		t.Fatalf("ReplayBytes: %v", err)
	}
	if !strings.Contains(replay, mark+"-1") {
		t.Fatalf("中途接入看不到已打出的历史行: 回放尾部 %q",
			headOf(replay, 200))
	}

	// 2) 实时：接入**之后**才打的行必须从事件流里到达
	//    （回放与实时流的分工就在这里：一个管过去，一个管将来）
	expect(t, late, 10*time.Second, "接入之后的新输出", func(e Event) bool {
		return e.Kind == EvOutput && strings.Contains(e.Data, mark+"-3")
	})
}

// mustAttach 中途接入一条已存在的会话（模拟换设备）。同样带 OOM 重试。
func mustAttach(t *testing.T, name, who string) *Session {
	t.Helper()
	var last error
	for i := 0; i < 2; i++ {
		s, err := Attach(testBin, name)
		if err == nil {
			return s
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s 接入失败: %v", who, last)
	return nil
}

// waitPaneCommand 轮询 tmux 的 #{pane_current_command}，直到前台程序变成 want。
// 这是"程序真的在跑"的独立观测量 —— 不依赖输出内容，也就不受 tty 提前回显
// 的影响（回显是行规程干的，不代表 shell 已经消费输入）。
func waitPaneCommand(t *testing.T, name, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		out, err := exec.Command(testBin, "display-message", "-p", "-t", name,
			"#{pane_current_command}").Output()
		if err == nil && strings.TrimSpace(string(out)) == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("前台程序没变成 %q（在等 %s）", want, d)
}

// 机器上装着 tmux、但 server 还没起过（全新机器、或机器重启后）时
// 列会话必须回"没有会话"，不是报错。
//
// tmux 在这两种情况下说的话完全不同：
//   - server 起着、会话被清空：  "no sessions"
//   - socket 都不存在（从没起过）："error connecting to <path> (No such file
//     or directory)"
//
// 只认前者的话，面板第一次启动就是 500 —— 而那次启动恰恰是用户第一次
// 打开面板。实测 tmux 3.7c 给的是后者。
func TestListSessionsWithoutServer(t *testing.T) {
	tmuxReady(t)
	dir := t.TempDir()
	t.Setenv("TMUX_TMPDIR", dir) // 私有空目录：那里必定没有 server

	names, err := ListSessions(testBin, TmuxPrefix)
	if err != nil {
		t.Fatalf("没起过 server 该回空列表, got err=%v", err)
	}
	if len(names) != 0 {
		t.Fatalf("该回空列表, got %v", names)
	}

	// HasSession 同理：不能把"问不到"报成错误，否则删除路径上
	// 一个不存在的会话会变成 500 而不是"已经没了"
	live, err := HasSession(testBin, "lp-1")
	if err != nil {
		t.Fatalf("没起过 server 时 HasSession 该回 (false,nil), got err=%v", err)
	}
	if live {
		t.Fatal("不可能有会话")
	}
}
