package filemgr

// 后台文件任务（设计 8.4 / M6-T4）—— 持久层与状态机。
//
// 存在的理由：复制/移动/删除的工作量随一个**用户可控且无上限**的量增长
// （字节数 / 条目数 / 目录树深度），而同步 HTTP 端点的 r.Context() 会随
// 关标签页、锁屏、代理超时取消，任务就地停住（实测中断时原地剩
// 2999/3000，而前端只看到一个网络错误）。设计第 18 行与验收项都要"不因
// 浏览器关闭而中断"，所以执行方必须是守护进程：先落库拿 id， **worker 池
// 异步跑，进度经 WS 推**。
//
// 本文件只管"任务在盘上的样子"：建、读、原子领取、进度、终态、重启对账。
// 执行与 worker 池在 queue.go。claim/progress/finish 一律不导出 —— 队列
// 若能被 HTTP 层调用，"谁决定任务在跑"就有了第二个主人。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// maxJobHistory 是列表里终态（done/failed/canceled/interrupted）任务的上限。
//
// 每次删除都留一行，跑一年就是几万条：抽屉一次拉全量会拖慢首屏，而用户
// 只看最近几条。进行中的任务不受此限（见 ListJobs）—— 一个排在几万条
// 之前的长任务若被历史挤掉，会从界面上消失，而它其实还在跑。
const maxJobHistory = 100

// JobOp 是任务类型。只有三种，比设计 598 行的枚举窄（理由见 0006 迁移）：
// upload 有自己的可续传协议，rename/mkdir/zip 工作量有界，做成任务只会给
// UI 多加一次"已提交 + 轮询"。
type JobOp string

const (
	OpCopy   JobOp = "copy"
	OpMove   JobOp = "move"
	OpDelete JobOp = "delete"
)

func (o JobOp) valid() bool {
	switch o {
	case OpCopy, OpMove, OpDelete:
		return true
	}
	return false
}

// JobState 是任务状态（设计 8.4 的状态机）。
type JobState string

const (
	JobPending     JobState = "pending"
	JobRunning     JobState = "running"
	JobDone        JobState = "done"
	JobFailed      JobState = "failed"
	JobCanceled    JobState = "canceled"
	JobInterrupted JobState = "interrupted"
)

// Valid 报告这是不是一个已知的任务状态。
//
// 导出是为了 HTTP 层能拒掉非法的 ?state= 而不是静默回全部（静默会让筛选
// 变成"看着能用而结果永远是全部"）。
//
// 这份枚举与 0006 迁移里 fs_jobs.state 的 CHECK 是同一套值，两处必须同步：
// 加了新状态而忘了改这里，GET ?state=新状态 会被自己的校验拒掉（抽屉里
// 永远看不到这类任务）；只改这里而忘了改 CHECK，则是能查一个库里根本存不
// 下来的值。TestJobStateSetMatchesSchema 把两边钉在一起。
func (s JobState) Valid() bool {
	switch s {
	case JobPending, JobRunning, JobDone, JobFailed, JobCanceled, JobInterrupted:
		return true
	}
	return false
}

// terminal 报告该状态是否已终态（不再有 worker 会动它）。
func (s JobState) terminal() bool {
	switch s {
	case JobDone, JobFailed, JobCanceled, JobInterrupted:
		return true
	}
	return false
}

// 任务层的哨兵错误。HTTP 层用 errors.Is 判，绝不对错误文本做中文子串匹配
// （改文案会把 404 变成 500）。
var (
	// ErrNoJob 指定 id 不存在（404）。
	ErrNoJob = errors.New("任务不存在")
	// ErrJobNotCancellable 任务已进终态，取消无从谈起（409）。
	// 不能静默成功：那等于谎报"已中止"，而任务其实早跑完了。
	ErrJobNotCancellable = errors.New("任务已结束，无法取消")
	// ErrJobInput 建任务的入参不合法（400）。
	ErrJobInput = errors.New("任务参数不合法")
	// ErrNoDB 面板没接数据库，队列不可用。
	ErrNoDB = errors.New("任务队列未接数据库")
)

