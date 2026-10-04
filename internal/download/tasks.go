package download

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"litepanel/internal/store"
)

// 下载任务的本地历史（设计 9.2 / M7-T2 的 tasks.go）。
//
// 这张表的存在理由是一条实测结论：**aria2 跨重启不保留任何终态记录**
// （dev/aria2hist：tellStopped 从 2 条变 0 条，`--save-session` 默认只存
// active/waiting/paused）。所以"保留已完成/已失败的历史"这件事面板不自己做
// 就没人做，它不是 aria2 的缓存。
//
// 分工必须划清，否则会出现两个真相来源：
//   - **进行中**的状态（速度、已下字节、连接数、active/waiting/paused）归
//     aria2 独占，由轮询器每 tick 取，只活在内存里（progress 字段）。
//   - **提交时的事实**（uri/name/dir/gid）与**终态的事实**（state/error/
//     finished_at/完成时的大小）落库。
//
// 往库里镜像进行中状态的诱惑很实在（"重启后还能看到进度"），代价同样实在：
// 每 tick 一次 SQLite 写（违背本里程碑"面板开销极低"的承诺），外加一个必然
// 与 aria2 漂移的副本 —— aria2 侧被别的客户端（Web UI、aria2rpc）改动时，
// 界面得挑一个信，挑错就显示假进度。

// State 是下载任务的状态。取值与迁移里的 CHECK 一致。
type State string

const (
	StateActive   State = "active"
	StateWaiting  State = "waiting"
	StatePaused   State = "paused"
	StateComplete State = "complete"
	StateError    State = "error"
	StateRemoved  State = "removed"
)

// MaxHistory 是终态记录（"历史"）的条数上限。
//
// 与 fs_jobs 同理：无上限的表在常年运行后只会拖慢列表查询，而用户根本翻不
// 到那么远。上限只管终态 —— 进行中的任务不受裁剪（见 trimHistory）。
const MaxHistory = 200

// IsTerminal 报告该状态是否已终结。终结意味着"aria2 那边消失之后也不再有
// 任何更新"，因此它同时是"可以被历史裁剪删掉"与"不可被后续事件改写"的判据。
func (s State) IsTerminal() bool {
	switch s {
	case StateComplete, StateError, StateRemoved:
		return true
	}
	return false
}

// ErrNoTask 表示 gid 不在本面板的提交记录里。
var ErrNoTask = errors.New("没有这条下载任务")

// Submission 是一次提交（POST /api/dl/tasks 的入参经校验后的形状）。
type Submission struct {
	URIs []string
	GID  string
	Dir  string
	Name string
}

// Record 是一条下载记录。
//
// 注意 uri 的存法：JSON 数组而不是逗号拼接。aria2 的 addUri 接受同一文件的
// 多个镜像地址，而 URL 的查询串里逗号是合法字符（?r=1,2）—— 拼接再拆会把
// 用户填的地址改掉。
type Record struct {
	ID             int64    `json:"id"`
	GID            string   `json:"gid"`
	URIs           []string `json:"uris"`
	Name           string   `json:"name,omitempty"`
	Dir            string   `json:"dir,omitempty"`
	State          State    `json:"state"`
	Error          string   `json:"error,omitempty"`
	TotalBytes     int64    `json:"total_bytes"`
	CompletedBytes int64    `json:"completed_bytes"`
	CreatedAt      int64    `json:"created_at"`
	FinishedAt     int64    `json:"finished_at,omitempty"`
}

// ListFilter 是历史/列表查询的过滤条件。
type ListFilter struct {
	// States 为空表示不限状态。
	States []State
	Limit  int
}

// TaskStore 是下载记录的持久层 + 内存进度表。可并发使用。
type TaskStore struct {
	db    *store.DB
	clock func() time.Time

	mu       sync.Mutex
	progress map[string]Progress
}

// NewTaskStore 建存储。clock 便于测试固定时间。
func NewTaskStore(db *store.DB, clock func() time.Time) *TaskStore {
	if clock == nil {
		clock = time.Now
	}
	return &TaskStore{db: db, clock: clock, progress: map[string]Progress{}}
}

func (s *TaskStore) sqlDB() (*sql.DB, error) {
	if s.db == nil {
		return nil, errors.New("下载模块没有接数据库")
	}
	return s.db.SqlDB(), nil
}

