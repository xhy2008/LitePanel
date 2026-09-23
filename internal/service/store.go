// Package service 服务器托管：数据层 + 子进程监管 + 状态判定。
//
// 三条硬约束贯穿本包：
//   - D11 面板被 kill -9 时子进程必须一并消失（PDEATHSIG），重启后状态清为 stopped；
//   - D19 服务日志只在内存环形缓冲里，绝不落盘、不进数据库；
//   - D21 退出呈现只有「运行中 / 正常退出 code 0 / 异常退出 code N」三种。
package service

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"litepanel/internal/store"
)

// Kind 是服务类型。只有两种：自己拉起的命令，或交给 systemd 的单元。
type Kind = string

const (
	KindCommand Kind = "command"
	KindSystemd Kind = "systemd"
)

// State 是运行状态机取值。
type StateName = string

const (
	StateStopped  StateName = "stopped"
	StateStarting StateName = "starting"
	StateRunning  StateName = "running"
	StateStopping StateName = "stopping"
)

// Reason 是退出归因。D21：只有这两种，不存在第三种。
type Reason = string

const (
	ReasonClean Reason = "clean"
	ReasonError Reason = "error"
)

// 谁把服务停掉的。归因影响 UI 文案，也用于排查"谁杀了我的服务"。
const (
	StoppedByUser          = "user"
	StoppedByPanelShutdown = "panel-shutdown"
	StoppedByPdeathsig     = "pdeathsig"
	StoppedBySelf          = "self"
)

// ErrDuplicateName 服务名唯一（原型上磁贴直接显示名字，重名会无法区分）。
var ErrDuplicateName = errors.New("服务名已存在")

// ErrNotFound 指定 id 不存在。
var ErrNotFound = errors.New("服务不存在")

// Service 是 services 表一行。
type Service struct {
	ID        int64
	Name      string
	Kind      Kind
	Unit      string
	StartCmd  string
	StopCmd   string
	Cwd       string
	Autostart bool
	Sort      int
	CreatedAt int64
}

// ServiceInput 是创建/更新的入参。校验集中在 valid()，API 层不必重复。
type ServiceInput struct {
	Name      string
	Kind      Kind
	Unit      string
	StartCmd  string
	StopCmd   string
	Cwd       string
	Autostart bool
	Sort      int
}

func (in ServiceInput) valid() error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("名称不能为空")
	}
	switch in.Kind {
	case KindCommand:
		if strings.TrimSpace(in.StartCmd) == "" {
			return errors.New("command 类型必须填写启动命令")
		}
	case KindSystemd:
		if strings.TrimSpace(in.Unit) == "" {
			return errors.New("systemd 类型必须填写单元名")
		}
	default:
		return fmt.Errorf("未知服务类型 %q（只支持 command / systemd）", in.Kind)
	}
	return nil
}

// ExitInfo 是一次退出的记录；nil 表示这个服务从没退出过。
// 用指针而不是 exit_code IS NULL 的散字段：否则合法的 code 0
// 会被读成"空"，UI 上就分不清「正常退出 code 0」和「从没跑过」。
//
// tag 是必须的：这个类型也会被 WS 帧直接序列化，缺 tag 就吐 PascalCase，
// 而前端坚持与 HTTP 用同一套 snake_case key。
type ExitInfo struct {
	Code      int    `json:"code"`
	Signal    int    `json:"signal"`
	Reason    Reason `json:"reason"`
	At        int64  `json:"at"`
	StoppedBy string `json:"stopped_by"`
}

// State 是 service_state 表一行。
type State struct {
	State     StateName
	PID       int
	PGID      int
	StartedAt int64
	Exit      *ExitInfo
}

const serviceCols = `id, name, kind, IFNULL(unit,''), IFNULL(start_cmd,''),
	IFNULL(stop_cmd,''), IFNULL(cwd,''), autostart, sort, created_at`

func scanService(row interface{ Scan(...any) error }) (Service, error) {
	var (
		s         Service
		autostart int
	)
	err := row.Scan(&s.ID, &s.Name, &s.Kind, &s.Unit, &s.StartCmd,
		&s.StopCmd, &s.Cwd, &autostart, &s.Sort, &s.CreatedAt)
	s.Autostart = autostart != 0
	return s, err
}