// Job 是一个后台任务。
type Job struct {
	ID           int64    `json:"id"`
	Op           JobOp    `json:"op"`
	Src          []string `json:"src"`
	Dst          string   `json:"dst"`
	TotalBytes   int64    `json:"total_bytes"`
	DoneBytes    int64    `json:"done_bytes"`
	EntriesTotal int      `json:"entries_total"`
	EntriesDone  int      `json:"entries_done"`
	State        JobState `json:"state"`
	// CancelRequested 是"用户点了取消但 worker 还没落地"。它必须落盘：
	// 否则面板在任务排队时崩掉，重启后队列会把用户已明确取消的删除真做掉。
	CancelRequested bool `json:"cancel_requested"`
	// Permanent 只对 delete 有意义：true = 直接永久删除、不进回收站。
	// 与 cancel_requested 同类——都是"用户已做出的、不可从盘上重新推断的
	// 决定"，必须落盘（理由见 0007 迁移：排队中崩了，重启不能把永久
	// 删除偷换成可还原，也不能反过来吃掉唯一的后悔药）。
	Permanent bool `json:"permanent"`
	// Resumed 为真表示这条任务是从 interrupted 重试来的（见 0008 迁移）。
	// 只有 move 的执行体会读它：跨盘移动可能中断在"副本已校验、源未删",
	// 那时源与目标各有一份内容相同的文件，重跑要认出这个状态并只补删源,
	// 而全新一次移动撞同名目标必须照旧 409；两者在盘上无法区分，
	// "是不是重试"正是唯一的分界。
	Resumed   bool   `json:"resumed"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// JobInput 是建任务入参。
type JobInput struct {
	Op  JobOp
	Src []string
	Dst string
	// Permanent 只对 delete 生效（见 Job.Permanent）；其他 op 会被
	// normalized 强制归零，避免"copy 带了个 permanent=1"这种无意义组合。
	Permanent bool
}

// normalized 去重源路径、填默认、校验。
//
// 去重必须在**入库前**：前端"全选 + 手点两下"很容易交出重复项，而重复源
// 会让同一个文件被处理两次 —— copy 得到一次"目标已存在"的假失败，delete
// 第二次必然 404，用户明明只点了一次删除却看到一条红色报错。
func (in JobInput) normalized() (JobInput, error) {
	out := in
	if !out.Op.valid() {
		return in, fmt.Errorf("%w: 未知操作 %q", ErrJobInput, out.Op)
	}
	seen := make(map[string]bool, len(out.Src))
	src := make([]string, 0, len(out.Src))
	for _, p := range out.Src {
		if p == "" {
			return in, fmt.Errorf("%w: 源路径里有空值", ErrJobInput)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		src = append(src, p)
	}
	if len(src) == 0 {
		return in, fmt.Errorf("%w: 没有源路径", ErrJobInput)
	}
	out.Src = src
	// delete 没有目标；copy/move 必须有，否则 worker 取到只能立刻失败，
	// 用户在抽屉里看到的是"一条平白无故失败的记录"，比一个 400 难懂。
	if out.Op == OpDelete {
		out.Dst = ""
	} else if strings.TrimSpace(out.Dst) == "" {
		return in, fmt.Errorf("%w: %s 需要目标路径", ErrJobInput, out.Op)
	}
	// permanent 只对 delete 有意义：其他 op 带个 permanent=1 是个无意义组合，
	// 归零而不是报错（它不危险，只是垃圾数据）。
	if out.Op != OpDelete {
		out.Permanent = false
	}
	return out, nil
}

// JobFilter 是列举过滤。零值 = 全部。
type JobFilter struct {
	State JobState
}

// CreateJob 校验入参并落库，回一条 pending 任务（设计 115 行：立即落库
// + 返回 job_id）。
func (s *Service) CreateJob(ctx context.Context, in JobInput) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	db, err := s.jobDB()
	if err != nil {
		return Job{}, err
	}
	in, err = in.normalized()
	if err != nil {
		return Job{}, err
	}
	src, err := json.Marshal(in.Src)
	if err != nil {
		return Job{}, err
	}
	now := s.clock().Unix()
	var dst any
	if in.Dst != "" {
		dst = in.Dst
	}
	// 列是 INTEGER NOT NULL DEFAULT 0，所以显式转成 0/1 而不是传 bool 指望
	// 驱动顺手转：驱动行为是外部实现细节，而这一列的真假决定"要不要给
	// 用户留后悔药"。
	perm := 0
	if in.Permanent {
		perm = 1
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO fs_jobs(op,src,dst,entries_total,permanent,state,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		string(in.Op), string(src), dst, len(in.Src), perm,
		string(JobPending), now, now)
	if err != nil {
		return Job{}, fmt.Errorf("写入任务: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Job{}, fmt.Errorf("读任务 id: %w", err)
	}
	// 唤醒 worker 立刻来取（不等兜底 tick）。少了这一脚，刚提交的
	// 任务会干等一个轮询周期，界面上就是"按下复制、排队中转圈转半天"。
	s.notifyJob(ctx, id)
	s.kick()
	return s.GetJob(ctx, id)
}

// GetJob 按 id 读一条。
func (s *Service) GetJob(ctx context.Context, id int64) (Job, error) {
	db, err := s.jobDB()
	if err != nil {
		return Job{}, err
	}
	return scanJob(db.QueryRowContext(ctx, jobSelect+` WHERE id=?`, id))
}

// jobSelect 是共用的读列。列名与顺序必须和 scanJob 一一对上，所以只写
// 一份：两处各列一遍，加列时漏一处会得到错位的数据而不是报错。
const jobSelect = `SELECT id,op,src,dst,total_bytes,done_bytes,entries_total,entries_done,
	state,cancel_requested,permanent,resumed,COALESCE(error,''),created_at,updated_at FROM fs_jobs`

// ListJobs 列举：进行中的**全部** + 最近 maxJobHistory 条终态。
//
// 顺序按 id 倒序（刚提交的在顶上）。执行顺序是相反的 FIFO —— 两者必须
// 各自是自己的顺序，"顺手复用同一条 SQL"会让其中一个悄悄变形。
//
// 不能简单 `ORDER BY id DESC LIMIT n`：那样一个跑了很久、排在 n 条之前的
// 任务会从界面上消失，而它还在跑 —— 用户以为它没了，这是最难排查的一类
// "任务不见了"。
func (s *Service) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	db, err := s.jobDB()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q := jobSelect + `
		WHERE (? = '' OR state = ?)
		  AND (state IN ('pending','running') OR id IN (
		        SELECT id FROM fs_jobs WHERE state NOT IN ('pending','running')
		        ORDER BY id DESC LIMIT ?))
		ORDER BY id DESC`
	rows, err := db.QueryContext(ctx, q, string(f.State), string(f.State), maxJobHistory)
	if err != nil {
		return nil, fmt.Errorf("读任务列表: %w", err)
	}
	defer rows.Close()
	out := make([]Job, 0, 8)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ReconcileJobs 重启对账，返回改动的条数。
//
// running → interrupted：进程死了就是死了，那个 copy 到底复制了几个字节
// 只有上辈子的内存知道。留成 running 会让抽屉永远显示"进行中"，用户等不
// 到结果；直接重跑又可能留下半份目标 —— 所以停在这里等用户点重试。
//
// pending 且带取消意图 → canceled：这是"取消意图必须落盘"的另一半，光存
// 不用等于没存 —— 否则重启后这个用户已经说"别做"的 delete 会被真做掉。
//
// done 直接删行（用户裁定：已完成的任务不值得持久化）。终态里只有它对
// 重启后的世界没有任何下一步 —— interrupted 能重试、failed 带错误原文、
// canceled 是用户自己按的，done 只剩账目；而用户判断"跑完了吗"的手段本来
// 就是看目标目录，不是看抽屉里那行灰字。
//
// 清理只发生在**启动对账**，不在 finishJob 里：同一次运行内 done 行照常
// 留着（抽屉的"已完成"行、文件列表刷新、以及"30% 关浏览器 5 分钟回来看
// 结果"这条验收都靠它）。在 finalize 里顺手 DELETE 会连抽屉的完成通知一起
// 删掉 —— notifyJob 靠重读那一行拼推送，删了就推不出去。
//
// 普通 pending 保持不动（会继续跑）。其余终态一律不碰。
func (s *Service) ReconcileJobs(ctx context.Context) (int, error) {
	db, err := s.jobDB()
	if err != nil {
		return 0, err
	}
	now := s.clock().Unix()
	// 两条语句都要把改到的 id 带回来，理由不是性能而是**正确性**：
	// 对账是批量 UPDATE，改完之后手里只有"改了几行"这个计数，没有 id。
	// 直接照原样写下去的后果是任务状态在库里变成了 interrupted / canceled,
	// 而前端一条通知都收不到 —— 用户重启后打开任务抽屉，看到的还是上次
	// 崩溃前的"进行中"，重试按钮也不出现。这恰好是重启对账存在的目的
	// （告诉用户哪些任务需要他决定），漏推等于整个功能失效。
	// 改动行数上界是"上次在跑的并发数 + 排队中被取消的条数"，几十量级，
	// 逐条回读推送的代价可以忽略。
	var n int
	ids, err := reconcileExec(ctx, db, now)
	if err != nil {
		return 0, err
	}
	n += len(ids)
	s.notifyJobs(ctx, ids)
	// 删掉的 done 不进 ids：行都没了，notifyJob 的重读注定失败，塞进去
	// 只会白跑一轮 —— 前端的抽屉本来也没订过这些历史行。
	deleted, err := reconcileDropDone(ctx, db)
	if err != nil {
		return n, err
	}
	return n + deleted, nil
}

// doneDeleter 只取清历史用到的那一个方法（与 sqlExecutor 同一理由：
// 让"对账到底动了库的哪两处"在读代码时就能数清）。
type doneDeleter interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// reconcileDropDone 清掉上一次运行留下的已完成任务，返回删掉的条数。
func reconcileDropDone(ctx context.Context, db doneDeleter) (int, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM fs_jobs WHERE state='done'`)
	if err != nil {
		return 0, fmt.Errorf("清已完成任务: %w", err)
	}
	c, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("清已完成任务: 统计改动行数: %w", err)
	}
	return int(c), nil
}

