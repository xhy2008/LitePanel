package download

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"litepanel/internal/store"
)

// 本地历史表存在的唯一理由（实测 dev/aria2hist）：aria2 重启后 tellStopped
// 一条记录都不留。所以这张表不是缓存，而是"下载历史"这个功能的唯一载体。
//
// 它同时立下一条对偶的约束：**进行中的状态归 aria2 独占，表里不镜像**。
// 否则会有两个真相来源（aria2 侧被别的客户端改动时必然漂移），而且每个进度
// tick 都要写一次 SQLite，正好违背本里程碑"面板开销极低"的承诺。下面
// TestProgressTickWritesNothing 专门钉这一条。

// fixedClock 让 created_at/finished_at 可预期（同 fs_jobs 的做法）。
func fixedClock() time.Time { return time.Unix(1700000000, 0) }

type taskEnv struct {
	t   *testing.T
	svc *TaskStore
	db  *store.DB
}

func newTaskEnv(t *testing.T) *taskEnv {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &taskEnv{t: t, db: db, svc: NewTaskStore(db, fixedClock)}
}

func (e *taskEnv) add(t *testing.T, gid, uri string) *Record {
	t.Helper()
	rec, err := e.svc.Add(context.Background(), Submission{
		URIs: []string{uri}, GID: gid, Dir: "/DISK/downloads", Name: "a.bin",
	})
	if err != nil {
		t.Fatalf("Add(%s): %v", gid, err)
	}
	return rec
}

func (e *taskEnv) get(gid string) (*Record, error) { return e.svc.Get(context.Background(), gid) }

// 提交必须立刻落库。
//
// "先落库再返回"不是洁癖：aria2 收下任务后面板就崩了，那一笔下载在历史上
// 就永远不会出现 —— 而用户看到的是"我明明下过这个文件"。
func TestAddPersistsSubmission(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0x1", "https://example.com/a.bin")
	got, err := e.get("0x1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.URIs) != 1 || got.URIs[0] != "https://example.com/a.bin" ||
		got.Dir != "/DISK/downloads" || got.Name != "a.bin" {
		t.Errorf("提交时的事实要存全: %+v", got)
	}
	if got.State != StateActive {
		t.Errorf("刚提交的默认状态应是 active, got %s", got.State)
	}
}

// 多个镜像地址要整体存下来。用逗号拼接再拆是常见写法，而 URL 的查询串里
// 逗号是合法字符（?a=1,2），拆回去就把用户填的地址改掉了。
func TestMultipleURIsStoredAsSet(t *testing.T) {
	e := newTaskEnv(t)
	uris := []string{"https://a/x.iso?r=1,2", "https://b/x.iso"}
	if _, err := e.svc.Add(context.Background(), Submission{URIs: uris, GID: "0xm"}); err != nil {
		t.Fatal(err)
	}
	got, err := e.get("0xm")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.URIs) != 2 || got.URIs[0] != uris[0] {
		t.Errorf("镜像地址被改写了: %#v", got.URIs)
	}
}

