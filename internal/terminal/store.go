package terminal

// 终端会话的元数据（term_sessions 表）。
//
// 边界要说清楚：这里**不存终端内容**。scrollback 一律由 tmux 持有（D5），
// 面板只在库里记住"还原一个用户看得懂的列表"所需的东西：标题、初始
// 目录/shell、历史上限、是否还活着、最近何时被打开。
//
// id 与 tmux 会话名是同一个数字的两种写法（lp-<id>），所以"面板记的"
// 与"tmux 里真实存在的"可以互相核对，不会出现两边各说各话。

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"litepanel/internal/store"
)

// DefaultHistoryLimit 是 tmux 的 history-limit 默认值（设计 7.2 的中间档）。
const DefaultHistoryLimit = 20000

// historyLimitOptions 是允许的档位。做成白名单而不是自由输入：100000 行
// × 多会话对 12GB 的机器是实打实的内存压力，不能让输入框随手填个 1e9。
var historyLimitOptions = []int{5000, DefaultHistoryLimit, 100000}

var (
	// ErrSessionNotFound 指定 id 不存在。
	ErrSessionNotFound = errors.New("终端会话不存在")
	// ErrBadHistoryLimit 历史上限不在白名单里。
	ErrBadHistoryLimit = fmt.Errorf("history_limit 只能是 %v 之一", historyLimitOptions)
	// ErrTitleRequired 标题必填。导出是为了 HTTP 层用 errors.Is 判断，
	// 而不是对 err.Error() 做中文子串匹配（改文案就会把 400 变成 500）。
	ErrTitleRequired = errors.New("会话标题不能为空")
)

// TmuxPrefix 是面板创建的 tmux 会话名前缀（对账靠它认人）。
const TmuxPrefix = "lp-"

// TmuxName 由会话 id 得到 tmux 会话名。
func TmuxName(id int64) string { return TmuxPrefix + strconv.FormatInt(id, 10) }