// Add 记录一次提交。**必须在 aria2 受理之后、HTTP 返回之前调用**：反过来
// （先落库再提交）会在 aria2 拒绝时留下一条永远不动的幽灵记录。
func (s *TaskStore) Add(ctx context.Context, in Submission) (*Record, error) {
	if len(in.URIs) == 0 {
		return nil, errors.New("下载地址不能为空")
	}
	if in.GID == "" {
		return nil, errors.New("aria2 没有返回 gid，无法跟踪这个任务")
	}
	db, err := s.sqlDB()
	if err != nil {
		return nil, err
	}
	uriJSON, err := json.Marshal(in.URIs)
	if err != nil {
		return nil, fmt.Errorf("编码下载地址: %w", err)
	}
	now := s.clock().Unix()
	res, err := db.ExecContext(ctx,
		`INSERT INTO downloads(gid,uri,name,dir,state,created_at)
		 VALUES(?,?,?,?,?,?)`,
		in.GID, string(uriJSON), in.Name, in.Dir, string(StateActive), now)
	if err != nil {
		return nil, fmt.Errorf("记录下载任务: %w", err)
	}
	id, _ := res.LastInsertId()
	rec := &Record{
		ID: id, GID: in.GID, URIs: in.URIs, Name: in.Name, Dir: in.Dir,
		State: StateActive, CreatedAt: now,
	}
	s.trimHistory(ctx)
	return rec, nil
}

// Get 按 gid 取一条记录。
func (s *TaskStore) Get(ctx context.Context, gid string) (*Record, error) {
	db, err := s.sqlDB()
	if err != nil {
		return nil, err
	}
	return scanOne(db.QueryRowContext(ctx, selectCols+` WHERE gid=?`, gid))
}

// 可空列统一 COALESCE 成零值再扫。
//
// 不用 sql.NullString 逐个判断：那样每个消费点都要写一遍 .String/.Valid，
// 漏一处就是把 NULL 当有值用；而直接把 NULL 扫进 string 会报错（实测
// "converting NULL to string is unsupported"），一行 name 为空的记录就能让
// 整张历史列表 500。迁移里 name/dir/error/finished_at 都是可空列。
const selectCols = `SELECT id,gid,uri,
	COALESCE(name,''), COALESCE(dir,''), state, COALESCE(error,''),
	total_len, completed_len, created_at, COALESCE(finished_at,0)
	FROM downloads`

func scanOne(row *sql.Row) (*Record, error) {
	var r Record
	var uri, name, dir, state, eSQL string
	err := row.Scan(&r.ID, &r.GID, &uri, &name, &dir, &state, &eSQL,
		&r.TotalBytes, &r.CompletedBytes, &r.CreatedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoTask
	}
	if err != nil {
		return nil, fmt.Errorf("读下载记录: %w", err)
	}
	r.URIs = unmarshalURIs(uri)
	r.Name, r.Dir, r.Error, r.State = name, dir, eSQL, State(state)
	return &r, nil
}

