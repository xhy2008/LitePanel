package download

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"litepanel/internal/store"
)

// Service 把两个真相来源合成一张列表：
//
//   - **进行中**归 aria2（tellActive/tellWaiting + 轮询来的速度）；
//   - **已完成/已失败**归本地表（实测 aria2 跨重启一条终态记录都不留）。
//
// 两条合成规则最容易写错，各有测试：
//   1. 同一个 gid 同时出现在两边时，**不能出两条**（aria2 尚未把它从
//      tellActive 摘掉、而完成事件已落库，这个窗口是常态）；
//   2. 本地表已是终态的，**不能被 aria2 的 active 倒回去** —— 与轮询器里
//      同一条规则，否则列表会在"完成/下载中"之间抖。

// fakeRPC 是 aria2 的可编程替身（只实现 Service 用到的那几个方法）。
type fakeRPC struct {
	mu       sync.Mutex
	active   []Status
	waiting  []Status
	paused   []Status
	stopped  []Status
	stat     map[string]*Status
	listErr  error
	paused2  map[string]bool
	addGID   string
	addErr   error
	lastOpts Options
	lastURIs []string
	version  string
	forced   []string
}

func (f *fakeRPC) TellActive(context.Context) ([]Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Status(nil), f.active...), f.listErr
}

func (f *fakeRPC) TellWaiting(context.Context) ([]Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Status(nil), f.waiting...), f.listErr
}

func (f *fakeRPC) GetGlobalStat(context.Context) (*GlobalStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &GlobalStat{
		DownloadSpeed: "1024",
		NumActive:     itoa(len(f.active)),
		NumWaiting:    itoa(len(f.waiting)),
		NumStopped:    itoa(len(f.stopped)),
	}, nil
}

func (f *fakeRPC) AddURI(_ context.Context, uris []string, o Options) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.lastURIs = append([]string(nil), uris...)
	f.lastOpts = o
	return f.addGID, nil
}

func (f *fakeRPC) Pause(_ context.Context, gid string) error {
	if !f.present(gid) {
		return aria2Absent()
	}
	return nil
}
func (f *fakeRPC) Resume(_ context.Context, gid string) error {
	if !f.present(gid) {
		return aria2Absent()
	}
	return nil
}

// Remove 模拟 aria2 的实情：已完成的任务它不留，因此对这类 gid 会报错。
// 这条行为必须做出来，否则 Service.Remove 里"忽略 not present"的分支永远
// 测不到 —— 而它直接关系到"用户能不能删掉自己已完成的历史条目"。
func (f *fakeRPC) Remove(_ context.Context, gid string) error {
	if !f.present(gid) {
		return aria2Absent()
	}
	f.mu.Lock()
	var left []Status
	for _, s := range f.active {
		if s.GID != gid {
			left = append(left, s)
		}
	}
	f.active = left
	f.mu.Unlock()
	return nil
}
func (f *fakeRPC) ForceRemove(ctx context.Context, gid string) error {
	f.mu.Lock()
	f.forced = append(f.forced, gid)
	f.mu.Unlock()
	return f.Remove(ctx, gid)
}