// reconcileExec 跑对账的两条批量改写，返回被改到的所有 id。
func reconcileExec(ctx context.Context, db sqlExecutor, now int64) ([]int64, error) {
	var ids []int64
	for _, q := range []string{
		`UPDATE fs_jobs SET state='interrupted', updated_at=? WHERE state='running' RETURNING id`,
		`UPDATE fs_jobs SET state='canceled', updated_at=?
		 WHERE state='pending' AND cancel_requested=1 RETURNING id`,
	} {
		rows, err := db.QueryContext(ctx, q, now)
		if err != nil {
			return ids, fmt.Errorf("对账任务: %w", err)
		}
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return ids, fmt.Errorf("对账任务: %w", err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return ids, fmt.Errorf("对账任务: %w", err)
		}
		rows.Close()
		ids = append(ids, got...)
	}
	return ids, nil
}

// sqlExecutor 只取对账用到的那一个方法，测试可以塞假实现验证"改到了才推"。
type sqlExecutor interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// RequestCancelJob 记录取消意图；正在跑的任务另由 worker 的 cancelFunc
// 打断（queue.go 负责持有那个函数）。
//
// 意图落库而不是只调用 cancelFunc：任务此刻可能还在排队（没有 cancelFunc
// 可调），也可能在面板崩掉之后才轮到执行。
func (s *Service) RequestCancelJob(ctx context.Context, id int64) error {
	db, err := s.jobDB()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.clock().Unix()
	res, err := db.ExecContext(ctx,
		`UPDATE fs_jobs SET cancel_requested=1, updated_at=?
		 WHERE id=? AND state IN ('pending','running')`, now, id)
	// 只在真的改到了才推：改不到说明任务已落终态（用户点了个已完成任务的
	// 取消按钮），推一条 cancel_requested=true 会让抽屉显示一个永远不会
	// 发生的取消。
	defer func() {
		if err == nil {
			if c, _ := res.RowsAffected(); c > 0 {
				s.notifyJob(ctx, id)
			}
		}
	}()
	if err != nil {
		return fmt.Errorf("记录取消: %w", err)
	}
	if c, _ := res.RowsAffected(); c == 1 {
		// 意图落了库，还要打断正在跑的那一个（若在跑）。只记 flag 等
		// worker 自己发现也行，但一个 10GB 的复制要几十分钟，而用户点
		// 取消是在他意识到"选错了"的那一刻。排队中的没有 goroutine 可
		// 打断，那种情况由 claimJob 的 CASE 兜住。
		s.cancelRunning(id)
		return nil
	}
	// 没改动任何行：要么没这个 id，要么已终态。两者都要能区分出来 ——
	// 对已完成的谎报"已取消"是最坏的一种友好。
	j, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if j.CancelRequested {
		return nil // 重复取消：幂等，不报错
	}
	return fmt.Errorf("%w: %s", ErrJobNotCancellable, j.State)
}

