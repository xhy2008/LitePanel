package filemgr

// M7-T5：后台任务并发数的热生效（SetJobConcurrency）。
//
// 这一组测试全部把 wakeInterval 拉到一个小时，于是"兜底轮询"这条路被关掉，
// 任何"要等一个周期才恢复"的实现都会在这里超时，而不是换个机器上悄悄变快
// 就看不见了。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"litepanel/internal/store"
)

// quiesce 让所有 worker 都阻塞在唤醒 select 上：提交 concurrency 条任务、
// 等它们全部跑完。跑完之后每个 worker 都会走一次 claimJob（库里已空）再
// 进 select —— 这是唯一能**可观察地**逼近"全部空闲阻塞"状态的做法。
//
// 没有这一步，缩编测试会走"worker 在循环开头自查退出"这条支路而通过，
// 于是测不到真正麻烦的那条路：worker 已经全部睡在 select 上，只能靠
// 被唤醒的那个把信号接力下去（见 waitForWork）。
func (e *hotEnv) quiesce(t testing.TB, ctx context.Context, n int) {
	t.Helper()
	e.submit(t, ctx, n)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.exec.count() < n {
		time.Sleep(2 * time.Millisecond)
	}
	e.exec.release()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := e.svc.ListJobs(ctx, JobFilter{})
		if err != nil {
			t.Fatal(err)
		}
		done := 0
		for _, j := range jobs {
			if j.State == JobDone {
				done++
			}
		}
		if done == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("quiesce: %d 条任务没能在 6s 内全部跑完", n)
}

// hotEnv 用独立的执行器夹具（每个测试自己一个 gate），因为这里要分阶段
// 放行任务：先卡住一批、改并发、再放行。
type hotEnv struct {
	svc       *Service
	exec      *blockingExec
	dir       string
	clk       *fakeClock
	submitted int
}