// 终态只写一次，且不能被后来的事件倒退。
//
// 与轮询器那边同一条规则，但在持久层更关键：历史是用户事后回看的唯一依据，
// 一条 complete 被后来的 error 覆盖，用户就会以为文件坏了（而它其实好好的）。
func TestTerminalStateDoesNotRegress(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xg", "https://example.com/g.bin")
	if err := e.svc.SetTerminal(context.Background(), "0xg", StateComplete, "", 1000, 1000); err != nil {
		t.Fatal(err)
	}
	// 迟到的 error（aria2 内部清理顺序、或轮询与事件的竞态）不得覆盖。
	if err := e.svc.SetTerminal(context.Background(), "0xg", StateError, "boom", 0, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := e.get("0xg")
	if got.State != StateComplete {
		t.Errorf("已完成不该被改成 %s（历史会误导用户以为文件坏了）", got.State)
	}
	if got.Error != "" {
		t.Errorf("不该留下错误文本: %q", got.Error)
	}
	if got.TotalBytes != 1000 {
		t.Errorf("完成时的大小要留住, got %d", got.TotalBytes)
	}
}

func TestTerminalRecordsFinishedAt(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xf", "https://example.com/f.bin")
	if err := e.svc.SetTerminal(context.Background(), "0xf", StateError, "磁盘满", 10, 3); err != nil {
		t.Fatal(err)
	}
	got, _ := e.get("0xf")
	if got.State != StateError || got.Error != "磁盘满" {
		t.Errorf("失败原因要能查回来: %+v", got)
	}
	if got.FinishedAt == 0 {
		t.Error("终态必须带完成时刻（历史列表要按它排序）")
	}
	if got.CompletedBytes != 3 {
		t.Errorf("失败时已下多少也是信息, got %d", got.CompletedBytes)
	}
}

// 进度 tick 一个字节都不许写。
//
// 这是"进行中状态归 aria2 独占"的可执行版本：轮询器每秒推一次进度，若实现
// 顺手把它们写库，就是每秒一次 SQLite 写 + 一个会与 aria2 漂移的副本。断言
// 用"跑了很多轮之后各行仍与提交时一致"，而不是去看有没有 UPDATE 语句 ——
// 后者要靠注入钩子，而前者就是用户在乎的性质。
func TestProgressTickWritesNothing(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xp", "https://example.com/p.bin")
	before, _ := e.get("0xp")
	for i := 0; i < 50; i++ {
		e.svc.ApplyProgress([]Progress{{GID: "0xp", TotalBytes: 999, DoneBytes: 555, Speed: 12}})
	}
	after, _ := e.get("0xp")
	if after.TotalBytes != before.TotalBytes || after.CompletedBytes != before.CompletedBytes {
		t.Errorf("进度被写进库了: %+v → %+v", before, after)
	}
	if after.State != before.State {
		t.Errorf("进度不该改状态: %s → %s", before.State, after.State)
	}
	// 但界面要能拿到实时进度 —— 它活在内存里，不经过库。
	snap := e.svc.ProgressSnapshot()
	if len(snap) != 1 || snap[0].DoneBytes != 555 {
		t.Errorf("实时进度要能从内存读到: %#v", snap)
	}
}

// 历史必须活过重启 —— 这正是这张表的全部意义（实测 aria2 跨重启不保留任何
// 终态记录）。用"同一个 db 文件、新的 TaskStore"来模拟面板重启。
func TestHistorySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	s1 := NewTaskStore(db, fixedClock)
	if _, err := s1.Add(context.Background(), Submission{URIs: []string{"https://x/a"}, GID: "0xa"}); err != nil {
		t.Fatal(err)
	}
	if err := s1.SetTerminal(context.Background(), "0xa", StateComplete, "", 5, 5); err != nil {
		t.Fatal(err)
	}

	// 换一个 TaskStore 实例（面板重启），读同一份库。
	s2 := NewTaskStore(db, fixedClock)
	items, err := s2.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].State != StateComplete {
		t.Fatalf("历史必须在重启后还在, got %+v", items)
	}
}

// 清除历史只能清终态。
//
// 一句 DELETE FROM downloads 会把正在下载的任务也抹掉 —— 而 aria2 那边还在
// 下，界面上却再也找不到它（进度、暂停按钮全没了），用户只能等它下完再去
// 文件管理器里找这个凭空出现的文件。
func TestClearHistoryKeepsUnfinished(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xrun", "https://x/running")
	e.add(t, "0xdone", "https://x/done")
	if err := e.svc.SetTerminal(context.Background(), "0xdone", StateComplete, "", 1, 1); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.ClearHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应只清掉那条已完成的, got %d", n)
	}
	items, _ := e.svc.List(context.Background(), ListFilter{})
	if len(items) != 1 || items[0].GID != "0xrun" {
		t.Errorf("进行中的任务不能被动: %+v", items)
	}
}

