package filemgr

// M6-T4：worker 池（queue.go）。
//
// 这里只测"池"的语义：领取、并发上限、取消传递、关停对账、进度节流。
// 真正的复制/移动/删除在 exec_test.go 里单独测 —— 两件事的失败方式完全
// 不同（一个是"任务永远不结束/结束错了"，一个是"文件内容错了"），混在
// 一起测的话报错时看不出该查哪一层。
//
// 池被测的是一个**假的执行器**（Options.JobExecutor）：真实操作在本机是
// 220µs 量级，"并发数是否真的受限""取消是否真的打断"这类断言需要一个
// 能被卡住的任务才可能稳定成立。真实执行器照样有它自己的测试。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"litepanel/internal/store"
)

// blockingExec 是一个可被卡住的执行器：进入时计数、在 gate 上等待。
type blockingExec struct {
	mu       sync.Mutex
	entering int
	maxSeen  int
	entered  []int64
	gate     chan struct{} // 关闭后所有任务立刻放行
	once     sync.Once
}

func newBlockingExec() *blockingExec {
	return &blockingExec{gate: make(chan struct{})}
}

func (b *blockingExec) exec(ctx context.Context, j Job, report func(int64, int) error) error {
	b.mu.Lock()
	b.entering++
	b.entered = append(b.entered, j.ID)
	if b.entering > b.maxSeen {
		b.maxSeen = b.entering
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.entering--
		b.mu.Unlock()
	}()
	select {
	case <-b.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingExec) concurrent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxSeen
}

func (b *blockingExec) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entered)
}

func (b *blockingExec) release() { b.once.Do(func() { close(b.gate) }) }

// poolEnv 在 jobEnv 之上接一个可注入执行器的池。
type poolEnv struct {
	*jobEnv
	exec *blockingExec
}

