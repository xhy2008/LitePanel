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

// 接管的外部会话也必须补上 remain-on-exit。
//
// 漏掉它的后果不是"少个选项"而是判错死因：Reconcile 收编的会话
// （手工起的 lp-*）没开这个选项，shell 正常 exit 时会话整个消失，
// 死因判定读不到尸体 → 被归进"异常消失"，一排正常退出的会话全
// 变成要人手动清理的僵尸记录 —— 恰好是用户最反感的那种。
func TestReconcileArmsAdoptedSession(t *testing.T) {
	tmuxReady(t)
	svc, ctx := newServiceEnv(t)
	const id = 9301
	name := TmuxName(id)
	runTMUX(t, "new-session", "-d", "-s", name)
	defer runTMUX(t, "kill-session", "-t", name)

	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// show-options 的 -t 不认 = 精确前缀（实测报 no such window），
	// 这里用裸名：私有 socket 的测试环境里没有 lp-9301x 兄弟名可撞。
	out := strings.TrimSpace(runTMUXQuiet(t,
		"show-options", "-t", name, "remain-on-exit"))
	if !strings.Contains(out, "on") {
		t.Fatalf("收编的会话没被补上 remain-on-exit: %q", out)
	}
}

// ---- 死因清理（Service.List 的尸检规程）----
//
// 规则（用户裁决）：status==0 自动删除不留痕迹；status!=0 与凭空消失
// 都是"异常"，保留现场直到用户手动删除；server 连不上是"未知"，
// 什么都不动。List 每 3 秒被前端轮询一次，这就是清理的执行点。
//
// 隔离说明：整个测试包共用一个 tmux server（TMUX_TMPDIR 在 TestMain
// 里指到临时目录），所以这些测试**不许** kill-server —— 会把别的
// 测试正在用的会话一起杀掉。每个测试用 t.Cleanup 清掉自己的会话
// （t.Fatal 之后 Cleanup 照跑），失败的测试不会往后面漏 lp-<id> 尸体。

// newSweepEnv 建一个会话并注册清理。
func newSweepEnv(t *testing.T, title string) (*Service, context.Context, SessionMeta) {
	t.Helper()
	svc, ctx := newServiceEnv(t)
	meta, err := svc.Create(ctx, SessionInput{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, meta.TmuxName) })
	return svc, ctx, meta
}

// waitCorpse 等 shell 真正死掉（尸体出现即可；删除是 List 的事）
func waitCorpse(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		out := strings.TrimSpace(runTMUXQuiet(t,
			"list-panes", "-t", "="+name, "-F", "#{pane_dead}"))
		if strings.HasPrefix(out, "1") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等不到 %s 的尸体", name)
}

// 规则 1：正常退出（status 0）—— 行、尸体、记录全部消失。
func TestListAutoDeletesCleanExit(t *testing.T) {
	tmuxReady(t)
	svc, ctx, meta := newSweepEnv(t, "自动清理")
	waitPrompt(t, meta.TmuxName) // waitPrompt 跑 true → 裸 exit 继承 0 退出码
	runTMUX(t, "send-keys", "-t", meta.TmuxName+":.0", "exit", "Enter")
	// 等尸体落定再 List（真实场景由 3 秒轮询自己撞上，这里要确定性）
	waitCorpse(t, meta.TmuxName)

	items, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("正常退出的会话必须从列表消失, got %+v", items)
	}
	if _, err := GetSessionMeta(svc.db, meta.ID); err == nil {
		t.Fatal("库里还留着正常退出的行 —— 用户要求不留任何多余痕迹")
	}
	// 不能用 HasSession 判尸体：探针实测 has-session 对尸体也返回 0。
	// 尸体清没清干净要问 list-sessions —— 名字不再出现才算没了。
	out := runTMUXQuiet(t, "list-sessions", "-F", "#{session_name}")
	if strings.Contains(out, meta.TmuxName) {
		t.Fatalf("tmux 里的尸体也得清掉: %s", out)
	}
}

// 规则 2：异常退出（status!=0）—— 保留、带退出码、反复 List 幂等
// （轮询每 3 秒撞一次 List，幂等不是奢侈品而是必需品）。
func TestListKeepsAbnormalExit(t *testing.T) {
	tmuxReady(t)
	svc, ctx, meta := newSweepEnv(t, "炸了")
	waitPrompt(t, meta.TmuxName)
	runTMUX(t, "send-keys", "-t", meta.TmuxName+":.0", "exit 7", "Enter")
	waitCorpse(t, meta.TmuxName)

	for round := 1; round <= 2; round++ {
		items, err := svc.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("第%d轮 异常会话必须保留, got %+v", round, items)
		}
		if items[0].Alive {
			t.Fatal("异常会话不该是活的")
		}
		if items[0].ExitStatus == nil || *items[0].ExitStatus != 7 {
			t.Fatalf("退出码必须是 7（前端要显示死因）, got %v", items[0].ExitStatus)
		}
	}
}