// Create 插入服务并建好配套状态行。
func Create(db *store.DB, in ServiceInput) (Service, error) {
	if err := in.valid(); err != nil {
		return Service{}, err
	}
	// 注：本库固定 SetMaxOpenConns(1)（modernc 驱动下每个连接要各自设 WAL）。
	// 因此事务进行期间绝不能再走 db.SqlDB() 发查询 —— 唯一的连接被事务占着，会死锁。
	tx, err := db.SqlDB().Begin()
	if err != nil {
		return Service{}, err
	}
	defer tx.Rollback()

	var sv Service
	var autostart int
	err = tx.QueryRow(`INSERT INTO services
		(name, kind, unit, start_cmd, stop_cmd, cwd, autostart, sort, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		RETURNING `+serviceCols,
		in.Name, in.Kind, nullStr(in.Unit), nullStr(in.StartCmd),
		nullStr(in.StopCmd), nullStr(in.Cwd), b2i(in.Autostart), in.Sort,
		time.Now().Unix()).
		Scan(&sv.ID, &sv.Name, &sv.Kind, &sv.Unit, &sv.StartCmd,
			&sv.StopCmd, &sv.Cwd, &autostart, &sv.Sort, &sv.CreatedAt)
	if err != nil {
		if isUnique(err) {
			return Service{}, ErrDuplicateName
		}
		return Service{}, fmt.Errorf("创建服务: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO service_state (service_id, state) VALUES (?, ?)`,
		sv.ID, StateStopped); err != nil {
		return Service{}, fmt.Errorf("建状态行: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Service{}, err
	}
	sv.Autostart = autostart != 0
	return sv, nil
}

// Get 按 id 取服务。
func Get(db *store.DB, id int64) (Service, error) {
	sv, err := scanService(db.SqlDB().QueryRow(
		`SELECT `+serviceCols+` FROM services WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Service{}, ErrNotFound
	}
	return sv, err
}

// List 按 sort 升序、同 sort 按名称返回服务清单。
func List(db *store.DB) ([]Service, error) {
	rows, err := db.SqlDB().Query(
		`SELECT ` + serviceCols + ` FROM services ORDER BY sort ASC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Service
	for rows.Next() {
		sv, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// Update 整条覆盖（原型对话框是一次性提交全部字段）。
func Update(db *store.DB, id int64, in ServiceInput) error {
	if err := in.valid(); err != nil {
		return err
	}
	res, err := db.SqlDB().Exec(`UPDATE services SET
		name=?, kind=?, unit=?, start_cmd=?, stop_cmd=?, cwd=?, autostart=?, sort=?
		WHERE id=?`,
		in.Name, in.Kind, nullStr(in.Unit), nullStr(in.StartCmd),
		nullStr(in.StopCmd), nullStr(in.Cwd), b2i(in.Autostart), in.Sort, id)
	if err != nil {
		if isUnique(err) {
			return ErrDuplicateName
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete 删除服务；状态行由外键级联清掉。
// 调用方负责先停进程 —— 数据层不该去碰进程。
func Delete(db *store.DB, id int64) error {
	res, err := db.SqlDB().Exec(`DELETE FROM services WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetState 读运行时状态。
func GetState(db *store.DB, id int64) (State, error) {
	var (
		st        State
		startedAt sql.NullInt64
		exitCode  sql.NullInt64
		reason    sql.NullString
		exitedAt  sql.NullInt64
		stoppedBy sql.NullString
		sig       int
	)
	err := db.SqlDB().QueryRow(`SELECT
		state, pid, pgid, started_at,
		exit_code, exit_signal, exit_reason, exited_at, stopped_by
		FROM service_state WHERE service_id=?`, id).
		Scan(&st.State, &st.PID, &st.PGID, &startedAt,
			&exitCode, &sig, &reason, &exitedAt, &stoppedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, ErrNotFound
	}
	if err != nil {
		return State{}, err
	}
	st.StartedAt = startedAt.Int64
	// exit_reason 非空 = 有过退出记录。这样 exit_code=0 才是可信的"正常退出"。
	if reason.Valid {
		st.Exit = &ExitInfo{
			Code: int(exitCode.Int64), Signal: sig, Reason: Reason(reason.String),
			At: exitedAt.Int64, StoppedBy: stoppedBy.String,
		}
	}
	return st, nil
}

// SaveState 整行覆盖状态。
func SaveState(db *store.DB, id int64, st State) error {
	var (
		startedAt any
		code      any
		reason    any
		exited    any
		stoppedBy any
		sig       int
	)
	if st.StartedAt != 0 {
		startedAt = st.StartedAt
	}
	if e := st.Exit; e != nil {
		code, reason, exited, sig = e.Code, e.Reason, e.At, e.Signal
		if e.StoppedBy != "" {
			stoppedBy = e.StoppedBy
		}
	}
	res, err := db.SqlDB().Exec(`UPDATE service_state SET
		pid=?, pgid=?, started_at=?, state=?,
		exit_code=?, exit_signal=?, exit_reason=?, exited_at=?, stopped_by=?
		WHERE service_id=?`,
		st.PID, st.PGID, startedAt, st.State,
		code, sig, reason, exited, stoppedBy, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ResetStatesOnBoot 把残留的 running/starting 清成 stopped，返回清理条数。
// D11 的收尾：面板能走到这里说明上次不是被 kill -9（那种情况下进程组已被
// 内核收走），但仍可能有 systemd 类型或极窄竞态留下的脏状态。
// 退出信息一律保留 —— 用户重启面板后还要能看到上次是怎么死的。
func ResetStatesOnBoot(db *store.DB) (int, error) {
	res, err := db.SqlDB().Exec(`UPDATE service_state SET
		state=?, pid=0, pgid=0, started_at=NULL
		WHERE state IN (?, ?)`,
		StateStopped, StateRunning, StateStarting)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUnique 识别唯一键冲突。驱动不导出错误类型（modernc 只给文本），
// 所以只能文判；调用点传入的必须是未经包装的原始驱动错误。
func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