// unmarshalURIs 解析失败时回一个空数组而不是 error。
//
// 这不是偷懒：读路径上一旦返回 error，整张历史列表就打不开（一行脏数据让
// 整个页面 500）。列表"少显示一条地址"远好过"看不到历史"。真正该报错的是写
// 路径，那里 marshal 失败会照实返回。
func unmarshalURIs(raw string) []string {
	var out []string
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// List 按条件取记录，时间倒序（新在前）。
func (s *TaskStore) List(ctx context.Context, f ListFilter) ([]Record, error) {
	db, err := s.sqlDB()
	if err != nil {
		return nil, err
	}
	q := selectCols
	var args []any
	if len(f.States) > 0 {
		q += ` WHERE state IN (` + placeholders(len(f.States)) + `)`
		for _, st := range f.States {
			args = append(args, string(st))
		}
	}
	q += ` ORDER BY id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("列下载记录: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var uri, name, dir, state, eSQL string
		if err := rows.Scan(&r.ID, &r.GID, &uri, &name, &dir, &state, &eSQL,
			&r.TotalBytes, &r.CompletedBytes, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, fmt.Errorf("列下载记录: %w", err)
		}
		r.URIs, r.Name, r.Dir, r.Error, r.State = unmarshalURIs(uri), name, dir, eSQL, State(state)
		out = append(out, r)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += "?"
	}
	return out
}

// SetTerminal 写终态。
//
// SQL 里的 `AND state NOT IN (终态)` 是这条记录的**幂等与不倒退**保证，
// 而且必须写在 SQL 里：事件桥与轮询器会并发写同一条，读-改-写在这层挡不住
// （两者都可能先读到 active 再各自写）。倒退的实际后果很糟：一条 complete 被
// 迟到的 error 覆盖，用户会以为下载坏了，而文件其实好好的。
//
// 未知 gid（别人用 aria2 的 Web UI 加的任务、面板重装过）影响 0 行且不报错
// —— 返回错误会让调用方在事件流里中断，而"这条不是面板提交的"是正常情况。
func (s *TaskStore) SetTerminal(ctx context.Context, gid string, st State, reason string, total, done int64) error {
	db, err := s.sqlDB()
	if err != nil {
		return err
	}
	if !st.IsTerminal() {
		return fmt.Errorf("SetTerminal 收到非终态 %s", st)
	}
	_, err = db.ExecContext(ctx,
		`UPDATE downloads
		    SET state=?, error=?, total_len=?, completed_len=?, finished_at=?
		  WHERE gid=? AND state NOT IN ('complete','error','removed')`,
		string(st), reason, total, done, s.clock().Unix(), gid)
	if err != nil {
		return fmt.Errorf("写下载终态: %w", err)
	}
	s.forgetProgress(gid)
	s.trimHistory(ctx)
	return nil
}

// SetState 改进行中的状态（暂停/继续这类由面板自己发起的动作成功后）。
// 已终结的记录同样不被改写。
func (s *TaskStore) SetState(ctx context.Context, gid string, st State) error {
	db, err := s.sqlDB()
	if err != nil {
		return err
	}
	if st.IsTerminal() {
		return fmt.Errorf("SetState 不能写终态 %s（请用 SetTerminal，它带原因与大小）", st)
	}
	_, err = db.ExecContext(ctx,
		`UPDATE downloads SET state=? WHERE gid=? AND state NOT IN ('complete','error','removed')`,
		string(st), gid)
	if err != nil {
		return fmt.Errorf("改下载状态: %w", err)
	}
	return nil
}

// ApplyProgress 收下轮询器本轮的进度快照，**不写库**。
//
// 快照语义（整体替换而不是增量合并）是刻意的：轮询器给的正是"此刻还在下的
// 全部任务"，增量合并会让已经消失的任务永远留在内存里。
func (s *TaskStore) ApplyProgress(items []Progress) {
	next := make(map[string]Progress, len(items))
	for _, p := range items {
		next[p.GID] = p
	}
	s.mu.Lock()
	s.progress = next
	s.mu.Unlock()
}

// ProgressSnapshot 交出现存进度（下载页把内存进度贴到记录上的那次合并用）。
func (s *TaskStore) ProgressSnapshot() []Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Progress, 0, len(s.progress))
	for _, p := range s.progress {
		out = append(out, p)
	}
	return out
}

func (s *TaskStore) forgetProgress(gid string) {
	s.mu.Lock()
	delete(s.progress, gid)
	s.mu.Unlock()
}

// ClearHistory 清掉已终结的记录，返回删除条数。
//
// 只删终态。整表 DELETE 会把正在下载的任务也抹掉，而 aria2 那边还在下 ——
// 界面上再也找不到它（进度和暂停按钮都没了），用户只能等它下完，然后在文件
// 管理器里发现一个凭空出现的文件。
func (s *TaskStore) ClearHistory(ctx context.Context) (int, error) {
	db, err := s.sqlDB()
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM downloads WHERE state IN ('complete','error','removed')`)
	if err != nil {
		return 0, fmt.Errorf("清下载历史: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// trimHistory 把终态记录裁到上限。
//
// 只裁终态：进行中那条是用户此刻唯一关心的，一条都不能少。按 id 升序删就是
// 删最老的（id 单调递增）。
//
// 这里**吞掉错误**是有意的：裁剪是维护性动作，它失败不该让 Add/SetTerminal
// 整体失败 —— 用户的任务已经提交成功了，因为"历史没裁动"回他一个 500 是
// 把内部整洁排在用户操作之前。
func (s *TaskStore) trimHistory(ctx context.Context) {
	db, err := s.sqlDB()
	if err != nil {
		return
	}
	_, _ = db.ExecContext(ctx,
		`DELETE FROM downloads WHERE state IN ('complete','error','removed')
		   AND id NOT IN (
		     SELECT id FROM downloads
		      WHERE state IN ('complete','error','removed')
		      ORDER BY id DESC LIMIT ?
		   )`, MaxHistory)
}

// Reconcile 在面板启动时给每条"看起来还在进行"的记录一次机会：alive 说不在
// 的，收口成终态并说明原因。返回收口的条数。
//
// 不收口的话，历史里会永远挂着几条"下载中"，而它们不会再有任何更新 ——
// aria2 跨重启不保留任务（实测），面板却还以为在下。alive 由调用方提供而不是
// 这里去问：判据来自 aria2 的 tellActive/tellWaiting/tellPaused，而且"面板重启
// 但 aria2 没重启"是完全正常的场景，那种情况下活着的任务必须保留。
func (s *TaskStore) Reconcile(ctx context.Context, alive func(gid string) bool) (int, error) {
	db, err := s.sqlDB()
	if err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT gid FROM downloads WHERE state NOT IN ('complete','error','removed')`)
	if err != nil {
		return 0, fmt.Errorf("列未完结的下载: %w", err)
	}
	var gids []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return 0, fmt.Errorf("列未完结的下载: %w", err)
		}
		gids = append(gids, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("列未完结的下载: %w", err)
	}
	n := 0
	for _, g := range gids {
		if alive != nil && alive(g) {
			continue
		}
		// 原因必须是"面板重启过、aria2 里已经查不到"，而不是"下载失败"：
		// 后者会把用户支去重下，而文件可能好端端地在盘上。
		if err := s.SetTerminal(ctx, g, StateError,
			"面板重启后 aria2 已无此任务（可能早已完成或已被移除，请检查目标目录）", 0, 0); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