// 历史有条数上限（与 fs_jobs 同理：无上限的表在常年运行后只会拖慢列表查询，
// 而用户根本翻不到那么远）。
func TestHistoryCapped(t *testing.T) {
	e := newTaskEnv(t)
	for i := 0; i < MaxHistory+40; i++ {
		gid := string(rune('a'+i%26)) + string(rune('a'+i/26)) + string(rune('a'+i%7))
		if _, err := e.svc.Add(context.Background(), Submission{
			URIs: []string{"https://x/" + gid}, GID: gid,
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.SetTerminal(context.Background(), gid, StateComplete, "", 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	items, err := e.svc.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) > MaxHistory {
		t.Errorf("历史必须封顶在 %d 条, got %d", MaxHistory, len(items))
	}
}

// 封顶裁剪时不能删掉进行中的任务。
func TestCapOnlyTrimsTerminal(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xkeep", "https://x/keep") // 永远 active
	for i := 0; i < MaxHistory+10; i++ {
		gid := "done" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i%19))
		if _, err := e.svc.Add(context.Background(), Submission{URIs: []string{"u"}, GID: gid}); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.SetTerminal(context.Background(), gid, StateComplete, "", 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.get("0xkeep"); err != nil {
		t.Errorf("裁剪历史时把进行中的任务删了: %v", err)
	}
}

// 事件可能对应一条面板没提交过的任务（别人用 aria2 的 Web UI 或
// aria2rpc 加的任务，或面板重装过）。不能报错，也不能凭空造一条 uri 为空的
// 历史记录糊在列表里。
func TestEventForUnknownGidIsNotInvented(t *testing.T) {
	e := newTaskEnv(t)
	if err := e.svc.SetTerminal(context.Background(), "0xghost", StateComplete, "", 1, 1); err != nil {
		t.Fatalf("未知 gid 不该报错（事件流会因此中断）: %v", err)
	}
	items, _ := e.svc.List(context.Background(), ListFilter{})
	if len(items) != 0 {
		t.Errorf("不该为没提交过的 gid 造历史记录: %+v", items)
	}
}

// 面板重启后，aria2 里已经不存在的"进行中"记录必须收口。
//
// 不收口的后果是历史列表里永远挂着几条"下载中"，而它们不会再有任何更新
// （aria2 跨重启不保留任务，实测）。收口的判据由调用方给：aria2 还在跑的
// 要保留（aria2 活着而面板重启，是正常场景）。
func TestReconcileClosesOrphans(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xalive", "https://x/alive")
	e.add(t, "0xgone", "https://x/gone")
	e.add(t, "0xfin", "https://x/fin")
	if err := e.svc.SetTerminal(context.Background(), "0xfin", StateComplete, "", 1, 1); err != nil {
		t.Fatal(err)
	}
	alive := map[string]bool{"0xalive": true}
	n, err := e.svc.Reconcile(context.Background(), func(gid string) bool { return alive[gid] })
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("只该收口那条没了的, got %d", n)
	}
	gone, _ := e.get("0xgone")
	if !gone.State.IsTerminal() {
		t.Errorf("aria2 里没了的记录不能永远显示下载中, got %s", gone.State)
	}
	if gone.Error == "" {
		t.Error("要说明为什么被收口（否则用户以为下载坏了）")
	}
	still, _ := e.get("0xalive")
	if still.State.IsTerminal() {
		t.Errorf("aria2 还在跑的任务不能被收口: %+v", still)
	}
	done, _ := e.get("0xfin")
	if done.State != StateComplete {
		t.Errorf("已完成的终态不该被动, got %s", done.State)
	}
}

// 列表按状态分组（下载中 / 等待中 / 已完成含错误）。
func TestListFiltersByStateGroup(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xa", "https://x/a")
	e.add(t, "0xb", "https://x/b")
	e.add(t, "0xc", "https://x/c")
	if err := e.svc.SetState(context.Background(), "0xb", StatePaused); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetTerminal(context.Background(), "0xc", StateComplete, "", 1, 1); err != nil {
		t.Fatal(err)
	}
	active, err := e.svc.List(context.Background(), ListFilter{States: []State{StateActive}})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].GID != "0xa" {
		t.Errorf("按状态筛不对: %+v", active)
	}
	all, _ := e.svc.List(context.Background(), ListFilter{})
	if len(all) != 3 {
		t.Errorf("不带筛选应返回全部, got %d", len(all))
	}
}

func TestAddRejectsEmptyInput(t *testing.T) {
	e := newTaskEnv(t)
	if _, err := e.svc.Add(context.Background(), Submission{URIs: nil, GID: "0xe"}); err == nil {
		t.Error("没有地址的任务不该存在")
	}
	if _, err := e.svc.Add(context.Background(), Submission{URIs: []string{"u"}, GID: ""}); err == nil {
		t.Error("没有 gid 无法跟踪，不该落库")
	}
}

// 可空列（name/dir/error/finished_at）为 NULL 时，整张列表必须还能读出来。
//
// 这条是写这段代码时真踩到的：把 NULL 直接扫进 string 会报
// "converting NULL to string is unsupported"，而 List 的语义是"给我历史"——
// 一行没填文件名的记录（aria2 从 URL 推断名字失败时就是这样）让整页 500，
// 等于用户的所有历史都看不见了。
func TestNullableColumnsDoNotBreakList(t *testing.T) {
	e := newTaskEnv(t)
	// 绕开 Add，直接插一行什么都没填的（模拟脏数据/旧数据/别的写入方）。
	if _, err := e.db.SqlDB().Exec(
		`INSERT INTO downloads(gid,uri,state,created_at) VALUES('0xnull','["https://x"]','complete',1)`); err != nil {
		t.Fatal(err)
	}
	items, err := e.svc.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("一行 NULL 不该让整张列表读不出来: %v", err)
	}
	if len(items) != 1 || items[0].Name != "" || items[0].FinishedAt != 0 {
		t.Errorf("NULL 应降级成零值: %+v", items)
	}
	// 单条读同样不能失败。
	if _, err := e.svc.Get(context.Background(), "0xnull"); err != nil {
		t.Errorf("Get 也要能读: %v", err)
	}
}