// TellStatus 模拟 aria2 实情：只对**还认得**的 gid 返回状态，其余报业务错
// （"Download not present."）。终态落库时要靠它抓最后一次大小，这个替身不做
// 出来，"查不到就落 0 而不是报错"那条分支就测不到。
func (f *fakeRPC) TellStatus(_ context.Context, gid string) (*Status, error) {
	if !f.present(gid) {
		return nil, aria2Absent()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range f.active {
		if st.GID == gid {
			cp := st
			return &cp, nil
		}
	}
	for _, st := range f.waiting {
		if st.GID == gid {
			cp := st
			return &cp, nil
		}
	}
	return nil, aria2Absent()
}

func (f *fakeRPC) GetVersion(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, f.listErr
}

func (f *fakeRPC) present(gid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.active {
		if s.GID == gid {
			return true
		}
	}
	for _, s := range f.waiting {
		if s.GID == gid {
			return true
		}
	}
	return false
}

// aria2 对不存在的 gid 回的就是这句话；替身照做一个**业务错**而不是
// unavailable：两者的状态码完全不同（4xx vs 503）。
func aria2Absent() error {
	return &Error{Code: 1, Message: "Download not present."}
}

func itoa(n int) string { return strconv.Itoa(n) }

func (f *fakeRPC) setActive(g ...Status) {
	f.mu.Lock()
	f.active = g
	f.mu.Unlock()
}

type svcEnv struct {
	svc   *Service
	rpc   *fakeRPC
	tasks *TaskStore
	dir   string
}

func newSvcEnv(t *testing.T) *svcEnv {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(base, "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rpc := &fakeRPC{addGID: "0xnew", version: "1.37.0"}
	tasks := NewTaskStore(db, fixedClock)
	s := NewService(rpc, tasks, ServiceOptions{})
	return &svcEnv{svc: s, rpc: rpc, tasks: tasks, dir: base}
}

func (e *svcEnv) submit(t *testing.T, gid, name string) {
	t.Helper()
	if _, err := e.tasks.Add(context.Background(), Submission{
		URIs: []string{"https://example.com/" + name}, GID: gid, Name: name, Dir: e.dir,
	}); err != nil {
		t.Fatal(err)
	}
}

// 进行中的任务速度来自 aria2，name/dir 来自本地表。
//
// aria2 的 tellActive 不带用户填的 out/dir（要 tellStatus 才有），而历史列表
// 要显示"下的是哪个文件、存到哪"。合成才两边都不缺。
func TestTasksMergeLiveAndLocal(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "movie.mkv")
	e.rpc.setActive(Status{GID: "0xa", Status: "active",
		TotalLength: "1000", CompletedLength: "300", DownloadSpeed: "512", Connections: "16"})
	view, err := e.svc.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 1 {
		t.Fatalf("应 1 条, got %d", len(view))
	}
	got := view[0]
	if got.Name != "movie.mkv" {
		t.Errorf("name 要从本地表补, got %q", got.Name)
	}
	if got.Speed != 512 || got.DoneBytes != 300 || got.Connections != 16 {
		t.Errorf("进度要来自 aria2: %+v", got)
	}
	if got.State != "active" {
		t.Errorf("state 不对: %s", got.State)
	}
}

// 同一个 gid 两边都有时只出一条 —— 这个窗口是常态而非异常：完成事件先落库，
// aria2 下一轮才把它从 tellActive 摘掉。出两条会让界面上出现两张同一任务的卡
// 片，一张"完成"一张"99%"。
func TestTasksDedupeAcrossSources(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "a.bin")
	e.rpc.setActive(Status{GID: "0xa", Status: "active", CompletedLength: "900"})
	view, _ := e.svc.Tasks(context.Background())
	if len(view) != 1 {
		t.Fatalf("应 1 条, got %d", len(view))
	}
}

// 本地表已是终态的，不得被 aria2 的 active 倒回去。
// 与轮询器同一条规则：抖动的列表比晚一帧的列表更难解释。
func TestTerminalRecordNotResurrectedAsActive(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "a.bin")
	if err := e.tasks.SetTerminal(context.Background(), "0xa", StateComplete, "", 10, 10); err != nil {
		t.Fatal(err)
	}
	e.rpc.setActive(Status{GID: "0xa", Status: "active", CompletedLength: "3"})
	view, _ := e.svc.Tasks(context.Background())
	if len(view) != 1 {
		t.Fatalf("应 1 条, got %d", len(view))
	}
	if view[0].State != "complete" {
		t.Errorf("终态不能被 active 覆盖, got %s", view[0].State)
	}
}

// aria2 里在跑、但本地表没有的任务也要显示。
//
// 场景是别的客户端（aria2 的 Web UI、aria2rpc 脚本）提交的任务。不显示的话
// 用户会以为"我的下载不见了"，而它其实正在占着带宽。
func TestTasksIncludeForeignGids(t *testing.T) {
	e := newSvcEnv(t)
	e.rpc.setActive(Status{GID: "0xforeign", Status: "active", CompletedLength: "5",
		TotalLength: "10", DownloadSpeed: "7"})
	view, err := e.svc.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 1 {
		t.Fatalf("外部任务也要显示, got %d", len(view))
	}
	if view[0].GID != "0xforeign" || view[0].Speed != 7 {
		t.Errorf("外部任务的进度要拿得到: %+v", view[0])
	}
	if view[0].Name != "" {
		t.Errorf("本地没有 name 时留空即可，别编一个: %q", view[0].Name)
	}
}

