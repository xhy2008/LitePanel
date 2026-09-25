package terminal

import (
	"testing"

	"litepanel/internal/store"
)

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// 创建后能读回全部字段；tmux_name 必须是 lp-<id>（对账靠这个前缀认人）。
func TestSessionStoreCreateAndList(t *testing.T) {
	db := openTestDB(t)

	got, err := CreateSessionMeta(db, SessionInput{Title: "编译内核", Cwd: "/root", Shell: "/bin/bash", HistoryLimit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if got.TmuxName != TmuxName(got.ID) {
		t.Fatalf("tmux 名应为 lp-<id>, got %q", got.TmuxName)
	}
	if got.Title != "编译内核" || got.Cwd != "/root" || got.HistoryLimit != 5000 {
		t.Fatalf("字段没存住: %+v", got)
	}
	if got.CreatedAt == 0 {
		t.Fatal("created_at 未填")
	}

	all, err := ListSessionsMeta(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != got.ID {
		t.Fatalf("列表不符: %+v", all)
	}
}

// history_limit 缺省取 20000（设计 7.2：三档 5000/20000/100000，默认中间档）。
// 传 0 表示"用默认"，不是"存 0"—— 存 0 会让 tmux 的历史长度变成 0。
func TestSessionStoreDefaultHistoryLimit(t *testing.T) {
	db := openTestDB(t)
	got, err := CreateSessionMeta(db, SessionInput{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.HistoryLimit != DefaultHistoryLimit {
		t.Fatalf("默认历史上限应为 %d, got %d", DefaultHistoryLimit, got.HistoryLimit)
	}
}

// 只允许白名单档位：这是设计里明确的三档，不是自由输入
// （100000 行 × 多会话对 12GB 机器是真实的内存压力，不能让人随手填 1e9）。
func TestSessionStoreRejectsBadHistoryLimit(t *testing.T) {
	db := openTestDB(t)
	for _, n := range []int{-1, 1, 20001, 99999, 1 << 30} {
		if _, err := CreateSessionMeta(db, SessionInput{Title: "x", HistoryLimit: n}); err == nil {
			t.Fatalf("history_limit=%d 应被拒绝", n)
		}
	}
	for _, n := range []int{5000, 20000, 100000} {
		if _, err := CreateSessionMeta(db, SessionInput{Title: "x", HistoryLimit: n}); err != nil {
			t.Fatalf("history_limit=%d 应被接受: %v", n, err)
		}
	}
}

// 标题必填：磁贴/标签页要显示它，空的会让用户无法区分会话。
func TestSessionStoreRequiresTitle(t *testing.T) {
	db := openTestDB(t)
	if _, err := CreateSessionMeta(db, SessionInput{Title: "   "}); err == nil {
		t.Fatal("空标题应被拒绝")
	}
}

func TestSessionStoreRename(t *testing.T) {
	db := openTestDB(t)
	s, err := CreateSessionMeta(db, SessionInput{Title: "旧名"})
	if err != nil {
		t.Fatal(err)
	}
	if err := RenameSessionMeta(db, s.ID, "新名"); err != nil {
		t.Fatal(err)
	}
	got, err := GetSessionMeta(db, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "新名" {
		t.Fatalf("重命名没生效: %q", got.Title)
	}
	if err := RenameSessionMeta(db, s.ID, "  "); err == nil {
		t.Fatal("空标题的重命名应被拒绝")
	}
	if err := RenameSessionMeta(db, 9999, "x"); err == nil {
		t.Fatal("不存在的 id 应报错")
	}
}

// 删除是硬删（用户在面板点删除 = 不再关心这个会话）。
// 不保留 alive=0 的空行：个人面板不需要"已删除会话"的历史账本。
func TestSessionStoreDelete(t *testing.T) {
	db := openTestDB(t)
	s, _ := CreateSessionMeta(db, SessionInput{Title: "a"})
	if err := DeleteSessionMeta(db, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := GetSessionMeta(db, s.ID); err == nil {
		t.Fatal("删除后应查不到")
	}
	if err := DeleteSessionMeta(db, 9999); err == nil {
		t.Fatal("删除不存在的 id 应报错")
	}
}

// 对账（D5）：tmux 里还剩哪些 lp-* 会话，是唯一的真相来源。
//   - tmux 里已消失 → 标记 alive=0 并仍返回，让 UI 说"会话已不存在"
//   - tmux 里有但库里没有（用户在 tmux 里手工建的 lp- 前缀会话）→ 补一行
func TestReconcileMarksDeadAndAdoptsUnknown(t *testing.T) {
	db := openTestDB(t)
	dead, _ := CreateSessionMeta(db, SessionInput{Title: "已退出"})
	_, err := CreateSessionMeta(db, SessionInput{Title: "活着"})
	if err != nil {
		t.Fatal(err)
	}
	live, _ := ListSessionsMeta(db)
	var liveID int64
	for _, s := range live {
		if s.Title == "活着" {
			liveID = s.ID
		}
	}

	// tmux 侧只剩 liveID 那个会话；再外加一个库里没有的 lp-9998
	res, err := ReconcileMeta(db, func() ([]string, error) {
		return []string{TmuxName(liveID), "lp-9998"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	all, _ := ListSessionsMeta(db)
	byID := map[int64]SessionMeta{}
	for _, s := range all {
		byID[s.ID] = s
	}
	if d, ok := byID[dead.ID]; !ok || d.Alive {
		t.Fatalf("tmux 里消失的会话应置 alive=0: %+v", d)
	}
	if l, ok := byID[liveID]; !ok || !l.Alive {
		t.Fatalf("仍在 tmux 里的会话应保持 alive: %+v", l)
	}
	if _, ok := byID[9998]; !ok {
		t.Fatalf("tmux 里存在但库里没有的会话应被收编: %+v", all)
	}
	adopted := 0
	for _, id := range res.Adopted {
		if id == 9998 {
			adopted++
		}
	}
	if adopted != 1 {
		t.Fatalf("应报告收编了 9998, got %v", res.Adopted)
	}
	if len(res.Died) != 1 || res.Died[0] != dead.ID {
		t.Fatalf("应报告 dead 会话死去, got %v", res.Died)
	}
}

// 对账第二次跑（tmux 状态不变）必须是幂等的：不重复收编、不重复标死。
func TestReconcileIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	s, _ := CreateSessionMeta(db, SessionInput{Title: "x"})
	names := func() ([]string, error) { return []string{TmuxName(s.ID)}, nil }

	if _, err := ReconcileMeta(db, names); err != nil {
		t.Fatal(err)
	}
	r2, err := ReconcileMeta(db, names)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Adopted) != 0 || len(r2.Died) != 0 {
		t.Fatalf("重复对账不应有变化: %+v", r2)
	}
	all, _ := ListSessionsMeta(db)
	if len(all) != 1 {
		t.Fatalf("重复对账不应多出会话行: %+v", all)
	}
}

// 触碰时间：侧栏要按"最近使用"排序
func TestTouchSessionMeta(t *testing.T) {
	db := openTestDB(t)
	s, _ := CreateSessionMeta(db, SessionInput{Title: "x"})
	if err := TouchSessionMeta(db, s.ID, 1234); err != nil {
		t.Fatal(err)
	}
	got, _ := GetSessionMeta(db, s.ID)
	if got.LastAttachedAt != 1234 {
		t.Fatalf("last_attached_at 未记录: %+v", got)
	}
}