// 地址字段坏了（不是合法 JSON）时列表要能读出来，只是那条没有地址。
// 与上一条同一条原则：读路径"少显示一点"永远好过"整页报错"。
func TestCorruptURIJsonDoesNotBreakList(t *testing.T) {
	e := newTaskEnv(t)
	if _, err := e.db.SqlDB().Exec(
		`INSERT INTO downloads(gid,uri,state,created_at) VALUES('0xbad','不是JSON','complete',1)`); err != nil {
		t.Fatal(err)
	}
	items, err := e.svc.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].URIs) != 0 {
		t.Errorf("坏数据应降级成空地址而不是报错: %+v", items)
	}
}

// SetState 不能拿来写终态：终态必须带原因与大小，走 SetTerminal 才有那些参数。
// 允许它写终态等于开了一条"绕过不倒退保证"的后门。
func TestSetStateRejectsTerminalStates(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xq", "https://x/q")
	if err := e.svc.SetState(context.Background(), "0xq", StateComplete); err == nil {
		t.Error("SetState 不该能写终态")
	}
	if err := e.svc.SetState(context.Background(), "0xq", StatePaused); err != nil {
		t.Errorf("非终态应该能写: %v", err)
	}
}

// 并发写同一条不能倒退（读-改-写在应用层挡不住，必须靠 SQL 的条件）。
func TestConcurrentTerminalWritesDoNotRegress(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xc", "https://x/c")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, reason := StateComplete, ""
			if i%2 == 0 {
				st, reason = StateError, "boom"
			}
			_ = e.svc.SetTerminal(context.Background(), "0xc", st, reason, 9, 9)
		}(i)
	}
	wg.Wait()
	got, err := e.get("0xc")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateComplete && got.State != StateError {
		t.Fatalf("状态必须是其中之一, got %s", got.State)
	}
	// 关键判据：写进去的是哪一个都行，但**必须与 error 字段自洽** ——
	// complete 配着 "boom" 就是两个主人互相踩过的痕迹。
	if got.State == StateComplete && got.Error != "" {
		t.Errorf("状态与原因不自洽（被并发写踩过了）: %+v", got)
	}
	if got.State == StateError && got.Error == "" {
		t.Errorf("error 状态必须带原因: %+v", got)
	}
}