// 已完成的历史条目要出现在列表里，且带大小与完成时刻。
//
// 这是本地表存在的全部理由（实测 aria2 重启后终态记录归零）。
func TestTasksIncludeTerminalHistory(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xold", "old.bin")
	if err := e.tasks.SetTerminal(context.Background(), "0xold", StateComplete, "", 4096, 4096); err != nil {
		t.Fatal(err)
	}
	e.rpc.setActive() // aria2 那边啥也没有（或已经重启过）
	view, err := e.svc.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 1 {
		t.Fatalf("历史必须在列表里, got %d", len(view))
	}
	if view[0].State != "complete" || view[0].TotalBytes != 4096 {
		t.Errorf("历史条目不对: %+v", view[0])
	}
}

// aria2 不可达时 Tasks 要报"不可达"而不是回一个只有历史的列表。
//
// 只有历史 = 界面上"没有任何任务在下"，而真相是"aria2 没起来"；用户会去
// 点新建（然后失败）。503 与安装引导靠这个错误才成立。
func TestTasksFailWhenAria2Unavailable(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xh", "h.bin")
	if err := e.tasks.SetTerminal(context.Background(), "0xh", StateComplete, "", 1, 1); err != nil {
		t.Fatal(err)
	}
	e.rpc.listErr = &unavailableError{err: errors.New("connection refused")}
	_, err := e.svc.Tasks(context.Background())
	if err == nil {
		t.Fatal("aria2 不可达必须报错，不能只回历史")
	}
	if !IsUnavailable(err) {
		t.Errorf("要能被判成不可达（handler 据此回 503）: %v", err)
	}
}

// 等待中的任务也算进行中，状态必须是 waiting 而不是 active。
//
// 并发上限默认 5，一次加 10 个任务时有 5 个在排队；把 waiting 显示成 active
// 会让用户以为带宽被 10 个任务分走了（而速度明明只有 5 份）。
func TestTasksReportWaitingSeparately(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "a.bin")
	e.submit(t, "0xb", "b.bin")
	e.rpc.setActive(Status{GID: "0xa", Status: "active"})
	e.rpc.mu.Lock()
	e.rpc.waiting = []Status{{GID: "0xb", Status: "waiting"}}
	e.rpc.mu.Unlock()
	view, _ := e.svc.Tasks(context.Background())
	byGID := map[string]string{}
	for _, v := range view {
		byGID[v.GID] = v.State
	}
	if byGID["0xa"] != "active" || byGID["0xb"] != "waiting" {
		t.Errorf("waiting 与 active 必须区分: %v", byGID)
	}
}

