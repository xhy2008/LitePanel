package terminal

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// M5-T6 的ServiceImpl：真 tmux + 真库。
//
// API 层那套用替身的测试回答的是"HTTP 语义对不对"；这里回答的是
// "库里记的与 tmux 里真实发生的是不是同一件事" —— 那正是面板最容易
// 自说自话的地方。

func newServiceEnv(t *testing.T) (*Service, context.Context) {
	t.Helper()
	if _, err := exec.LookPath(DefaultBin); err != nil {
		t.Skipf("没有 tmux: %v", err)
	}
	db := openTestDB(t)
	return NewService(db, DefaultBin), context.Background()
}

func svcID(t *testing.T, s *Service, title string) SessionMeta {
	t.Helper()
	meta, err := s.Create(context.Background(), SessionInput{Title: title})
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, meta.TmuxName) })
	return meta
}

func tmuxHas(name string) bool {
	return exec.Command(DefaultBin, "has-session", "-t", name).Run() == nil
}

func waitForSvc(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(80 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// 创建的会话必须**同时**存在于库里与 tmux 里，且名字是 lp-<id>。
// 只查库等于什么都没测（tmux 那半边才是用户真正在用的东西）。
func TestServiceCreateMakesRealTmuxSession(t *testing.T) {
	svc, ctx := newServiceEnv(t)
	meta, err := svc.Create(ctx, SessionInput{Title: "编译内核", HistoryLimit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	defer KillSession(DefaultBin, meta.TmuxName)

	if !tmuxHas(meta.TmuxName) {
		t.Fatalf("tmux 里没有 %s", meta.TmuxName)
	}
	if meta.TmuxName != TmuxName(meta.ID) {
		t.Fatalf("tmux 名应与 id 绑定: %s vs %d", meta.TmuxName, meta.ID)
	}
	// 历史上限真的落到 tmux 会话上了吗（不是只写进了库）
	out, err := exec.Command(DefaultBin, "show-options", "-t", meta.TmuxName,
		"-v", "history-limit").Output()
	if err != nil {
		t.Fatalf("问不到 history-limit: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "5000" {
		t.Fatalf("tmux 侧 history-limit=%s，要 5000", got)
	}
}

// tmux 侧会话消失（用户在 tmux 里 exit）后，List 必须把 alive 刷成 false。
// 这是"面板说的与真实一致"的核心一条：库里那格 alive 只是缓存。
func TestServiceListReflectsTmuxReality(t *testing.T) {
	svc, ctx := newServiceEnv(t)
	meta := svcID(t, svc, "会被我杀掉")

	items, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !items[0].Alive {
		t.Fatalf("初始应是在线: %+v", items)
	}

	if err := KillSession(DefaultBin, meta.TmuxName); err != nil {
		t.Fatal(err)
	}
	waitForSvc(t, "alive 变 false", func() bool {
		items, err := svc.List(ctx)
		if err != nil {
			return false
		}
		return len(items) == 1 && !items[0].Alive
	})

	// 行还在：会话"已退出"是一个可解释的状态，不是被面板吞掉了
	items, _ = svc.List(ctx)
	if len(items) != 1 || items[0].Title != "会被我杀掉" {
		t.Fatalf("已退出的会话行应保留: %+v", items)
	}
}

// 用户在 tmux 里手工建的 lp- 前缀会话，Reconcile 要收编，
// 且**必须用 tmux 名里的数字当 id** —— 用自增 id 会让 tmux_name 与
// lp-<id> 分叉，之后按 id 反推会话名就会指向另一个 tmux 会话。
func TestServiceReconcileAdoptsExternalSession(t *testing.T) {
	svc, ctx := newServiceEnv(t)
	const id = 4242
	name := TmuxName(id)
	if out, err := exec.Command(DefaultBin, "new-session", "-d", "-s", name).CombinedOutput(); err != nil {
		t.Fatalf("造外部会话失败: %v (%s)", err, out)
	}
	defer KillSession(DefaultBin, name)

	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := GetSessionMeta(svc.db, id)
	if err != nil {
		t.Fatalf("外部会话没被收编: %v", err)
	}
	if got.TmuxName != name {
		t.Fatalf("收编后 tmux_name 必须是 %s（id 与名字必须一一对应）, got %s", name, got.TmuxName)
	}
	items, _ := svc.List(ctx)
	if len(items) != 1 || !items[0].Alive {
		t.Fatalf("收编的会话应在线: %+v", items)
	}
}

// 面板重启（新 Service 实例）后再 Reconcile 必须幂等。
func TestServiceReconcileIsIdempotent(t *testing.T) {
	svc, ctx := newServiceEnv(t)
	svcID(t, svc, "一次就好")

	for i := 0; i < 3; i++ {
		if err := NewService(svc.db, DefaultBin).Reconcile(ctx); err != nil {
			t.Fatalf("第 %d 次对账失败: %v", i+1, err)
		}
	}
	items, _ := ListSessionsMeta(svc.db)
	if len(items) != 1 {
		t.Fatalf("重复对账多出会话行: %+v", items)
	}
}

// 删除要同时清掉 tmux 会话与库里的行。
func TestServiceDeleteRemovesBoth(t *testing.T) {
	svc, ctx := newServiceEnv(t)
	meta := svcID(t, svc, "删掉我")

	if err := svc.Delete(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	if tmuxHas(meta.TmuxName) {
		t.Fatal("tmux 会话还在")
	}
	if _, err := GetSessionMeta(svc.db, meta.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("库里的行应被删掉, got %v", err)
	}
}

// 删除一个 tmux 里已经不存在的会话（用户自己 exit 过）：应成功而不是报错。
// 用户要的结果是"这行没了"，而它确实没了。
func TestServiceDeleteAlreadyGoneSucceeds(t *testing.T) {
	svc, ctx := newServiceEnv(t)
	meta := svcID(t, svc, "我自己会 exit")
	if err := KillSession(DefaultBin, meta.TmuxName); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, meta.ID); err != nil {
		t.Fatalf("删除已消失的会话应成功, got %v", err)
	}
	if _, err := GetSessionMeta(svc.db, meta.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("僵尸行应被清掉, got %v", err)
	}
}

func TestServiceDeleteUnknownIsNotFound(t *testing.T) {
	svc, ctx := newServiceEnv(t)
	if err := svc.Delete(ctx, 99999); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("应回 ErrSessionNotFound（API 据此回 404）, got %v", err)
	}
}

// Attach 记录 last_attached_at：侧栏按"最近使用"排序靠它。
func TestServiceAttachTouches(t *testing.T) {
	svc, _ := newServiceEnv(t)
	meta := svcID(t, svc, "接入我")

	sess, err := svc.Attach(context.Background(), meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	got, err := GetSessionMeta(svc.db, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastAttachedAt == 0 {
		t.Fatal("last_attached_at 没记录")
	}
	if _, err := svc.Attach(context.Background(), 99999); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("未知 id 应 404 级错误, got %v", err)
	}
}

// tmux 问不到的时候不能谎报。返回空列表会被前端渲染成"服务器上没有任何
// 终端会话"，与真相（tmux 用不了）完全相反。
func TestServiceListFailsLoudlyWithoutTmux(t *testing.T) {
	db := openTestDB(t)
	svc := NewService(db, "/nonexistent/tmux-not-here")
	if _, err := svc.List(context.Background()); err == nil {
		t.Fatal("tmux 不可用时 List 必须报错，不能回空列表")
	}
}

// 库与 tmux 之间不许出现"库里说创建成功、tmux 里什么都没有"。
// 用一个必定失败的 tmux 创建（非法 cwd）来检查回滚。
func TestServiceCreateRollsBackWhenTmuxFails(t *testing.T) {
	db := openTestDB(t)
	svc := NewService(db, DefaultBin)
	if _, err := exec.LookPath(DefaultBin); err != nil {
		t.Skipf("没有 tmux: %v", err)
	}
	// 本测试的库是临时的、随用例销毁，但 **tmux 侧不是**：某些 tmux 版本
	// 会退回 $HOME 而真的把会话建出来（下面就走那条 skip 分支）。必须清掉：
	// 每个用例用的都是新库，第一个会话名总是 lp-1，泄漏一个 lp-1 就会让
	// 后面任意一个建会话的用例撞上 duplicate session —— 那种失败看起来
	// 与本用例毫无关系，只会让人从头查一遍。
	// 名字可推算：这个库是新建的，Create 拿到 rowid 1 → lp-1。
	t.Cleanup(func() { _ = KillSession(DefaultBin, TmuxName(1)) })
	_, err := svc.Create(context.Background(), SessionInput{
		Title: "建不成的", Cwd: "/definitely/not/a/real/directory/xyzzy",
	})
	if err == nil {
		// 某些 tmux 版本会退回 $HOME 而不是报错；没有失败就没什么可断言的
		t.Skip("tmux 接受了非法 cwd，本用例无从验证回滚")
	}
	items, _ := ListSessionsMeta(db)
	if len(items) != 0 {
		t.Fatalf("tmux 创建失败后库里不该留行: %+v", items)
	}
}

// tmux 的 -t 是前缀匹配：只有 lp-10 存在时 `has-session -t lp-1` 也成功
// （下面第一件事就是把这个前提本身测出来，否则整条测试在测空气）。
// 不钉住它，删除 lp-1 可能经由前缀匹配把 lp-10 杀掉 —— 用户看到的是
// "删了 A，结果 B 没了"。
func TestHasSessionIsExactNotPrefix(t *testing.T) {
	if _, err := exec.LookPath(DefaultBin); err != nil {
		t.Skipf("没有 tmux: %v", err)
	}
	const shortName, longName = "lp-1", "lp-10"
	for _, n := range []string{shortName, longName} {
		_ = exec.Command(DefaultBin, "kill-session", "-t", "="+n).Run()
	}
	if out, err := exec.Command(DefaultBin, "new-session", "-d", "-s", longName).CombinedOutput(); err != nil {
		t.Fatalf("造 %s 失败: %v (%s)", longName, err, out)
	}
	defer exec.Command(DefaultBin, "kill-session", "-t", "="+longName).Run()

	// 前提：裸 -t 确实会误命中；否则本测试没在保护任何东西
	if exec.Command(DefaultBin, "has-session", "-t", shortName).Run() != nil {
		t.Skip("本 tmux 版本的 -t 不做前缀匹配，精确匹配无需保护")
	}

	ok, err := HasSession(DefaultBin, shortName)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("HasSession(%s) 被前缀匹配骗到了（实际只有 %s 存在）", shortName, longName)
	}
	if ok, err := HasSession(DefaultBin, longName); err != nil || !ok {
		t.Fatalf("存在的会话应报 true: ok=%v err=%v", ok, err)
	}
}

// 创建会话之后不许留一条控制连接。
//
// CreateSession 返回的 *Session 是一条 `tmux -CC attach` 进程 + 一个读泵。
// REST 建会话时如果把它攥着不关，每条创建都会泄漏一个客户端：没人读它的
// Events，缓冲填满后解码协程永久阻塞，进程和内存都不退。桥接那边会在浏览器
// 订阅时自己 attach 一条，所以这里必须关掉。
//
// 这条是被装配测试（"两个标签共用一条控制连接"数到 2）反向暴露出来的。
func TestServiceCreateLeavesNoControlConnection(t *testing.T) {
	svc, ctx := newServiceEnv(t)
	meta := svcID(t, svc, "别泄漏")

	// 等可能存在的迟到进程退出，再数：给泄漏一点暴露时间
	time.Sleep(500 * time.Millisecond)
	if n := tmuxClientCount(t, meta.TmuxName); n != 0 {
		t.Fatalf("创建后残留 %d 条控制连接（应为 0，浏览器订阅时由桥接自己接）", n)
	}
	_ = ctx
}

// 对账同样不许留连接：它只是"看看 tmux 里有什么"。
//
// 这里的会话用 exec 直接造（不经 Create），否则测到的是 Create 的泄漏，
// 两条测试就会同生同死，对账自己是否干净永远没被单独验过。
func TestServiceReconcileLeavesNoControlConnection(t *testing.T) {
	svc, ctx := newServiceEnv(t)
	name := TmuxName(777001)
	if out, err := exec.Command(DefaultBin, "new-session", "-d", "-s", name).CombinedOutput(); err != nil {
		t.Fatalf("造会话失败: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, name) })

	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := tmuxClientCount(t, name); n != 0 {
		t.Fatalf("对账后残留 %d 条控制连接（应为 0：对账只查询，不接管）", n)
	}
}

func tmuxClientCount(t *testing.T, name string) int {
	t.Helper()
	out, err := exec.Command(DefaultBin, "list-clients", "-t", "="+name).Output()
	if err != nil {
		return 0 // 问不到 = 没有任何客户端
	}
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

// 删 id=2 的会话时，绝不能杀掉 lp-20。
//
// tmux 的 -t 只在"存在精确匹配"时优先精确；名字已经不存在、而恰好有个
// 前缀邻居时，`kill-session -t lp-2` 就打到 lp-20 上。组合起来是完全可达
// 的：库里的行还留着（用户直接在 tmux 里 exit 了会话），面板上点删除，
// 于是删掉了另一个人的会话 —— 而且是不可逆的。
//
// 上层 Delete 会先 HasSession（精确）挡住这条路，但 KillSession 自己
// 必须也是精确的：否则任何新调用点都自带这个坑。
func TestKillSessionIsExactNotPrefix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = ctx

	neigh := TmuxName(20) // 与被杀名构成前缀关系：lp-2 vs lp-20
	stale := TmuxName(2)
	out, err := exec.Command(DefaultBin, "new-session", "-d", "-s", neigh).CombinedOutput()
	if err != nil {
		t.Fatalf("造邻居会话失败: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = KillSession(DefaultBin, neigh) })

	// stale 从来没建过（正是"库里有、tmux 里没有"的那一步）
	if err := KillSession(DefaultBin, stale); err == nil {
		t.Log("KillSession 对不存在的目标报了成功（可接受），但绝不能顺手杀邻居")
	}
	live, err := HasSession(DefaultBin, neigh)
	if err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Fatalf("KillSession(%s) 误杀了前缀邻居 %s", stale, neigh)
	}
}