// claimJob 原子地领一个待办任务（FIFO）。没有待办回 sql.ErrNoRows。
//
// 用一条 `UPDATE ... WHERE id = (SELECT ... LIMIT 1) RETURNING` 而不是
// "先 SELECT 再 UPDATE"：后者是两个语句，并发数 2 时两个 worker 会领到
// 同一个任务，于是同一批文件被处理两遍（copy 得到一次假的"目标已存在"，
// delete 第二次必然 404）。单条语句由 SQLite 自己串行化，无需事务。
//
// 顺带在同一句里把"带取消意图"的任务直接落成 canceled：排队期间用户点了
// 取消，轮到它时就不该被执行，而这两件事必须在同一个原子动作里 —— 分两步
// 的话中间崩一次，重启后就要靠对账来猜。
func (s *Service) claimJob(ctx context.Context) (Job, error) {
	db, err := s.jobDB()
	if err != nil {
		return Job{}, err
	}
	now := s.clock().Unix()
	var j Job
	var src, dst, state sql.NullString
	var eerr sql.NullString
	err = db.QueryRowContext(ctx,
		`UPDATE fs_jobs
		 SET state = CASE WHEN cancel_requested=1 THEN 'canceled' ELSE 'running' END,
		     updated_at = ?
		 WHERE id = (SELECT id FROM fs_jobs WHERE state='pending' ORDER BY id LIMIT 1)
		 RETURNING `+jobReturningCols, now).
		Scan(&j.ID, &j.Op, &src, &dst, &j.TotalBytes, &j.DoneBytes,
			&j.EntriesTotal, &j.EntriesDone, &state, &j.CancelRequested, &j.Permanent, &j.Resumed,
			&eerr, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, sql.ErrNoRows
	}
	if err != nil {
		return Job{}, fmt.Errorf("领取任务: %w", err)
	}
	j.State = JobState(state.String)
	j.Src, err = decodeSrc(src.String)
	if err != nil {
		return Job{}, err
	}
	j.Dst = dst.String
	j.Error = eerr.String
	// 领到即推：pending→running（带取消意图时是 pending→canceled）是队列
	// 自己发起的状态变化，没有任何调用方会替它广播。
	s.notifyJob(ctx, j.ID)
	return j, nil
}