// 规则 3：会话凭空消失（外部 kill 掉这一个会话）也是异常，
// 用 ExitVanished(-1) 和真实退出码区分 —— 两者都要人手动清。
// 前提：别的会话还活着，server 还在 —— 才能把"这个会话没了"
// 和"tmux 整体问不到"分开（后者见下一条测试）。
func TestListVanishedCountsAbnormal(t *testing.T) {
	tmuxReady(t)
	svc, ctx, meta := newSweepEnv(t, "被杀")
	witness, err := svc.Create(ctx, SessionInput{Title: "证人"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, witness.TmuxName) })

	if err := KillSession(DefaultBin, meta.TmuxName); err != nil {
		t.Fatal(err)
	}
	items, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var victim *SessionMeta
	for i := range items {
		if items[i].ID == meta.ID {
			victim = &items[i]
		}
	}
	if victim == nil {
		t.Fatal("异常消失的行必须保留")
	}
	if victim.Alive {
		t.Fatal("被外部杀掉的会话不该是活的")
	}
	if victim.ExitStatus == nil || *victim.ExitStatus != ExitVanished {
		t.Fatalf("凭空消失应记 ExitVanished(-1), got %v", victim.ExitStatus)
	}
}

// 规则 4：tmux 整体问不到是"未知"，不是"死了" —— 报错但什么都不动。
// 这条防的是最恶心的连锁：tmux 暂时不可达（升级中/socket 抖动）时
// 把全部会话标记成异常，用户醒来一排僵尸记录等着手动删。
//
// 怎么制造"问不到"而不 kill-server（共享服务器杀不得）：bin 指向
// 一个不存在的路径。生产里等价物是 tmux 被卸载/socket 目录被清。
func TestListUnreachableTouchedNothing(t *testing.T) {
	tmuxReady(t)
	svc, ctx, meta := newSweepEnv(t, "幸存")

	svc.bin = "/nonexistent/tmux-nope"
	if _, err := svc.List(ctx); err == nil {
		t.Fatal("tmux 不可达时 List 必须报错（前端要显示'未知'而不是空列表）")
	}
	if _, err := GetSessionMeta(svc.db, meta.ID); err != nil {
		t.Fatalf("未知状态下不许删行: %v", err)
	}
}

// 真实崩溃模拟：面板与 tmux 一起死（CI 式 kill-server），退出码必须
// 已经落进库 —— 重启后尸体没了，死因也不能退化成"未知消失"。
// 唯一允许 kill-server 的测试：见上面的隔离说明；跑完 server 是空的，
// 后面的用例自己会把它拉起来（tmuxReady）。
func TestExitCodeSurvivesFullCrash(t *testing.T) {
	tmuxReady(t)
	svc, ctx, meta := newSweepEnv(t, "崩")
	waitPrompt(t, meta.TmuxName)
	runTMUX(t, "send-keys", "-t", meta.TmuxName+":.0", "exit 9", "Enter")
	waitCorpse(t, meta.TmuxName)
	if _, err := svc.List(ctx); err != nil {
		t.Fatal(err) // 这一次 List 要把 9 持久化
	}
	runTMUX(t, "kill-server") // 面板没来得及看，server 也没了（真实崩溃序）

	// 重启后 server 没了：会话只活在 server 内存里，server 没了就是
	// 确凿的死（不是"未知"）。这一行的死因在第一次闻到尸体时就落了
	// 库，此刻必须原样读得出来 —— 不能退化成 -1"凭空消失"。
	items, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ExitStatus == nil || *items[0].ExitStatus != 9 {
		t.Fatalf("退出码必须在第一次观察时就落库, got %+v", items)
	}
}

// 规则 0：socket 指错 → 全体误标"消失"→ 配置修好后必须能自愈。
//
// 场景在真实世界是真的：TMUX_TMPDIR 配错/被清一次，一轮轮询就把
// 全部行标成异常消失；之后就算 tmux"回来"了，没有复活逻辑的话这些
// 行也永远停在 alive=0 —— 面板谎报到有人手动删为止。复活是"标消失"
// 能选可恢复方向的前提。
func TestReviveAfterVanished(t *testing.T) {
	tmuxReady(t)
	svc, ctx, meta := newSweepEnv(t, "会复活")

	// 制造一轮"server 没了"（不真杀共享 server：bin 指向不存在的 socket
	// —— tmux 的 socket-path 参数可以做到，但最省的是直接翻库模拟上一轮
	// 的产物：alive=0 + -1。真实写入路径由 markAllVanished 的调用测试覆盖）
	if err := SetSessionDeath(svc.db, meta.ID, false, ptrOf(ExitVanished)); err != nil {
		t.Fatal(err)
	}
	items, err := svc.List(ctx) // tmux 好好活着，名字也在 → 必须复活
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !items[0].Alive {
		t.Fatalf("名字重新出现必须复活, got %+v", items)
	}
	if items[0].ExitStatus != nil {
		t.Fatalf("复活后死因必须清空（别让运行中的会话挂着旧退出码）, got %v", *items[0].ExitStatus)
	}
	// 库里也得是复活态（内存对了库没对 = 下一轮又翻车）
	got, err := GetSessionMeta(svc.db, meta.ID)
	if err != nil || !got.Alive || got.ExitStatus != nil {
		t.Fatalf("库没刷成复活态: %+v err=%v", got, err)
	}
}

