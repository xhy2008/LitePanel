package filemgr

// worker 池（设计 8.4 / M6-T4）。
//
// 只负责"把库里的 pending 变成终态"：领取、并发上限、取消传递、关停对账、
// 进度节流。真正的复制/移动/删除在 exec.go 的 runJob 里。
//
// 为什么必须异步而不是在 HTTP handler 里同步做完：见 jobs.go 顶部。一句话
// —— r.Context() 会随关标签页取消，同步做等于"用户锁屏就中断"。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"litepanel/internal/logx"
)

// DefaultJobConcurrency 是后台任务的默认并发上限（设计 8.4："worker pool
// 默认并发 2（设置页可调）"）。
//
// 并发数在这里不是性能调优：这台机器上还跑着别的服务，机械盘上几个大文件
// 同时读写会互相抢头；更要紧的是没有上限的话，一次"全选 500 个目录做
// 复制"会一口气起 500 个 goroutine 各开一对文件句柄。
const DefaultJobConcurrency = 2

const (
	// defaultJobWakeInterval 是没有唤醒信号时的兜底轮询周期。
	//
	// 正常路径是提交任务时投一个 wake；这个 tick 只兜"wake 丢了"的情况
	// （比如另一个进程往库里塞了任务，或上一次投递正好撞上通道满）。
	// 所以它慢一点没关系，但绝不能是唯一的通路 —— 那会让刚提交的任务
	// 干等一整个周期，界面上就是"按下复制、排队中转圈转半天"。
	defaultJobWakeInterval = 30 * time.Second

	// progressInterval / progressBytes 是进度落库的两个阈值（设计 8.4：
	// "每 200ms 或每 4MB 更新一次进度"）。任一越过就落库。
	progressInterval = 200 * time.Millisecond
	progressBytes    = int64(4 << 20)
)

// progressFunc 是执行器上报进度的回调：已处理字节数 + 已完成条目数。
//
// 返回值不是装饰：取消是经由这个返回值传回执行器的（节流层观察到取消或
// 库写失败时回错误，执行器据此收工）。取消只放在 ctx 里的话，一个只在
// report 之间检查 ctx 的实现会漏掉检查点。
//
// 用别名而不是定义类型：它纯粹是个回调形状（没有方法、不参与任何方法集），
// 而定型类型会让 Options.JobExecutor 与外部写的执行器签名不兼容 —— 注入
// 点用起来别扭，而这里没有任何东西需要那个新类型名。
type progressFunc = func(doneBytes int64, entriesDone int) error

// jobExecutor 跑一个任务的实际工作。可注入（见 Options.JobExecutor）。
type jobExecutor func(ctx context.Context, j Job, report progressFunc) error

// StartJobs 起 worker 池，返回是否真的接上了。
//
// 没接数据库时必须报告"没在跑"：与两个 janitor 同一条理由 —— 漏接的当下
// 没有任何症状，而日志里若写着"队列已启动"，运维的排查方向从一开始就错了。
// 重复调用不起第二套 worker：两套会把并发上限翻倍，而配置文件里的"并发 2"
// 是用户唯一能看到的数字。
func (s *Service) StartJobs(ctx context.Context) bool {
	if s.db == nil || s.executor == nil {
		return false
	}
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if s.poolStarted {
		return false
	}
	s.poolStarted = true
	s.jobsCtx = ctx
	// 先把上次遗留的任务对账掉，**再**起 worker。
	//
	// 顺序不能反过来。对账靠"running 一定是上个进程留下的死任务"这个
	// 前提把 running 转成 interrupted；如果先起 worker，worker 可能立刻
	// 领一条任务转成 running，紧接着本轮对账就把这条**正在真跑**的任务
	// 也盖成 interrupted——库里显示中断、实际还在删文件。实测 -count=8
	// 随机复现（同一轮启动内对账与 worker 抢跑）。先对账后起工，本进程
	// 产生任何 running 之前遗留已全部落地，前提就成立了。
	if n, err := s.ReconcileJobs(ctx); err != nil {
		logx.Error("任务对账失败: %v", err)
	} else if n > 0 {
		logx.Info("任务对账：%d 条上次遗留的任务已标记", n)
	}
	for i := 0; i < s.jobConcurrency; i++ {
		s.jobsWG.Add(1)
		go s.jobWorker(ctx)
	}
	// 队列里可能还有上次没跑完的 pending，唤醒一次让 worker 立刻去取。
	s.kick()
	return true
}

// WaitJobs 等池收尾（关停流程用）。没有池时立刻返回。
func (s *Service) WaitJobs(ctx context.Context, timeout time.Duration) error {
	s.poolMu.Lock()
	started := s.poolStarted
	s.poolMu.Unlock()
	if !started {
		return nil
	}
	done := make(chan struct{})
	go func() {
		s.jobsWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("任务池未在 %s 内退出", timeout)
	}
}