// 汇总的数字要自洽：active+waiting 的数量来自 aria2，done/failed 来自本地表。
//
// 之所以两边取：aria2 的 numStopped 跨重启归零，用它算"已完成 40 个"会在
// 重启后变成 0，界面像是历史被清了。
func TestSummaryCombinesBothSources(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "alive.bin")
	e.rpc.setActive(Status{GID: "0xa", Status: "active"})
	for _, g := range []string{"0xc1", "0xc2"} {
		e.submit(t, g, g)
		if err := e.tasks.SetTerminal(context.Background(), g, StateComplete, "", 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	e.submit(t, "0xe1", "bad.bin")
	if err := e.tasks.SetTerminal(context.Background(), "0xe1", StateError, "boom", 0, 0); err != nil {
		t.Fatal(err)
	}
	s, err := e.svc.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Active != 1 {
		t.Errorf("active 应来自 aria2, got %d", s.Active)
	}
	if s.Done != 2 {
		t.Errorf("done 应来自本地表（aria2 跨重启不保留）, got %d", s.Done)
	}
	if s.Failed != 1 {
		t.Errorf("failed 应来自本地表, got %d", s.Failed)
	}
	if s.Speed != 1024 {
		t.Errorf("速度应来自 getGlobalStat, got %d", s.Speed)
	}
}

func TestHealthProbesViaService(t *testing.T) {
	e := newSvcEnv(t)
	h := e.svc.Health(context.Background())
	if !h.OK || h.Version != "1.37.0" {
		t.Errorf("got %+v", h)
	}
	e.rpc.mu.Lock()
	e.rpc.listErr = &unavailableError{err: errors.New("refused")}
	e.rpc.mu.Unlock()
	// 缓存：换个 Service 才绕开 TTL，这里直接建新实例。
	s2 := NewService(e.rpc, e.tasks, ServiceOptions{})
	h2 := s2.Health(context.Background())
	if h2.OK {
		t.Error("连不上要报不健康")
	}
	if h2.Message == "" {
		t.Error("要带一句给用户的话")
	}
}

// Add 必须"先问 aria2、成功再落库"。
//
// 反过来的顺序（先落库）会在 aria2 拒绝时留下一条永远不动的幽灵记录：历史
// 里挂着一条 active、进度永远是 0、aria2 那边根本没有这个 gid，而且它还会被
// 启动对账收口成"失败"，用户在历史里看到一条自己从没成功提交过的下载。
func TestAddPersistsOnlyAfterAria2Accepts(t *testing.T) {
	e := newSvcEnv(t)
	e.rpc.mu.Lock()
	e.rpc.addErr = &Error{Code: 22, Message: "Resource not found."}
	e.rpc.mu.Unlock()
	_, err := e.svc.Add(context.Background(), AddInput{
		URIs: []string{"https://example.com/x"}, Dir: e.dir, Out: "x.bin",
	})
	if err == nil {
		t.Fatal("aria2 拒绝时 Add 必须报错")
	}
	items, _ := e.tasks.List(context.Background(), ListFilter{})
	if len(items) != 0 {
		t.Errorf("aria2 拒绝时不该留下记录: %+v", items)
	}
}

func TestAddPassesDirNameSplitToAria2(t *testing.T) {
	e := newSvcEnv(t)
	v, err := e.svc.Add(context.Background(), AddInput{
		URIs: []string{"https://example.com/x.iso"}, Dir: e.dir, Out: "x.iso", Split: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.rpc.lastOpts.Dir != e.dir || e.rpc.lastOpts.Out != "x.iso" || e.rpc.lastOpts.Split != 16 {
		t.Errorf("没把参数交给 aria2: %+v", e.rpc.lastOpts)
	}
	if v.GID != "0xnew" || v.Name != "x.iso" {
		t.Errorf("返回视图不对: %+v", v)
	}
	items, _ := e.tasks.List(context.Background(), ListFilter{})
	if len(items) != 1 || items[0].Name != "x.iso" {
		t.Errorf("受理成功后必须落库（否则重启后历史里没有这条）: %+v", items)
	}
}

// 暂停/继续要同时动 aria2 与本地状态。
//
// 只动 aria2：面板重启后本地仍显示 paused（其实用户已经点了继续）；
// 只动本地：aria2 还在下载而界面显示暂停，用户以为按下了暂停。
func TestPauseResumeTouchBothSides(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "a.bin")
	e.rpc.setActive(Status{GID: "0xa", Status: "active"})
	if err := e.svc.Pause(context.Background(), "0xa"); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.tasks.Get(context.Background(), "0xa")
	if rec.State != StatePaused {
		t.Errorf("本地状态要跟着改, got %s", rec.State)
	}
	if err := e.svc.Resume(context.Background(), "0xa"); err != nil {
		t.Fatal(err)
	}
	rec, _ = e.tasks.Get(context.Background(), "0xa")
	if rec.State == StatePaused {
		t.Error("继续后本地不该还是 paused")
	}
}

// 操作一个面板没提交过的 gid 要报 ErrNoTask（→ 404），而不是 500。
//
// 前端列表可能是陈旧的（别的标签页删掉了它），404 才能触发它刷新列表。
func TestPauseUnknownGidIsErrNoTask(t *testing.T) {
	e := newSvcEnv(t)
	err := e.svc.Pause(context.Background(), "0xghost")
	if !errors.Is(err, ErrNoTask) {
		t.Errorf("got %v", err)
	}
}

// Remove 要同时清 aria2 与本地记录，force 要透传。
func TestRemoveClearsBothSides(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "a.bin")
	if err := e.svc.Remove(context.Background(), "0xa", true); err != nil {
		t.Fatal(err)
	}
	rec, err := e.tasks.Get(context.Background(), "0xa")
	if err != nil {
		// 删掉或标成 removed 都算收口；但不能继续显示 active。
		if !errors.Is(err, ErrNoTask) {
			t.Fatal(err)
		}
		return
	}
	if !rec.State.IsTerminal() {
		t.Errorf("移除后不能还在下载中: %+v", rec)
	}
}

func TestClearHistoryDelegates(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "a.bin")
	if err := e.tasks.SetTerminal(context.Background(), "0xa", StateComplete, "", 1, 1); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.ClearHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("got %d", n)
	}
	items, _ := e.tasks.List(context.Background(), ListFilter{})
	if len(items) != 0 {
		t.Errorf("清完应空, got %+v", items)
	}
}