// jobReturningCols 是 jobSelect 的列清单在 RETURNING 里的形态。
// 与 jobSelect 同一批列、同一顺序（共用 scan 顺序）。
const jobReturningCols = `id,op,src,dst,total_bytes,done_bytes,entries_total,entries_done,
	state,cancel_requested,permanent,resumed,COALESCE(error,''),created_at,updated_at`

// setJobProgress 写进度。节流策略在执行侧（设计：每 200ms 或每 4MB），
// 这里只负责"写了就能读到"。
func (s *Service) setJobProgress(ctx context.Context, id int64, doneBytes int64, entriesDone int) error {
	db, err := s.jobDB()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`UPDATE fs_jobs SET done_bytes=?, entries_done=?, updated_at=? WHERE id=?`,
		doneBytes, entriesDone, s.clock().Unix(), id)
	if err != nil {
		return fmt.Errorf("写任务进度: %w", err)
	}
	s.notifyJob(ctx, id)
	return nil
}

// finishJob 写终态。
//
// 带取消意图的任务即使 worker 正常返回也记 canceled：worker 在块边界看到
// 取消后清理半成品、然后返回 nil，如果这里照单写 done，用户在抽屉里看到
// "已完成"而实际文件只删了一半 —— 这种"看起来成功了"的假失败最危险。
func (s *Service) finishJob(ctx context.Context, id int64, st JobState, reason string) error {
	db, err := s.jobDB()
	if err != nil {
		return err
	}
	if !st.terminal() {
		return fmt.Errorf("finishJob 只能写终态, got %q", st)
	}
	// 用 defer 而不是在每个 return 前加一句：这个函数有三个出口，其中
	// done→canceled 那条是中途改写。只贴着最后一个 return 加通知，
	// 改写分支就会静默漏推 —— 界面停在"进行中"，而任务其实早就落了
	// canceled。终态是抽屉最要紧的一站，漏不得。
	defer func() {
		if err == nil {
			s.notifyJob(ctx, id)
		}
	}()
	now := s.clock().Unix()
	var res sql.Result
	if st == JobDone {
		res, err = db.ExecContext(ctx,
			`UPDATE fs_jobs SET state=?, updated_at=? WHERE id=? AND cancel_requested=0`,
			string(JobDone), now, id)
		if err != nil {
			return fmt.Errorf("写任务终态: %w", err)
		}
		if c, _ := res.RowsAffected(); c == 0 {
			// 要么任务被取消了（那就转 canceled），要么没了（那就没什么可写）
			_, err = db.ExecContext(ctx,
				`UPDATE fs_jobs SET state=?, updated_at=? WHERE id=? AND cancel_requested=1`,
				string(JobCanceled), now, id)
			return err
		}
		return nil
	}
	var e any
	if reason != "" {
		e = reason
	}
	_, err = db.ExecContext(ctx,
		`UPDATE fs_jobs SET state=?, error=?, updated_at=? WHERE id=?`,
		string(st), e, now, id)
	if err != nil {
		return fmt.Errorf("写任务终态: %w", err)
	}
	return nil
}