// kick 非阻塞投一个唤醒信号。
//
// 通道满（已经有信号在等）时直接丢弃是对的：唤醒是"有活了"这一条电平
// 信号，不是每个任务都要一发 —— 堆积会让空闲 worker 被历史信号唤醒空跑。
func (s *Service) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// jobWorker 是单个 worker 的取任务循环。
func (s *Service) jobWorker(ctx context.Context) {
	defer s.jobsWG.Done()
	tick := time.NewTicker(s.wakeInterval)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		j, err := s.claimJob(ctx)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// 没有待办：等活来、兜底 tick、或关停。
			select {
			case <-s.wake:
			case <-tick.C:
			case <-ctx.Done():
				return
			}
			continue
		case err != nil:
			// 领取失败（多半是库暂时不可用）。不能当"没有活"悄悄跳过：
			// 那会让 worker 安静地停在轮询上，而库里其实堆着一排 pending。
			logx.Error("领取任务失败: %v", err)
			select {
			case <-s.wake:
			case <-tick.C:
			case <-ctx.Done():
				return
			}
			continue
		}
		// claimJob 会把"排队期间已被取消"的任务在同一句里落成
		// canceled 并 RETURNING 回来。这一步是关键：领到 ≠ 该跑。若照旧
		// 交给执行器，用户已明确取消的删除会被真做掉——库里写着
		// canceled 而文件没了，比不取消更糟。
		if j.State != JobRunning {
			continue
		}
		s.runOneJob(ctx, j)
	}
}

// runOneJob 跑一条任务并落终态。
//
// 每个任务用自己的、派生自池上下文的 ctx，取消函数登记到 runningCancel：
// 用户取消经 RequestCancelJob 找到它，关停则从父 ctx 往下传导。
func (s *Service) runOneJob(poolCtx context.Context, j Job) {
	jobCtx, cancel := context.WithCancel(poolCtx)
	s.runningMu.Lock()
	s.runningCancel[j.ID] = cancel
	s.runningMu.Unlock()
	defer func() {
		s.runningMu.Lock()
		delete(s.runningCancel, j.ID)
		s.runningMu.Unlock()
		cancel()
	}()

	// panic 不能带走整台面板。一次复制里可能有用户可控路径、意料之外的
	// 文件类型；一个任务崩就把进程换掉的话，别的在跑的任务会一起变
	// interrupted，而用户完全不知道发生了什么。
	err := s.invoke(jobCtx, j)

	// 落终态必须用一个**没被取消**的上下文：关停和用户取消都会取消
	// jobCtx，可终态恰恰是在这两种情况下才最需要写进去的。
	s.finalize(j, err)
}

// invoke 调执行器并兜住 panic，把 panic 转成一条 failed 原因。
func (s *Service) invoke(jobCtx context.Context, j Job) (err error) {
	rep := s.newThrottle(jobCtx, j.ID)
	defer func() {
		if r := recover(); r != nil {
			logx.Error("任务 %d(%s) 执行 panic: %v\n%s", j.ID, j.Op, r, debug.Stack())
			err = fmt.Errorf("任务内部错误: %v", r)
		}
	}()
	if runErr := s.executor(jobCtx, j, rep.report); runErr != nil {
		return runErr
	}
	// 执行器正常返回也要把最后一段进度落下去：节流会把末尾几十 KB 的
	// 上报吞掉，任务在抽屉里显示"已完成"而进度条停在 99.8%，用户会以为
	// 它还在跑。
	return rep.flush()
}

// finalize 决定并写入终态。
//
// 先定义"被取消"的形状：执行器回 nil，或回一个 context.Canceled（含包装）。
// 其余错文本都算"真错"。
//
// 优先级（每一档都有理由，顺序不能随手换）：
//
//  1. 用户要求过取消 && 没出真错 → canceled。用户确实取消过，"已取消"
//     就是真相；哪怕紧接着面板崩了也不改。
//  2. 出了真错 → failed（带错文本）。**真错压倒取消意图**：用户在跑的中途
//     点了取消，而盘其实在那之前就满了 —— 记成"已取消"等于把他该知道的
//     事藏起来，他会以为自己的取消生效了、目标区是干净的，实际那是一堆
//     写到一半的碎片。错文本是唯一能告诉他"要重试之前先处理盘"的东西。
//  3. 面板在关停 → interrupted。区分"用户取消"与"面板退出"全靠这一步：
//     两者都取消了 jobCtx，含义却完全不同。一次正常重启把用户没取消过的
//     删除记成"已取消"，用户会以为自己的取消生效了。
//  4. 没人要求取消而执行器自述被取消 → canceled（带原文）。可疑但不是谎。
//  5. 其余（执行器正常返回）→ done。
func (s *Service) finalize(j Job, execErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cur, err := s.GetJob(ctx, j.ID)
	if errors.Is(err, ErrNoJob) {
		return // 任务被删了，没什么可写
	}
	if err != nil {
		logx.Error("落终态前读任务 %d 失败: %v", j.ID, err)
		return
	}
	// cancelLike: 没有错，或者错就是"被取消"本身。
	cancelLike := execErr == nil || errors.Is(execErr, context.Canceled)
	switch {
	case cur.CancelRequested && cancelLike:
		// 用户在跑的过程中点了取消，且没有别的错。
		if err := s.finishJob(ctx, j.ID, JobCanceled, "已取消"); err != nil {
			logx.Error("写任务 %d 取消终态失败: %v", j.ID, err)
		}
	case execErr != nil && !cancelLike:
		if err := s.finishJob(ctx, j.ID, JobFailed, execErr.Error()); err != nil {
			logx.Error("写任务 %d 失败终态失败: %v", j.ID, err)
		}
	case s.jobsCtx != nil && s.jobsCtx.Err() != nil:
		// 面板在退出，任务是被我们打断的，不是用户的错、也不是数据出错。
		// 停成 interrupted 等用户点重试，而不是假装成功或谎称失败。
		if err := s.finishJob(ctx, j.ID, JobInterrupted, "面板关闭，任务未完成"); err != nil {
			logx.Error("写任务 %d 中断终态失败: %v", j.ID, err)
		}
	case execErr != nil:
		if err := s.finishJob(ctx, j.ID, JobCanceled, execErr.Error()); err != nil {
			logx.Error("写任务 %d 取消终态失败: %v", j.ID, err)
		}
	default:
		if err := s.finishJob(ctx, j.ID, JobDone, ""); err != nil {
			logx.Error("写任务 %d 完成终态失败: %v", j.ID, err)
		}
	}
}