// 启动对账要把 aria2 里已经不存在的"进行中"记录收口，而活着的保留。
//
// 不收口 → 历史里永远挂着几条"下载中"，再也不会有更新（面板重启过，aria2
// 那边跨重启不保留任务）。误收口活着的 → 一个正在跑几十 GB 的任务在界面上
// 变成失败，用户会去重下。
func TestServiceReconcileKeepsAlive(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xalive", "alive.bin")
	e.submit(t, "0xgone", "gone.bin")
	e.rpc.setActive(Status{GID: "0xalive", Status: "active"})
	n, err := e.svc.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应只收口 0xgone, got %d", n)
	}
	alive, _ := e.tasks.Get(context.Background(), "0xalive")
	if alive.State.IsTerminal() {
		t.Errorf("aria2 还在跑的不能收口: %+v", alive)
	}
	gone, _ := e.tasks.Get(context.Background(), "0xgone")
	if !gone.State.IsTerminal() {
		t.Errorf("没了的必须收口: %+v", gone)
	}
}

// 对账时 aria2 是不可达的 → 什么都不收。
//
// 这是最容易写错的一处：aria2 恰好没起来时 tellActive 会失败，若把"查不到"
// 当成"任务不存在"，一次 aria2 重启就会把所有进行中的记录全标成失败。
func TestReconcileDoesNothingWhenAria2Unreachable(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "a.bin")
	e.rpc.mu.Lock()
	e.rpc.listErr = &unavailableError{err: errors.New("refused")}
	e.rpc.mu.Unlock()
	// 两个未完结的记录：一个 aria2 其实在跑（只是这次查不到），一个真的没了。
	// 只造一个的话，"把全部未完结都收口"与"一个也不收"在 n=0/n=1 上能区分，
	// 但"全部收口"与"只收查不到的"区分不了 —— 而那才是真会发生的事故。
	e.submit(t, "0xb", "b.bin")
	n, err := e.svc.Reconcile(context.Background())
	if err == nil {
		t.Errorf("aria2 不可达必须报错（调用方要据此知道对账没做成）, got n=%d", n)
	}
	if n != 0 {
		t.Errorf("连不上时不能收口任何记录, got %d", n)
	}
	for _, gid := range []string{"0xa", "0xb"} {
		rec, _ := e.tasks.Get(context.Background(), gid)
		if rec.State.IsTerminal() {
			t.Errorf("aria2 没起来不等于任务没了 (%s): %+v", gid, rec)
		}
	}
}