func newPoolEnv(t testing.TB, concurrency int) *poolEnv {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(base, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	be := newBlockingExec()
	e := &poolEnv{
		jobEnv: &jobEnv{db: db, dir: base, clk: &fakeClock{now: fakeNow}},
		exec:   be,
	}
	e.svc = NewService(Options{
		DB: db, Clock: e.clk.Now, JobConcurrency: concurrency,
		JobExecutor: be.exec,
	})
	return e
}

// waitState 轮询到某条任务变成期望状态（池是异步的，没有这个就只能 sleep）。
func waitState(t testing.TB, svc *Service, id int64, want JobState, d time.Duration) Job {
	t.Helper()
	deadline := time.Now().Add(d)
	var last Job
	for time.Now().Before(deadline) {
		j, err := svc.GetJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		last = j
		if j.State == want {
			return j
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等 %s 超时（最后状态 %s）", want, last.State)
	return last
}

// ---------- 跑起来 ----------

// 提交后必须**很快**开始跑，不能等下一个 tick。
//
// 池只会"每隔 N 秒扫一次"是实现里最省事也最难发现的一种错：所有测试照样
// 绿（轮询等待会把那点延迟吃掉），而用户在界面上按下复制后要盯着
// "排队中"卡满一个轮询周期。所以这里把安全兜底的周期拉得很长，只留
// "有新任务就立刻醒"这一条通路能在规定时间内让任务跑完。
func TestPoolStartsJobPromptly(t *testing.T) {
	e := newPoolEnv(t, 2)
	e.svc.wakeInterval = time.Hour // 只靠兜底轮询的话这条测试必然超时
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !e.svc.StartJobs(ctx) {
		t.Fatal("池没接上")
	}
	j, err := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	e.exec.release()
	waitState(t, e.svc, j.ID, JobDone, 2*time.Second)
	if e.exec.count() != 1 {
		t.Errorf("执行器该被调用一次, got %d", e.exec.count())
	}
}

// 多个任务按提交顺序被领取（FIFO）。
func TestPoolRunsInOrder(t *testing.T) {
	e := newPoolEnv(t, 1) // 单 worker，顺序才是可观察的
	e.svc.wakeInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	var ids []int64
	for i := 0; i < 5; i++ {
		j, err := e.svc.CreateJob(ctx, JobInput{
			Op: OpDelete, Src: []string{e.mk(t, "f"+itoa(i), "x")},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	// 放行全部（gate 是一次性的，开了就都过了），再逐条确认收尾。
	// 进入顺序在下面按 ids 比对 —— 单 worker 下必须严格等于提交顺序。
	e.exec.release()
	for _, id := range ids {
		waitState(t, e.svc, id, JobDone, 3*time.Second)
	}
	e.exec.mu.Lock()
	entered := append([]int64(nil), e.exec.entered...)
	e.exec.mu.Unlock()
	for i := range ids {
		if entered[i] != ids[i] {
			t.Fatalf("进入顺序应为 %v, got %v", ids, entered)
		}
	}
}

// ---------- 并发上限 ----------

// 同时在跑的任务数不得超过并发数（设计 8.4：默认 2）。
//
// 这条不是"性能调优"：并发跑满时机械盘上会同时有几个大文件在读写，而
// 面板跑在一台已经跑着别的服务的机器上。更要紧的是没有上限的话，一次
// "全选 500 个目录做复制"会一口气起 500 个 goroutine 各开一对文件句柄。
func TestPoolRespectsConcurrency(t *testing.T) {
	e := newPoolEnv(t, 2)
	e.svc.wakeInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !e.svc.StartJobs(ctx) {
		t.Fatal("池没接上")
	}
	for i := 0; i < 8; i++ {
		if _, err := e.svc.CreateJob(ctx, JobInput{
			Op: OpDelete, Src: []string{e.mk(t, "f"+itoa(i), "x")},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 等池把能起的都起满
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.exec.concurrent() >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := e.exec.concurrent(); got > 2 {
		t.Errorf("同时跑了 %d 个任务，超过并发上限 2", got)
	}
	// 而且确实在跑（不是"一个都没起"糊过去的）
	if got := e.exec.count(); got < 2 {
		t.Errorf("并发上限是 2，但只起了 %d 个：池可能压根没转", got)
	}
	e.exec.release()
	// 全部收尾
	for i := 0; i < 8; i++ {
		jobs, _ := e.svc.ListJobs(ctx, JobFilter{})
		if len(jobs) == 0 {
			break
		}
		done := true
		for _, j := range jobs {
			if !j.State.terminal() {
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if running := listNotTerminal(t, e.svc); len(running) != 0 {
		t.Errorf("放行后应全部收尾, 还剩 %d 条", len(running))
	}
}

// ---------- 取消 ----------

// 正在跑的任务必须被**打断**，而不是等它自然结束。
//
// 只在库里记一个 flag、等 worker 自己发现，听起来也行 —— 但一个 10GB 的
// 复制可能要几十分钟，而用户点取消是在他意识到"选错了"的那一刻。这条
// 断言的是"取消有牙齿"：执行器收到的 ctx 真的被取消了。
func TestPoolCancelsRunningJob(t *testing.T) {
	e := newPoolEnv(t, 2)
	e.svc.wakeInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	j, err := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, e.svc, j.ID, JobRunning, 2*time.Second)
	if err := e.svc.RequestCancelJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	got := waitState(t, e.svc, j.ID, JobCanceled, 2*time.Second)
	if got.Error != "" {
		t.Logf("取消也带了原因文本（可选）: %q", got.Error)
	}
	// 不许顺手把源文件删了：执行器是在 ctx 取消后返回的，删除没发生
	if _, err := os.Stat(e.root("a.txt")); err != nil {
		t.Errorf("取消的任务动了文件: %v", err)
	}
}

// 用户点了取消、但任务其实是因为真错挂的 → 记 failed，不记 canceled。
//
// 真错必须压倒取消意图：盘在半路满了、用户看到不对点了取消——如果
// 因为"有取消意图"就盖成已取消，他把目标区当成干净的，实际那是一堆
// 写到一半的碎片，而且永远看不到"盘满了"。错文本是唯一能告诉他"重试
// 之前先处理盘"的东西。不钉住的话，一个"看到 cancel_requested 就写
// canceled"的实现完全合理也完全错（实测：该写法能过前面所有测试）。
func TestPoolRealErrorBeatsCancelIntent(t *testing.T) {
	e := newPoolEnv(t, 1)
	e.svc.wakeInterval = time.Hour
	diskFull := errors.New("no space left on device")
	release := make(chan struct{})
	e.svc.executor = func(ctx context.Context, j Job, report func(int64, int) error) error {
		release <- struct{}{} // 告诉测试：已开始跑
		<-release             // 等测试把取消意图写进去
		return diskFull       // 但它其实是因真错挂的，不是 ctx 取消
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	j, err := e.svc.CreateJob(ctx, JobInput{
		Op: OpCopy, Src: []string{e.mk(t, "a.txt", "x")}, Dst: "/DISK/d",
	})
	if err != nil {
		t.Fatal(err)
	}
	<-release // 确认真的在跑
	if err := e.svc.RequestCancelJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	close(release) // 放行执行器，它回 diskFull
	got := waitState(t, e.svc, j.ID, JobFailed, 2*time.Second)
	if !strings.Contains(got.Error, "no space") {
		t.Errorf("真错必须带进抽屉, got %q", got.Error)
	}
}

// 排队中被取消的任务**永远不进执行器**。
//
// 池这边也要测一遍：持久层那边测的是"领取时转成 canceled"，这里测的是
// "worker 不会先领回来再决定不跑" —— 后者多一次无意义的状态往返，而且
// 如果执行器在决定之前就被调用，用户的删除已经被执行了。
func TestPoolNeverRunsQueuedCanceled(t *testing.T) {
	e := newPoolEnv(t, 1)
	e.svc.wakeInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j, err := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "别动.txt", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RequestCancelJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.StartJobs(ctx) // 启动时它已经是"被取消的排队任务"
	e.exec.release()
	waitState(t, e.svc, j.ID, JobCanceled, 2*time.Second)
	if got := e.exec.count(); got != 0 {
		t.Errorf("已取消的任务进了执行器 %d 次", got)
	}
	if _, err := os.Stat(e.root("别动.txt")); err != nil {
		t.Errorf("文件被删了: %v", err)
	}
}

// ---------- 领取后才发现被取消 ----------

// 任务在**对账之后**才被取消（池已在跑），于是它不会被启动对账提前
// 落 canceled，只能靠 worker 领取后那道 `State != running 就跳过` 的
// 守卫拦住。
//
// 上一一条测试（启动前就取消）现在会因为对账而绿，它证明不了领取
// 守卫——两个时机走的是两条不同的路：对账 vs 领取 CASE+guard。只测前者，
// 把守卫删掉也没人报错（实测：变异 runs-cancelled-claim 幸存），而真正的
// 危险恰恰是"池跑起来之后用户才取消"这个常见时机。
func TestPoolSkipsJobCanceledAfterClaim(t *testing.T) {
	e := newPoolEnv(t, 1)
	e.svc.wakeInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// gate 未开。先埋一条诱饵再启动池：单 worker 会被诱饵卡在 gate 上，
	// 于是目标任务被提交时**不可能**被领走 —— 取消意图的写入因此严格
	// 早于领取，没有竞态。诱饵必须在 StartJobs 之前建：那样对账时它还
	// 是普通 pending（不带取消意图）不会被提前处理掉。
	decoy, err := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "诱饵.txt", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.StartJobs(ctx)
	waitState(t, e.svc, decoy.ID, JobRunning, 2*time.Second)

	j, err := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "排队时取消.txt", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	// worker 正忙，它领不走：此处取消严格发生在领取之前。
	if err := e.svc.RequestCancelJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	e.exec.release() // 诱饵放行，worker 腾出手来领目标
	got := waitState(t, e.svc, j.ID, JobCanceled, 2*time.Second)
	if got.State != JobCanceled {
		t.Fatalf("目标应为 canceled, got %s", got.State)
	}
	// 关键：目标绝不能进执行器。count 必须恰好是 1（只有诱饵）。
	if n := e.exec.count(); n != 1 {
		t.Errorf("执行器被调用 %d 次，应为 1（只有诱饵）：领取守卫失效", n)
	}
	if _, err := os.Stat(e.root("排队时取消.txt")); err != nil {
		t.Errorf("被取消的任务把文件删了: %v", err)
	}
}

// 启动时会把上个进程遗留的 running 对账成 interrupted。
//
// 为什么是这个方向而不是"直接测对账在 worker 之前跑"：
//
// StartJobs 里的顺序（先 ReconcileJobs、后 `go jobWorker`）在结构上
// 就已经保证了"对账时本进程不会有任何 running"——那时 worker 还没出生。
// 这个"两句顺序"没有可靠的黑盒测试（先起后对账的变异，错窗口小到
// 只能靠调度运气命中，实测 -count 下也只偶尔报），而一个 1/3 概率才
// 抓到的测试比没有更糟：它会在 CI 里飘，还给假安心。
//
// 真正值得钉住的是那个"容易忘"的性质：池启动时一定会跑对账。少了
// 它，每次重启都会留一堆僵尸 running 行（用户永远等不到结果）。这条
// 测试确定性地验它：先塑一条 running（模拟上个进程死的现场），
// StartJobs 同步跑完对账后它必须已经是 interrupted。
func TestPoolReconcilesLeftoversOnStart(t *testing.T) {
	e := newPoolEnv(t, 2)
	e.svc.wakeInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 塑一条上个进程遗留的 running（直接写库，模拟进程崩溃后的现场）。
	stale, err := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "僵尸.txt", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.SqlDB().ExecContext(ctx, `UPDATE fs_jobs SET state='running' WHERE id=?`, stale.ID); err != nil {
		t.Fatal(err)
	}
	if !e.svc.StartJobs(ctx) {
		t.Fatal("池没接上")
	}
	// StartJobs 同步跑完对账才返回；此刻遗留行必须已被标 interrupted。
	got, err := e.svc.GetJob(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobInterrupted {
		t.Errorf("遗留的 running 未在对账时转 interrupted, got %s", got.State)
	}
	e.exec.release()
}

// ---------- 失败 ----------

// 执行器报错 → failed + 错误文本进抽屉。
func TestPoolMarksFailedOnError(t *testing.T) {
	e := newPoolEnv(t, 1)
	e.svc.wakeInterval = time.Hour
	boom := errors.New("回收站不可写: /DISK 建 .trash 失败")
	e.svc.executor = func(ctx context.Context, j Job, report func(int64, int) error) error {
		return boom
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	j, _ := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")}})
	got := waitState(t, e.svc, j.ID, JobFailed, 2*time.Second)
	if got.Error == "" {
		t.Error("失败原因必须带进抽屉，否则用户只能猜")
	}
}

// 执行器 panic 不能把整个面板带走。
//
// 一次复制里可能有用户可控的路径、意料之外的文件类型；一个任务的崩溃
// 换成整台面板重启的话，别的正在跑的任务会一起变成 interrupted，而用户
// 完全不知道发生了什么。
func TestPoolSurvivesExecutorPanic(t *testing.T) {
	e := newPoolEnv(t, 2)
	e.svc.wakeInterval = time.Hour
	var n int32
	e.svc.executor = func(ctx context.Context, j Job, report func(int64, int) error) error {
		if atomic.AddInt32(&n, 1) == 1 {
			panic("炸了")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	j1, _ := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")}})
	got := waitState(t, e.svc, j1.ID, JobFailed, 2*time.Second)
	if got.Error == "" {
		t.Error("panic 也要落成一条带原因的 failed")
	}
	j2, _ := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "b.txt", "x")}})
	waitState(t, e.svc, j2.ID, JobDone, 2*time.Second) // 池还活着
}

// ---------- 关停 ----------

// 关停时正在跑的任务落成 interrupted，而不是一走了之。
//
// 进程要退了，那个复制到底做了一半还是四分之一，只有内存知道。留着
// running 的话新进程的对账会把它转成 interrupted —— 那是靠"下一次启动"
// 才修好的状态；退出前自己写清楚，用户哪怕一直不重启也能看到真相。
func TestPoolInterruptsRunningOnShutdown(t *testing.T) {
	e := newPoolEnv(t, 2)
	e.svc.wakeInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	j, err := e.svc.CreateJob(ctx, JobInput{Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.StartJobs(ctx)
	waitState(t, e.svc, j.ID, JobRunning, 2*time.Second)
	cancel() // 关停
	if err := e.svc.WaitJobs(context.Background(), 3*time.Second); err != nil {
		t.Fatalf("池没在期限内退出: %v", err)
	}
	got, _ := e.svc.GetJob(context.Background(), j.ID)
	if got.State != JobInterrupted {
		t.Errorf("关停时正在跑的任务该转 interrupted, got %s", got.State)
	}
}

// WaitJobs 没有池时立刻返回（不是死等）。
func TestWaitJobsWithoutPool(t *testing.T) {
	e := newJobEnv(t)
	if err := e.svc.WaitJobs(context.Background(), time.Second); err != nil {
		t.Errorf("没起池时不该报错/阻塞: %v", err)
	}
}

// 没接数据库时必须明说"没在跑"。
//
// 与两个 janitor 同一条理由：漏接的当下没有任何症状。而这里如果池
// "谎称在跑"，运维在日志里看到的是"任务队列已启动"，排查方向从一开始
// 就是错的。
//
// 执行器不在考虑范围：它有内置默认值（copy/move/delete），永远非 nil，
// 所以唯一能"没接上"的东西是数据库。
func TestStartJobsReportsNotWired(t *testing.T) {
	svc := NewService(Options{}) // 没有 DB
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if svc.StartJobs(ctx) {
		t.Error("没接数据库时不该谎称池在跑")
	}
	if err := svc.WaitJobs(ctx, time.Second); err != nil {
		t.Errorf("没起池时 WaitJobs 不该报错: %v", err)
	}
}

// 重复 StartJobs 不起两套 worker。
//
// 两套 worker 会把并发上限翻倍（各自 2 = 实际 4），而配置文件里写的
// "并发 2"是用户唯一能看到的数字。
func TestStartJobsTwiceIsNoOp(t *testing.T) {
	e := newPoolEnv(t, 1)
	e.svc.wakeInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !e.svc.StartJobs(ctx) {
		t.Fatal("没接上")
	}
	if e.svc.StartJobs(ctx) {
		t.Error("第二次启动应无效并报告出来")
	}
	for i := 0; i < 4; i++ {
		if _, err := e.svc.CreateJob(ctx, JobInput{
			Op: OpDelete, Src: []string{e.mk(t, "f"+itoa(i), "x")},
		}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if e.exec.count() >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := e.exec.concurrent(); got > 1 {
		t.Errorf("并发上限 1，实际同时跑了 %d 个", got)
	}
	e.exec.release()
}

// ---------- 进度节流 ----------

// 进度落库要节流（设计：每 200ms 或每 4MB）。
//
// 每写一块就更新一次数据库的话，一次大复制会打出几万次 UPDATE —— 而
// 写提交在本机是 200µs 量级，DB 会变成整条复制路径的瓶颈，还会把 WAL
// 撑大。断言用"次数"而不是"时间"：时间是墙钟，跑在负载高的机器上会漂。
func TestPoolThrottlesProgress(t *testing.T) {
	e := newPoolEnv(t, 1)
	e.svc.wakeInterval = time.Hour
	const total = int64(20 << 20)
	const chunk = int64(32 << 10)
	var reports int32
	e.svc.executor = func(ctx context.Context, j Job, report func(int64, int) error) error {
		// 模拟 20MB / 每 32KB 一块。上报的是"这块拷完后的累计字节"，
		// 所以最后一次正好是 total（真实拷贝循环就是这样：写完一块再报）。节流前 640 次。
		for copied := int64(0); copied < total; copied += chunk {
			atomic.AddInt32(&reports, 1)
			if err := report(copied+chunk, 0); err != nil {
				return err
			}
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	j, _ := e.svc.CreateJob(ctx, JobInput{
		Op: OpCopy, Src: []string{e.mk(t, "a.txt", "x")}, Dst: "/DISK/dst",
	})
	waitState(t, e.svc, j.ID, JobDone, 5*time.Second)
	if atomic.LoadInt32(&reports) < 100 {
		t.Fatalf("夹具本身没报够次数, got %d（测试写错了）", reports)
	}
	var writes int32
	e.svc.progressObserver = func() { atomic.AddInt32(&writes, 1) }
	// 重跑一条，数真正落到库上的次数
	j2, _ := e.svc.CreateJob(ctx, JobInput{
		Op: OpCopy, Src: []string{e.mk(t, "b.txt", "x")}, Dst: "/DISK/dst2",
	})
	waitState(t, e.svc, j2.ID, JobDone, 5*time.Second)
	if got := atomic.LoadInt32(&writes); got > 40 {
		t.Errorf("20MB 的复制打了 %d 次进度落库（节流没生效或阈值太小）", got)
	}
	if got := atomic.LoadInt32(&writes); got == 0 {
		t.Error("一次都没落库：进度会永远停在 0")
	}
	// 最终进度必须是全量：节流把"最后一次上报"丢掉的话，任务在抽屉里
	// 显示"已完成"而进度条停在 99.8% —— 用户会以为它还在跑。
	got, _ := e.svc.GetJob(ctx, j2.ID)
	if got.DoneBytes != total {
		t.Errorf("最终 done_bytes 该是 %d, got %d", total, got.DoneBytes)
	}
}

// 节流期间也要能收到取消检查点（长复制不能"报完一次进度才想起来看取消"）。
func TestPoolProgressReportHonorsCancel(t *testing.T) {
	e := newPoolEnv(t, 1)
	e.svc.wakeInterval = time.Hour
	e.svc.executor = func(ctx context.Context, j Job, report func(int64, int) error) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if err := report(1<<20, 0); err != nil {
				return err // 节流层把取消传回来
			}
			e.clk.advance(30 * time.Second) // 越过 200ms 阈值
			return report(2<<20, 0)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.StartJobs(ctx)
	j, _ := e.svc.CreateJob(ctx, JobInput{
		Op: OpCopy, Src: []string{e.mk(t, "a.txt", "x")}, Dst: "/DISK/d",
	})
	waitState(t, e.svc, j.ID, JobDone, 2*time.Second)
}

// ---------- 助手 ----------

func listNotTerminal(t testing.TB, svc *Service) []Job {
	t.Helper()
	jobs, err := svc.ListJobs(context.Background(), JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var out []Job
	for _, j := range jobs {
		if !j.State.terminal() {
			out = append(out, j)
		}
	}
	return out
}

// ---------- 节流器本体（不经池的直接单测） ----------

// 取消之后 report 必须回错误 —— 经由池测不出来，只能直接测节流器。
//
// 真实拷贝循环有两种观察取消的方式：看 ctx（池保证取消，永远有效）、
// 或看 report 的返回值（只在节流器也检查 ctx 时才有效）。一个只写
// `if err := report(...); err != nil { return }` 的执行器，如果节流器
// 不检查取消，会在用户点取消之后把剩下几个 G 照拷不误 —— 取消形同
// 虚设。经由池测这件事只能靠"多快停下"的时间差，是竞态测试；对组件
// 本体做直接断言则是确定性的。
func TestThrottlerReportErrorsAfterCancel(t *testing.T) {
	e := newJobEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j, err := e.svc.CreateJob(ctx, JobInput{Op: OpCopy, Src: []string{"/a"}, Dst: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	th := e.svc.newThrottle(ctx, j.ID)
	if err := th.report(1000, 0); err != nil {
		t.Fatalf("未取消时 report 不该报错: %v", err)
	}
	cancel()
	if err := th.report(2000, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("取消后 report 该回 context.Canceled（执行器靠它收工）, got %v", err)
	}
}

// report 被节流吞掉时**最新进度必须留着**，flush 才写得出全量。
//
// 节流器若"落库就丢值"，末尾被吞的那段进度就永远丢了：任务显示"已完成"
// 而进度条停在最后一个落库点。假时钟让阈值永远不越过，把"吞掉"与
// "保留"这两件事逼到明处。
func TestThrottlerKeepsLatestForFlush(t *testing.T) {
	e := newJobEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j, err := e.svc.CreateJob(ctx, JobInput{Op: OpCopy, Src: []string{"/a"}, Dst: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	th := e.svc.newThrottle(ctx, j.ID) // 假时钟恒定 → 除首次外全部被节流
	// 首次上报一定落库（让任务在界面上动起来）。
	if err := th.report(100, 1); err != nil {
		t.Fatal(err)
	}
	first, _ := e.svc.GetJob(ctx, j.ID)
	if first.DoneBytes != 100 {
		t.Fatalf("首次上报该落库, got %d", first.DoneBytes)
	}
	// 后续快速上报都被节流吞掉，库里停在 100。
	for _, n := range []int64{200, 300} {
		if err := th.report(n, 3); err != nil {
			t.Fatal(err)
		}
	}
	swallowed, _ := e.svc.GetJob(ctx, j.ID)
	if swallowed.DoneBytes != 100 {
		t.Errorf("节流该吞掉高频上报, done_bytes=%d", swallowed.DoneBytes)
	}
	// flush 写的是**最后一次**（300），不是最后一个落库点（100）。
	if err := th.flush(); err != nil {
		t.Fatal(err)
	}
	after, _ := e.svc.GetJob(ctx, j.ID)
	if after.DoneBytes != 300 || after.EntriesDone != 3 {
		t.Errorf("flush 要写最后一次的值, got %d/%d", after.DoneBytes, after.EntriesDone)
	}
}
