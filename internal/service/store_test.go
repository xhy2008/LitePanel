package service

import (
	"database/sql"
	"path/filepath"
	"testing"

	"litepanel/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// hasTable 独立查 sqlite_master，不复用被测代码的判断。
func hasTable(t *testing.T, db *store.DB, name string) bool {
	t.Helper()
	var got string
	err := db.SqlDB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// ---- 迁移 ----

func TestMigrationCreatesServiceTables(t *testing.T) {
	db := openDB(t)
	for _, name := range []string{"services", "service_state"} {
		if !hasTable(t, db, name) {
			t.Fatalf("表 %s 不存在", name)
		}
	}
}

// D20：快捷命令改为注入终端执行，历史表被明确取消。
// 库里若混进 exec_history，说明有人把旧设计实现了回来。
func TestNoExecHistoryTable(t *testing.T) {
	db := openDB(t)
	if hasTable(t, db, "exec_history") {
		t.Fatal("exec_history 表存在，违反 D20")
	}
}

// ---- CRUD ----

func TestCreateAndGet(t *testing.T) {
	db := openDB(t)
	svc, err := Create(db, ServiceInput{
		Name: "nginx", Kind: KindCommand, StartCmd: "nginx -g 'daemon off;'", Cwd: "/srv",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if svc.ID == 0 || svc.Name != "nginx" || svc.Kind != KindCommand {
		t.Fatalf("Create 返回异常: %+v", svc)
	}
	if svc.CreatedAt == 0 {
		t.Fatal("CreatedAt 未填")
	}
	got, err := Get(db, svc.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.StartCmd != "nginx -g 'daemon off;'" || got.Cwd != "/srv" {
		t.Fatalf("回读不一致: %+v", got)
	}
	// autostart 走的是 RETURNING 扫描（INTEGER→bool），别只信写入入参。
	if got.Autostart {
		t.Fatal("autostart 默认应为 false")
	}
	// 新建服务必须带一条 stopped 状态行，调用方不必处理"无状态行"分支。
	st, err := GetState(db, svc.ID)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if st.State != StateStopped || st.PID != 0 {
		t.Fatalf("初始状态异常: %+v", st)
	}
}

func TestCreateAutostartRoundTrip(t *testing.T) {
	db := openDB(t)
	svc, err := Create(db, ServiceInput{
		Name: "auto", Kind: KindCommand, StartCmd: "x", Autostart: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := Get(db, svc.ID)
	if !got.Autostart {
		t.Fatal("autostart=true 没存住")
	}
}

func TestNameUnique(t *testing.T) {
	db := openDB(t)
	if _, err := Create(db, ServiceInput{Name: "a", Kind: KindCommand, StartCmd: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(db, ServiceInput{Name: "a", Kind: KindCommand, StartCmd: "true"}); err != ErrDuplicateName {
		t.Fatalf("重名应得 ErrDuplicateName，得 %v", err)
	}
}

// Create 返回值要取自库里的扫描结果，不能回显入参：
// 否则 RETURNING 的列顺序描歪了也测不出。
func TestCreateReturnsPersistedValues(t *testing.T) {
	db := openDB(t)
	off, err := Create(db, ServiceInput{Name: "p", Kind: KindCommand, StartCmd: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if off.Autostart {
		t.Fatal("Create 返回的 autostart 应为库里的 false")
	}
	on, err := Create(db, ServiceInput{Name: "q", Kind: KindCommand, StartCmd: "x", Autostart: true})
	if err != nil {
		t.Fatal(err)
	}
	if !on.Autostart {
		t.Fatal("Create 返回的 autostart 应为库里的 true")
	}
}

// 不存在的 id 必须报错，不能让 UI 拿着“成功”去刷列表。
func TestUpdateMissingRow(t *testing.T) {
	db := openDB(t)
	if err := Update(db, 4242, ServiceInput{
		Name: "ghost", Kind: KindCommand, StartCmd: "x",
	}); err != ErrNotFound {
		t.Fatalf("改不存在的服务应得 ErrNotFound，得 %v", err)
	}
}

func TestDeleteMissingRow(t *testing.T) {
	db := openDB(t)
	if err := Delete(db, 4242); err != ErrNotFound {
		t.Fatalf("删不存在的服务应得 ErrNotFound，得 %v", err)
	}
}

func TestKindValidated(t *testing.T) {
	db := openDB(t)
	// 未知类型必须在入口拒掉：supervisor 拿到会不知道怎么启停。
	if _, err := Create(db, ServiceInput{Name: "x", Kind: "docker", StartCmd: "x"}); err == nil {
		t.Fatal("kind=docker 应被拒绝")
	}
	// command 没有启动命令、systemd 没有单元名，都是废记录。
	if _, err := Create(db, ServiceInput{Name: "y", Kind: KindCommand}); err == nil {
		t.Fatal("command 缺 start_cmd 应被拒绝")
	}
	if _, err := Create(db, ServiceInput{Name: "z", Kind: KindSystemd}); err == nil {
		t.Fatal("systemd 缺 unit 应被拒绝")
	}
}

func TestListOrderedBySortThenName(t *testing.T) {
	db := openDB(t)
	for _, in := range []ServiceInput{
		{Name: "b", Kind: KindCommand, StartCmd: "b", Sort: 10},
		{Name: "c", Kind: KindCommand, StartCmd: "c", Sort: 5},
		{Name: "a", Kind: KindCommand, StartCmd: "a", Sort: 5},
	} {
		if _, err := Create(db, in); err != nil {
			t.Fatal(err)
		}
	}
	list, err := List(db)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "c", "b"} // sort 升序，同 sort 按名称
	if len(list) != len(want) {
		t.Fatalf("List 返回 %d 条, want %d", len(list), len(want))
	}
	for i, w := range want {
		if list[i].Name != w {
			t.Fatalf("第 %d 位 = %s, want %s", i, list[i].Name, w)
		}
	}
}

func TestUpdate(t *testing.T) {
	db := openDB(t)
	svc, _ := Create(db, ServiceInput{Name: "s", Kind: KindCommand, StartCmd: "old"})
	if err := Update(db, svc.ID, ServiceInput{
		Name: "s2", Kind: KindCommand, StartCmd: "new", Cwd: "/tmp",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := Get(db, svc.ID)
	if got.Name != "s2" || got.StartCmd != "new" || got.Cwd != "/tmp" {
		t.Fatalf("Update 未生效: %+v", got)
	}
}

func TestDeleteCascadesState(t *testing.T) {
	db := openDB(t)
	svc, _ := Create(db, ServiceInput{Name: "d", Kind: KindCommand, StartCmd: "d"})
	if err := SaveState(db, svc.ID, State{State: StateRunning, PID: 42}); err != nil {
		t.Fatal(err)
	}
	if err := Delete(db, svc.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var n int
	if err := db.SqlDB().
		QueryRow(`SELECT COUNT(*) FROM service_state WHERE service_id=?`, svc.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("删除服务后 service_state 残留，级联没生效（foreign_keys pragma 掉了？）")
	}
}

// ---- 状态 ----

func TestSaveStateRoundTrip(t *testing.T) {
	db := openDB(t)
	svc, err := Create(db, ServiceInput{Name: "r", Kind: KindCommand, StartCmd: "r"})
	if err != nil {
		t.Fatal(err)
	}
	want := State{
		State:     StateStopped,
		StartedAt: 1000,
		Exit:      &ExitInfo{Code: 137, Signal: 9, Reason: ReasonError, At: 2000, StoppedBy: StoppedByPdeathsig},
	}
	if err := SaveState(db, svc.ID, want); err != nil {
		t.Fatal(err)
	}
	got, err := GetState(db, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateStopped || got.StartedAt != 1000 {
		t.Fatalf("基本字段不一致: %+v", got)
	}
	if got.Exit == nil {
		t.Fatal("退出信息丢失")
	}
	if got.Exit.Code != 137 || got.Exit.Signal != 9 || got.Exit.Reason != ReasonError ||
		got.Exit.At != 2000 || got.Exit.StoppedBy != StoppedByPdeathsig {
		t.Fatalf("退出信息不一致: %+v", got.Exit)
	}
}

// 没有退出过就不该有退出信息：空值不能伪装成「code 0 正常退出」。
func TestStateWithoutExit(t *testing.T) {
	db := openDB(t)
	svc, _ := Create(db, ServiceInput{Name: "n", Kind: KindCommand, StartCmd: "n"})
	if err := SaveState(db, svc.ID, State{State: StateRunning, PID: 7, PGID: 7, StartedAt: 10}); err != nil {
		t.Fatal(err)
	}
	got, _ := GetState(db, svc.ID)
	if got.Exit != nil {
		t.Fatalf("未退出的服务不该带退出信息: %+v", got.Exit)
	}
	if got.PID != 7 || got.PGID != 7 {
		t.Fatalf("pid/pgid 没存住: %+v", got)
	}
}

// D11：面板重启后残留的 running/starting 一律视为 stopped（进程已被 PDEATHSIG 收走），
// 但上次留下的退出信息要被保留 —— 重启面板后仍能看到"异常退出 code 137"。
func TestResetStatesOnBoot(t *testing.T) {
	db := openDB(t)
	running, _ := Create(db, ServiceInput{Name: "run", Kind: KindCommand, StartCmd: "x"})
	starting, _ := Create(db, ServiceInput{Name: "boot", Kind: KindCommand, StartCmd: "x"})
	withExit, _ := Create(db, ServiceInput{Name: "was", Kind: KindCommand, StartCmd: "x"})

	if err := SaveState(db, running.ID, State{State: StateRunning, PID: 999, PGID: 999, StartedAt: 111}); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(db, starting.ID, State{State: StateStarting, PID: 888, StartedAt: 222}); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(db, withExit.ID, State{
		State: StateStopped,
		Exit:  &ExitInfo{Code: 3, Reason: ReasonError, At: 555, StoppedBy: StoppedBySelf},
	}); err != nil {
		t.Fatal(err)
	}

	n, err := ResetStatesOnBoot(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应重置 2 条（running + starting），实际 %d", n)
	}

	for _, id := range []int64{running.ID, starting.ID} {
		g, err := GetState(db, id)
		if err != nil {
			t.Fatal(err)
		}
		if g.State != StateStopped || g.PID != 0 || g.PGID != 0 || g.StartedAt != 0 {
			t.Fatalf("service %d 未清干净: %+v", id, g)
		}
	}
	// 已有退出信息不能被重置流程抹掉。
	g, _ := GetState(db, withExit.ID)
	if g.Exit == nil || g.Exit.Code != 3 || g.Exit.Reason != ReasonError || g.Exit.At != 555 {
		t.Fatalf("已有退出信息被抹掉了: %+v", g.Exit)
	}
}

// systemd 服务没有本地进程，但同样要有状态行 —— API 层不必分两种读法。
func TestSystemdStateRowExists(t *testing.T) {
	db := openDB(t)
	svc, err := Create(db, ServiceInput{Name: "ssh", Kind: KindSystemd, Unit: "sshd.service"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetState(db, svc.ID); err != nil {
		t.Fatalf("systemd 服务也该有状态行: %v", err)
	}
}