// ProgressSnapshot 要把轮询到的进度贴到列表上（前端每 tick 更新进度条靠它）。
func TestProgressExposedOnViews(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "a.bin")
	e.rpc.setActive(Status{GID: "0xa", Status: "active", TotalLength: "100"})
	e.tasks.ApplyProgress([]Progress{{GID: "0xa", TotalBytes: 100, DoneBytes: 42, Speed: 9, Connections: 4}})
	e.svc.SetProgress([]Progress{{GID: "0xa", TotalBytes: 100, DoneBytes: 42, Speed: 9, Connections: 4}})
	view, _ := e.svc.Tasks(context.Background())
	if len(view) != 1 {
		t.Fatalf("got %d", len(view))
	}
	if view[0].DoneBytes != 42 || view[0].Speed != 9 {
		t.Errorf("轮询进度要贴到视图上: %+v", view[0])
	}
}

// 等待中的 gid 也要能被唤醒轮询器 —— 排队任务的进度同样要更新。
func TestWaitingTasksWakePoller(t *testing.T) {
	e := newSvcEnv(t)
	e.rpc.mu.Lock()
	e.rpc.waiting = []Status{{GID: "0xw", Status: "waiting"}}
	e.rpc.mu.Unlock()
	var woke int
	e.svc.OnNeedPoll = func() { woke++ }
	if _, err := e.svc.Tasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if woke == 0 {
		t.Error("有进行中/排队任务时要把轮询器叫醒")
	}
}

// 时间戳不能是 0（前端"完成于 -"很难看，而且排序会乱）。
func TestViewsCarryTimestamps(t *testing.T) {
	e := newSvcEnv(t)
	e.submit(t, "0xa", "a.bin")
	view, _ := e.svc.Tasks(context.Background())
	if view[0].CreatedAt == 0 {
		t.Error("created_at 不能为 0")
	}
	if view[0].FinishedAt != 0 {
		t.Error("没结束时 finished_at 应为 0")
	}
	// 列表按新在前排（历史列表默认按提交顺序倒序）。
	e.submit(t, "0xb", "b.bin")
	view, _ = e.svc.Tasks(context.Background())
	if len(view) < 2 {
		t.Fatalf("got %d", len(view))
	}
	if view[0].GID != "0xb" {
		t.Errorf("新提交应排在前面, got %s", view[0].GID)
	}
}

// 空列表必须是空切片而不是 nil（HTTP 层要 marshal 成 []）。
func TestTasksEmptyIsNotNilSlice(t *testing.T) {
	e := newSvcEnv(t)
	view, err := e.svc.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view == nil {
		t.Error("Tasks 要返回空切片（nil 会让 HTTP 层回 null，前端 .length 抛异常）")
	}
}

// emit 的调用顺序必须是 OnEvent → ViaWS（落库先于推送）。
//
// 前端的用法是"收到事件 → 立刻 GET /dl/tasks"。若推送先到而落库后到，那次
// GET 读到的是旧状态：界面上"完成"跳回"下载中"再变"完成"，像面板在抽风，
// 而且没有任何日志能解释这一帧。顺序写死在 emit 里而不是两个生产者各自保证
// —— 漏一处的表现只在真机上偶发。
func TestEmitOrderPersistsBeforePush(t *testing.T) {
	var log []string
	s := NewService(&fakeRPC{}, nil, ServiceOptions{})
	s.OnEvent = func(Event) { log = append(log, "db") }
	s.ViaWS = func(Event) { log = append(log, "ws") }
	s.emit(Event{Kind: EventComplete})
	if strings.Join(log, ",") != "db,ws" {
		t.Errorf("必须先是落库再是推送, got %v", log)
	}
}

// 只接了一头（或两头都没接）都不能 panic。
//
// ViaWS 不接是"只要 API 不要 WS"的合法装配；OnEvent 不接是只读端点的用法
// （单测、或未来的只读探针）。任何一个漏判都是 nil 解引用，而 emit 在事件
// 循环里 —— 崩一次就把整个事件桥带下去。
func TestEmitToleratesNilHooks(t *testing.T) {
	s := NewService(&fakeRPC{}, nil, ServiceOptions{})
	s.emit(Event{Kind: EventStart})
	s.OnEvent = func(Event) {}
	s.emit(Event{Kind: EventStart})
	s.OnEvent = nil
	s.ViaWS = func(Event) {}
	s.emit(Event{Kind: EventStart})
}