func newHotEnv(t testing.TB, concurrency int) *hotEnv {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(base, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	be := newBlockingExec()
	clk := &fakeClock{now: fakeNow}
	return &hotEnv{
		svc: NewService(Options{
			DB: db, Clock: clk.Now, JobConcurrency: concurrency,
			JobExecutor: be.exec, JobWakeInterval: time.Hour,
		}),
		exec: be, dir: base, clk: clk,
	}
}

func (e *hotEnv) mk(t testing.TB, rel, body string) string {
	t.Helper()
	p := filepath.Join(e.dir, rel)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// submit 提交 n 条删除任务。文件名带一个自增序号：同一个测试里会多次提交，
// 重名会让"删一个不存在的文件"这种错误和真正的断言混在一起。
func (e *hotEnv) submit(t testing.TB, ctx context.Context, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		e.submitted++
		if _, err := e.svc.CreateJob(ctx, JobInput{
			Op: OpDelete, Src: []string{e.mk(t, "job-"+itoa(e.submitted), "x")},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// 调高并发必须**立刻**补起 worker。
//
// 只改数字不补人的形态："并发 2 → 8"要等下次重启才生效，而用户改完就会
// 连着提交一批任务来验证 —— 这正是这条设置被使用的唯一方式。
func TestRaiseConcurrencySpawnsWorkers(t *testing.T) {
	e := newHotEnv(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	e.submit(t, ctx, 8)

	// 先确认卡在 2。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.exec.concurrent() < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := e.exec.concurrent(); got != 2 {
		t.Fatalf("初始并发应为 2，得 %d", got)
	}

	e.svc.SetJobConcurrency(5)
	if got := e.svc.JobDesired(); got != 5 {
		t.Errorf("期望值应为 5，得 %d", got)
	}
	// 观察事实（存活 worker 数）而不是意图：补 spawn 漏了的话 desired 照样是 5。
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.exec.concurrent() < 5 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := e.exec.concurrent(); got != 5 {
		t.Fatalf("调高到 5 之后同时在跑的任务数应到 5，得 %d（说明没补 spawn）", got)
	}
	e.exec.release()
}

// 调低并发**不得打断正在跑的任务**。
//
// 强行取消正在复制的目录是不可逆的半成品，而用户要的只是"以后别同时跑
// 这么多"。所以调低只影响后续取任务。
func TestLowerConcurrencyDoesNotKillRunning(t *testing.T) {
	e := newHotEnv(t, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	e.submit(t, ctx, 4)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.exec.concurrent() < 4 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := e.exec.concurrent(); got != 4 {
		t.Fatalf("应有 4 个在跑，得 %d", got)
	}

	e.svc.SetJobConcurrency(1)
	// 立刻调低之后，在跑的 4 个任务不得被取消：放行后它们都要正常完成。
	e.exec.release()
	jobs, err := e.svc.ListJobs(ctx, JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 4 {
		t.Fatalf("应有 4 条任务，得 %d", len(jobs))
	}
	for _, j := range jobs {
		if j.State == JobCanceled {
			t.Errorf("任务 %d 被并发数下调取消成了 canceled（正在复制的目录会留半成品）", j.ID)
		}
		waitState(t, e.svc, j.ID, JobDone, 3*time.Second)
	}
}

// 调低后存活 worker 数要收敛到期望值（不是一行代码就把它们杀掉，而是
// 各自在取任务的检查点自行退出）。
func TestLowerConcurrencyShrinksLiveCount(t *testing.T) {
	e := newHotEnv(t, 6)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	if got := e.svc.JobLive(); got != 6 {
		t.Fatalf("起池后应有 6 个 worker，得 %d", got)
	}
	// 先把 6 个 worker 都灌进 select（否则测的是另一条支路，见 quiesce）。
	e.exec.reset()
	e.quiesce(t, ctx, 6)
	e.svc.SetJobConcurrency(2)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.svc.JobLive() > 2 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := e.svc.JobLive(); got != 2 {
		t.Errorf("调低到 2 之后存活 worker 应收敛到 2，得 %d", got)
	}
}

// 调低之后**新提交的任务仍要立刻开始**。
//
// 这条与下一条（ShrinksLiveCount）合起来钉住"缩编要链式传播"：空闲 worker
// 全阻塞在同一个缓冲 1 的唤醒信号上，被唤醒的那个若悄悄走人，其余的永远
// 醒不过来。用 wakeInterval=1 小时关掉兜底轮询，于是任何"要等一个周期才
// 恢复"的实现都会在这里超时而不是在某些机器上悄悄变快就看不见。
func TestJobsStillStartAfterLowering(t *testing.T) {
	e := newHotEnv(t, 6)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	e.quiesce(t, ctx, 6)
	e.svc.SetJobConcurrency(1)
	e.exec.reset()
	e.submit(t, ctx, 3) // 不等收敛：缩编与新任务提交并发发生，这才是真实时序

	// 判据用**执行器真的进入过**，而不是 job 的 state：state 在提交瞬间
	// 就可能不是 pending，那种判据在有 bug 与没 bug 的两个版本里都会绿
	// （我第一版就栽在这里，写出了一个永远抓不到东西的测试）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.exec.count() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	e.exec.release()
	if got := e.exec.count(); got == 0 {
		t.Fatal("调低并发后提交的任务一条都没开始执行")
	}
}

// 池没启动时只记期望值，StartJobs 按它起。
// 否则"改设置 → 之后才起池"的场合（启动顺序、或没接 WS 的形态）会拿到
// 配置文件里的旧数字，而设置页显示的已经是新值。
func TestSetConcurrencyBeforeStart(t *testing.T) {
	e := newHotEnv(t, 2)
	e.svc.SetJobConcurrency(5)
	if got := e.svc.JobLive(); got != 0 {
		t.Errorf("池没起时不该有 worker，得 %d", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !e.svc.StartJobs(ctx) {
		t.Fatal("池没接上")
	}
	if got := e.svc.JobLive(); got != 5 {
		t.Errorf("起池应按期望值起 5 个 worker，得 %d", got)
	}
}

// 越界值夹到 [1,16]，与 config 那层的 1–16 一致。
// 0 尤其要紧：0 个 worker 的队列是一个完全无症状的瘫痪队列。
func TestSetConcurrencyClamps(t *testing.T) {
	e := newHotEnv(t, 2)
	for _, tc := range []struct{ in, want int }{{0, 1}, {-5, 1}, {17, maxJobConcurrency}, {1000, maxJobConcurrency}, {8, 8}} {
		e.svc.SetJobConcurrency(tc.in)
		if got := e.svc.JobDesired(); got != tc.want {
			t.Errorf("SetJobConcurrency(%d) 应得 %d，得 %d", tc.in, tc.want, got)
		}
	}
}

// 反复来回调并发不得泄漏 worker（每次都补 spawn，退出侧只按超编退出）。
// 泄漏的形态：用户来回调几次，worker 数越积越多，并发上限名存实亡。
func TestConcurrencyChurnDoesNotLeakWorkers(t *testing.T) {
	e := newHotEnv(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	e.quiesce(t, ctx, 2)
	for _, n := range []int{4, 1, 8, 2, 16, 3, 1} {
		e.svc.SetJobConcurrency(n)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.svc.JobLive() != 1 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := e.svc.JobLive(); got != 1 {
		t.Fatalf("来回调 7 次后存活 worker 应是 1（最后一次设的），得 %d", got)
	}
}

// 关停时缩编后的池也要能正常收尾（WaitJobs 等的是 jobsWG）。
// 缩编时 jobsWG 没有 Done 的话，关停会一直等到超时，表现为"重启面板卡住"。
func TestShrinkThenShutdownCompletes(t *testing.T) {
	e := newHotEnv(t, 5)
	ctx, cancel := context.WithCancel(context.Background())
	e.svc.StartJobs(ctx)
	e.svc.SetJobConcurrency(1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.svc.JobLive() > 1 {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- e.svc.WaitJobs(context.Background(), 3*time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("缩编后关停失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("缩编后 WaitJobs 挂住 —— 表现为重启面板卡死")
	}
}