// jobDB 取底层句柄。每个方法开头判一次而不是在建 Service 时 panic：
// 文件管理的其余能力（浏览/上传/回收站）不需要数据库，队列没接不该
// 让整包不可用。
func (s *Service) jobDB() (*sql.DB, error) {
	if s.db == nil {
		return nil, ErrNoDB
	}
	return s.db.SqlDB(), nil
}

// rowScanner 让 scanJob 同时吃 QueryRow 与 Rows。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(r rowScanner) (Job, error) {
	var (
		j     Job
		src   sql.NullString
		dst   sql.NullString
		state sql.NullString
		eerr  sql.NullString
	)
	if err := r.Scan(&j.ID, &j.Op, &src, &dst, &j.TotalBytes, &j.DoneBytes,
		&j.EntriesTotal, &j.EntriesDone, &state, &j.CancelRequested, &j.Permanent, &j.Resumed,
		&eerr, &j.CreatedAt, &j.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, fmt.Errorf("%w", ErrNoJob)
		}
		return Job{}, fmt.Errorf("读任务: %w", err)
	}
	j.State = JobState(state.String)
	j.Dst = dst.String
	j.Error = eerr.String
	var err error
	if j.Src, err = decodeSrc(src.String); err != nil {
		return Job{}, err
	}
	return j, nil
}

// decodeSrc 解源路径数组。坏 JSON 不当"没有源"处理：那会让一个数据损坏的
// 任务被当成空任务展示（用户看到一条"0 个文件"的记录），而实际原因永远
// 没人知道。
func decodeSrc(raw string) ([]string, error) {
	if raw == "" {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("任务源路径已损坏: %w", err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