// SessionIDFromTmuxName 反解 tmux 会话名；不是本面板的会话则 ok=false。
func SessionIDFromTmuxName(name string) (int64, bool) {
	if !strings.HasPrefix(name, TmuxPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(name, TmuxPrefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// SessionMeta 是 term_sessions 表一行。
type SessionMeta struct {
	ID             int64  `json:"id"`
	TmuxName       string `json:"tmux_name"`
	Title          string `json:"title"`
	Cwd            string `json:"cwd"`
	Shell          string `json:"shell"`
	HistoryLimit   int    `json:"history_limit"`
	CreatedAt      int64  `json:"created_at"`
	LastAttachedAt int64  `json:"last_attached_at"`
	Alive          bool   `json:"alive"`
	// ExitStatus 死因：nil=未知/活着，-1=凭空消失（ExitVanished），
	// 0=正常退出（会被下一次 List 清掉），>0=异常退出码。见 0005 迁移。
	ExitStatus *int `json:"exit_status"`
}

// SessionInput 是创建入参。HistoryLimit=0 表示用默认档（不是"存 0"——
// 存 0 会让 tmux 的历史长度变成 0，重连就什么都看不到了）。
type SessionInput struct {
	Title        string
	Cwd          string
	Shell        string
	HistoryLimit int
}

func (in SessionInput) normalized() (SessionInput, error) {
	out := in
	out.Title = strings.TrimSpace(in.Title)
	if out.Title == "" {
		return in, ErrTitleRequired
	}
	if in.HistoryLimit == 0 {
		out.HistoryLimit = DefaultHistoryLimit
		return out, nil
	}
	for _, ok := range historyLimitOptions {
		if in.HistoryLimit == ok {
			return out, nil
		}
	}
	return in, ErrBadHistoryLimit
}

const sessionCols = `id, tmux_name, IFNULL(title,''), IFNULL(cwd,''), IFNULL(shell,''),
	history_limit, created_at, IFNULL(last_attached_at,0), alive, exit_status`

func scanSession(row interface{ Scan(...any) error }) (SessionMeta, error) {
	var (
		s     SessionMeta
		alive int
	)
	err := row.Scan(&s.ID, &s.TmuxName, &s.Title, &s.Cwd, &s.Shell,
		&s.HistoryLimit, &s.CreatedAt, &s.LastAttachedAt, &alive, &s.ExitStatus)
	s.Alive = alive != 0
	return s, err
}

// CreateSessionMeta 新建会话记录。tmux 会话由调用方另行创建（id 到手才拼得出名字）。
func CreateSessionMeta(db *store.DB, in SessionInput) (SessionMeta, error) {
	in, err := in.normalized()
	if err != nil {
		return SessionMeta{}, err
	}
	now := time.Now().Unix()
	// tmux_name 依赖自增 id（就是 lp-<id>），但列上挂着 NOT NULL + UNIQUE。
	// 空串占位会在第二条会话时撞唯一约束，所以先写一个随机占位，拿到 id 再回填。
	res, err := db.SqlDB().Exec(`INSERT INTO term_sessions
		(tmux_name, title, cwd, shell, history_limit, created_at, alive)
		VALUES('tmp-' || lower(hex(randomblob(8))), ?,?,?,?,?,1)`,
		in.Title, in.Cwd, in.Shell, in.HistoryLimit, now)
	if err != nil {
		return SessionMeta{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return SessionMeta{}, err
	}
	if _, err := db.SqlDB().Exec(`UPDATE term_sessions SET tmux_name=? WHERE id=?`, TmuxName(id), id); err != nil {
		return SessionMeta{}, err
	}
	return GetSessionMeta(db, id)
}

// GetSessionMeta 按 id 取。
func GetSessionMeta(db *store.DB, id int64) (SessionMeta, error) {
	row := db.SqlDB().QueryRow(`SELECT `+sessionCols+` FROM term_sessions WHERE id=?`, id)
	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionMeta{}, ErrSessionNotFound
	}
	return s, err
}

// ListSessionsMeta 按最近使用排序（侧栏顺序）。
func ListSessionsMeta(db *store.DB) ([]SessionMeta, error) {
	rows, err := db.SqlDB().Query(`SELECT ` + sessionCols + ` FROM term_sessions
		ORDER BY alive DESC, IFNULL(last_attached_at, created_at) DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionMeta
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RenameSessionMeta 改标题。
func RenameSessionMeta(db *store.DB, id int64, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return ErrTitleRequired
	}
	res, err := db.SqlDB().Exec(`UPDATE term_sessions SET title=? WHERE id=?`, title, id)
	if err != nil {
		return err
	}
	return affected(res, ErrSessionNotFound)
}

// DeleteSessionMeta 硬删记录。
func DeleteSessionMeta(db *store.DB, id int64) error {
	res, err := db.SqlDB().Exec(`DELETE FROM term_sessions WHERE id=?`, id)
	if err != nil {
		return err
	}
	return affected(res, ErrSessionNotFound)
}

// TouchSessionMeta 记录一次接入时间。
func TouchSessionMeta(db *store.DB, id, at int64) error {
	_, err := db.SqlDB().Exec(`UPDATE term_sessions SET last_attached_at=? WHERE id=?`, at, id)
	return err
}

func affected(res sql.Result, notFound error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound
	}
	return nil
}

// ReconcileResult 是一次对账的产出。
type ReconcileResult struct {
	Adopted []int64 // tmux 里有、库里没有 → 新建了记录
	Died    []int64 // 库里有、tmux 里没了 → 置 alive=0
}

// ReconcileMeta 以 tmux 为唯一真相来源核对库里的会话列表（D5）。
//
// list 由调用方提供（真实现是 `tmux ls`），这样本函数是纯数据层的，
// 单元测试不必真的起 tmux。
//
// 收编 tmux 里凭空多出来的 lp- 前缀会话：用户可能自己在 tmux 里建，
// 面板不该假装看不见；但标题只能给个占位，因为我们无从得知他本来的意图。
func ReconcileMeta(db *store.DB, list func() ([]string, error)) (ReconcileResult, error) {
	names, err := list()
	if err != nil {
		return ReconcileResult{}, err
	}
	live := make(map[int64]bool, len(names))
	for _, n := range names {
		if id, ok := SessionIDFromTmuxName(n); ok {
			live[id] = true
		}
	}

	existing, err := ListSessionsMeta(db)
	if err != nil {
		return ReconcileResult{}, err
	}
	known := make(map[int64]bool, len(existing))
	var res ReconcileResult
	for _, s := range existing {
		known[s.ID] = true
		if !live[s.ID] && s.Alive {
			if _, err := db.SqlDB().Exec(`UPDATE term_sessions SET alive=0 WHERE id=?`, s.ID); err != nil {
				return res, err
			}
			res.Died = append(res.Died, s.ID)
		}
	}

	for id := range live {
		if known[id] {
			continue
		}
		// id 必须显式指定为 tmux 名里的那串数字：本表的前提是
		// tmux_name == lp-<id>（API 由 id 反推 tmux 会话）。用自增 id 会让
		// 两者分叉，之后删除/对账就会指向另一个 tmux 会话。
		if _, err := db.SqlDB().Exec(`INSERT INTO term_sessions
			(id, tmux_name, title, history_limit, created_at, alive)
			VALUES(?,?,?,?,?,'1')`, id, TmuxName(id), "外部会话 "+TmuxName(id),
			DefaultHistoryLimit, time.Now().Unix()); err != nil {
			return res, err
		}
		res.Adopted = append(res.Adopted, id)
	}
	return res, nil
}