func ptrOf(v int) *int { return &v }

// 真实世界最常见的"server 没了"：最后一个会话被外部 kill-session，
// tmux 顺手把 server 也退了。此时库里还记着 alive=1 的行必须改判
// 消失 —— 不标的话面板永远谎报"会话在线"，点进去才是"没这个会话"。
func TestMarkAllVanishedOnServerExit(t *testing.T) {
	tmuxReady(t)
	svc, ctx, meta := newSweepEnv(t, "最后一个")
	other, err := svc.Create(ctx, SessionInput{Title: "陪葬"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, other.TmuxName) })

	// 一次杀光会话：最后一个倒下时 server 整个退出（tmux 的固有行为）
	_ = KillSession(DefaultBin, other.TmuxName)
	_ = KillSession(DefaultBin, meta.TmuxName)

	items, err := svc.List(ctx) // server 已没了 → ErrNoServer 分支
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Alive {
			t.Fatalf("server 没了还报 alive: %+v", it)
		}
		if it.ExitStatus == nil || *it.ExitStatus != ExitVanished {
			t.Fatalf("server 没了应记 ExitVanished, got %+v", it)
		}
	}
	if len(items) != 2 {
		t.Fatalf("行要留着（可复活 + 显示历史），got %+v", items)
	}
}

// Reconcile（启动对账）也必须能复活 —— List 那条只在运行期跑，
// 而"面板重启后满屏异常消失"正是启动这一瞬间最容易出现的观感。
func TestReconcileRevivesRows(t *testing.T) {
	tmuxReady(t)
	svc, ctx := newServiceEnv(t)
	meta, err := svc.Create(ctx, SessionInput{Title: "重启幻觉"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, meta.TmuxName) })

	if err := SetSessionDeath(svc.db, meta.ID, false, ptrOf(ExitVanished)); err != nil {
		t.Fatal(err) // 模拟"上一次启动时 tmux 恰好不在"
	}
	if err := NewService(svc.db, DefaultBin).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := GetSessionMeta(svc.db, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Alive || got.ExitStatus != nil {
		t.Fatalf("tmux 里明明活着，对账必须复活并清死因: %+v", got)
	}
}

// 遗言回放（异常退出的 tab 点进去看最后的输出）。
//
// 实测的两块地基：
//   - capture-pane 在尸体上读得到历史（dev/hold）；
//   - capture-pane 的 -t **不认 = 精确前缀**（实测 "can't find pane:
//     =cap1"）—— 只能裸名，而裸名是前缀匹配。所以必须先 HasSession
//     （=name，精确）确认，再裸名 capture：两步之间名字被释放又恰好
//     被 lp-1 撞上 lp-10 的概率可以忽略，而盲 capture 是必错的。
func TestCorpseOutputReadable(t *testing.T) {
	tmuxReady(t)
	svc, ctx := newServiceEnv(t)
	meta, err := svc.Create(ctx, SessionInput{Title: "留话"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, meta.TmuxName) })

	waitPrompt(t, meta.TmuxName)
	runTMUX(t, "send-keys", "-t", meta.TmuxName+":.0", "echo 临终遗言XYZ", "Enter")
	time.Sleep(300 * time.Millisecond)
	runTMUX(t, "send-keys", "-t", meta.TmuxName+":.0", "exit 5", "Enter")
	waitCorpse(t, meta.TmuxName)

	txt, err := svc.CorpseOutput(ctx, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txt, "临终遗言XYZ") {
		t.Fatalf("遗言必须读得到, got %q", txt)
	}
}

// 会话消失（尸体被 kill-server 毁了）：没有历史可显示，回空串而不是
// 报错 —— 前端 tab 还在（库里有行），页面显示"无输出记录"比弹错误对。
func TestCorpseOutputGoneIsEmpty(t *testing.T) {
	tmuxReady(t)
	svc, ctx := newServiceEnv(t)
	meta, err := svc.Create(ctx, SessionInput{Title: "无话可说"})
	if err != nil {
		t.Fatal(err)
	}
	if err := KillSession(DefaultBin, meta.TmuxName); err != nil {
		t.Fatal(err)
	}
	txt, err := svc.CorpseOutput(ctx, meta.ID)
	if err != nil {
		t.Fatalf("消失的会话回空串即可, got err=%v", err)
	}
	if txt != "" {
		t.Fatalf("应该没内容, got %q", txt)
	}
}

// 库里没有的 id 必须报 ErrSessionNotFound（API 映射成 404）。
// 绝不拿编造的 tmux 名去问 tmux。
func TestCorpseOutputUnknownID(t *testing.T) {
	tmuxReady(t)
	svc, ctx := newServiceEnv(t)
	if _, err := svc.CorpseOutput(ctx, 424242); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("未知 id 应报 ErrSessionNotFound, got %v", err)
	}
}