// SetState 在 SQL 里的终态守卫不是重复劳动，Go 层的类型检查够不着它。
//
// 真实竞态：用户点"暂停"→ 面板调 aria2.pause → 网络往返期间任务恰好下完，
// complete 事件先到并落了库 → pause 的响应回来，面板照惯例 SetState(paused)。
// paused 不是终态，Go 层的 if st.IsTerminal() 放行；只有 SQL 里那句
// AND state NOT IN (终态) 挡得住。它一旦被删，历史里会出现"已完成"被改回
// "已暂停"，用户点继续时 aria2 回"任务不存在"，一圈排查。
func TestSetStateCannotUncompleteARace(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xrace", "https://x/race")
	ctx := context.Background()
	// 完成先到。
	if err := e.svc.SetTerminal(ctx, "0xrace", StateComplete, "", 10, 10); err != nil {
		t.Fatal(err)
	}
	// 暂停的回答后到 —— 语义上是"迟到的旧状态"。
	if err := e.svc.SetState(ctx, "0xrace", StatePaused); err != nil {
		t.Fatal(err) // 这一步本身不该报错（乐观处理）
	}
	got, _ := e.get("0xrace")
	if got.State != StateComplete {
		t.Errorf("迟到的 paused 不得覆盖 complete, got %s", got.State)
	}
}

// 收口用的状态与文案是有契约的：必须是 error + 说明"去检查目标目录"。
//
// 换成 removed/静默 或 只写"下载失败"，用户的下一步动作完全不同：前者会
// 以为任务从没存在过，后者会立刻重下一遍 —— 而文件很可能已经完整地躺在
// 目标目录里（面板重启前它其实下完了，只是 aria2 没来得及留下记录）。
func TestReconcileOrphanReasonSendsUserToRightAction(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xorph", "https://x/orph")
	if _, err := e.svc.Reconcile(context.Background(), func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	got, _ := e.get("0xorph")
	if got.State != StateError {
		t.Errorf("收口必须是 error（可重下、可在历史里筛出来）, got %s", got.State)
	}
	if got.Error == "" || !strings.Contains(got.Error, "目录") {
		t.Errorf("文案必须引导用户先去目标目录确认, got %q", got.Error)
	}
}

// 非终态被塞给 SetTerminal 必须当场拒绝。
// 真发生（比如状态映射写错）时，宁可panic式地报错在调用点，也不要留下一条
// "state=active 且 finished_at 有值"的记录 —— 那种半吊子行在列表里既不归
// "下载中"也不归"已完成"，前端只能整组丢掉。
func TestSetTerminalRejectsNonTerminal(t *testing.T) {
	e := newTaskEnv(t)
	e.add(t, "0xnt", "https://x/nt")
	if err := e.svc.SetTerminal(context.Background(), "0xnt", StateActive, "", 1, 1); err == nil {
		t.Error("SetTerminal 必须拒绝非终态")
	}
	got, _ := e.get("0xnt")
	if got.FinishedAt != 0 {
		t.Error("被拒绝的调用不得留下 finished_at")
	}
}