// cancelRunning 打断正在跑的任务（若有）。RequestCancelJob 落库之后调它。
//
// 只在库里记一个 flag、等 worker 下一块才发现，听起来也行 —— 但一个 10GB
// 的复制可能要几十分钟，而用户点取消是在他意识到"选错了"的那一刻。这里
// 让取消真的有牙齿。没有对应 goroutine（任务还在排队）时空转即可，那种
// 情况由 claimJob 的 CASE 兜住。
func (s *Service) cancelRunning(id int64) {
	s.runningMu.Lock()
	if cancel, ok := s.runningCancel[id]; ok {
		cancel()
	}
	s.runningMu.Unlock()
}

// ---------- 进度节流 ----------

// throttler 把执行器高频的进度上报折成低频落库（设计：每 200ms 或每 4MB）。
//
// 每写一块就 UPDATE 一次的话，一次大复制会打出几万次写 —— 而写提交在本机
// 是 200µs 量级，数据库会变成整条复制路径的瓶颈，还会把 WAL 撑大。
type throttler struct {
	svc         *Service
	jobCtx      context.Context
	id          int64
	clock       func() time.Time
	lastWrite   time.Time
	lastBytes   int64
	lastEntries int
	// pending 是最新一次还没落库的进度。最后一次靠 flush 收尾。
	pendBytes   int64
	pendEntries int
}

func (s *Service) newThrottle(jobCtx context.Context, id int64) *throttler {
	return &throttler{
		svc: s, jobCtx: jobCtx, id: id, clock: s.clock,
		// lastWrite 置零，让**第一次**上报一定落库：用户提交后要在抽屉
		// 里看到任务动起来，而不是等满 200ms 才从 0 跳一下。
	}
}

// report 是执行器拿到的回调。越过阈值才落库；被取消时回错误。
func (t *throttler) report(doneBytes int64, entriesDone int) error {
	if err := t.jobCtx.Err(); err != nil {
		return err
	}
	// 记下最新值（即使这次不落库，flush 也要拿到最终进度）。
	t.pendBytes, t.pendEntries = doneBytes, entriesDone
	now := t.clock()
	if !t.lastWrite.IsZero() &&
		now.Sub(t.lastWrite) < progressInterval &&
		doneBytes-t.lastBytes < progressBytes {
		return nil // 两个阈值都没越过，省掉这次写
	}
	return t.write()
}

// flush 把最后一段进度写下去（正常收尾用）。
func (t *throttler) flush() error {
	// 没越过任何阈值时也要看"自上次落库后有没有新进展"：字节或条目**任一**
	// 动过就得补写最后一段。只看字节会让删除类任务（done_bytes 恒 0）的
	// 最终 entries_done 永远停在第一次上报的值。
	if t.lastBytes == t.pendBytes && t.lastEntries == t.pendEntries {
		return nil // 最后一段已经落过库（或从头到尾没有任何进度）
	}
	// flush 不看取消：任务收尾时 jobCtx 往往已经因取消/关停被取消，而最终
	// 进度恰恰在这时最需要写对。用独立的短上下文。
	return t.writeForce()
}

func (t *throttler) write() error {
	if err := t.jobCtx.Err(); err != nil {
		return err
	}
	return t.writeForce()
}

func (t *throttler) writeForce() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.svc.setJobProgress(ctx, t.id, t.pendBytes, t.pendEntries); err != nil {
		return err
	}
	t.lastWrite = t.clock()
	t.lastBytes = t.pendBytes
	t.lastEntries = t.pendEntries
	if t.svc.progressObserver != nil {
		t.svc.progressObserver()
	}
	return nil
}

// runJob 是内置执行器（按 op 分派到 copy/move/delete），定义在 exec_run.go。
