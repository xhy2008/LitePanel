package filemgr

// M6-T4：后台文件任务的持久层（fs_jobs，0006 迁移）。
//
// 这一层只管"任务存在盘上、状态转移合法、重启后还能对账"。执行在
// queue.go，HTTP 只碰 List/Get/RequestCancel 三个 —— claim/progress/finish
// 刻意不导出：队列一旦能被 HTTP 层调用，"谁决定任务在跑"就有了第二个主人。

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"litepanel/internal/store"
)

// fakeNow 是本包测试统一的"现在"。任务的时间戳断言要确定值，不能用
// time.Now（否则 created_at 的断言会随真实时钟漂）。
var fakeNow = time.Unix(1_800_000_000, 0)

type jobEnv struct {
	svc *Service
	db  *store.DB
	dir string // 已解析符号链接的可写目录
	clk *fakeClock
}

func newJobEnv(t testing.TB) *jobEnv {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(base, "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &jobEnv{db: db, dir: base, clk: &fakeClock{now: fakeNow}}
	e.svc = NewService(Options{DB: db, Clock: e.clk.Now})
	return e
}

func (e *jobEnv) mk(t testing.TB, rel, body string) string {
	t.Helper()
	p := filepath.Join(e.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------- 建与读 ----------

// 建一个任务要落库并回 id（设计 115 行：立即落库 + 返回 job_id）。
func TestJobCreatePersists(t *testing.T) {
	e := newJobEnv(t)
	a := e.mk(t, "a.txt", "1234")
	b := e.mk(t, "b.txt", "56789")
	j, err := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpCopy, Src: []string{a, b}, Dst: filepath.Join(e.dir, "dst"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if j.ID == 0 {
		t.Error("没回 id：前端拿它订阅进度")
	}
	if j.State != JobPending {
		t.Errorf("新建该是 pending, got %s", j.State)
	}
	if j.CreatedAt != fakeNow.Unix() {
		t.Errorf("created_at 该用注入时钟: %d", j.CreatedAt)
	}
	got, err := e.svc.GetJob(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 多个源路径必须原样回来（一次框选 500 个文件是一个任务，不是 500 个）
	if len(got.Src) != 2 || got.Src[0] != a || got.Src[1] != b {
		t.Errorf("Src 没原样往返: %v", got.Src)
	}
	if got.Dst != filepath.Join(e.dir, "dst") {
		t.Errorf("Dst = %q", got.Dst)
	}
	if got.EntriesTotal != 2 {
		t.Errorf("EntriesTotal 该是源路径数, got %d", got.EntriesTotal)
	}
}

// delete 没有目标：Dst 必须为空而任务合法。
func TestJobCreateDeleteHasNoDst(t *testing.T) {
	e := newJobEnv(t)
	j, err := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "x.txt", "x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if j.Dst != "" {
		t.Errorf("delete 不该有目标: %q", j.Dst)
	}
}

// 非法入参在**入库前**拒：一个 op 拼错的任务落库之后，worker 取到它只能
// 立刻失败，而用户在抽屉里看到的是"一条平白无故失败的记录"，比 400 更难懂。
func TestJobCreateRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   JobInput
	}{
		{"没有源路径", JobInput{Op: OpCopy, Dst: "/DISK/x"}},
		{"未知 op", JobInput{Op: JobOp("chmod"), Src: []string{"/a"}, Dst: "/b"}},
		{"copy 没有目标", JobInput{Op: OpCopy, Src: []string{"/a"}}},
		{"move 没有目标", JobInput{Op: OpMove, Src: []string{"/a"}}},
		{"源里有空串", JobInput{Op: OpDelete, Src: []string{""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newJobEnv(t)
			_, err := e.svc.CreateJob(context.Background(), tc.in)
			if err == nil {
				t.Fatal("应被拒绝")
			}
			// 必须是领域错误而不是数据库的 CHECK 违例：fs_jobs 上也有
			// CHECK(op/state)，所以"报错"这件事本身证明不了什么，而一条
			// `_SQLite error: CHECK constraint failed_` 到了 HTTP 层会
			// 变成 500 "服务器内部错误" —— 用户填错参数却看到面板坏了。
			if !errors.Is(err, ErrJobInput) {
				t.Errorf("应 ErrJobInput（HTTP 层据此回 400）, got %v", err)
			}
			jobs, _ := e.svc.ListJobs(context.Background(), JobFilter{})
			if len(jobs) != 0 {
				t.Errorf("被拒的任务不该留下记录: %+v", jobs)
			}
		})
	}
}

// 源里有重复路径要**去重**后再算条目数。
//
// 前端"全选 + 手点两下"很容易交出重复项；不去重的话一个任务会把同一个
// 文件处理两次 —— copy 会得到"目标已存在"的假失败，delete 第二次必然
// 报 404，而用户明明只点了一次删除。
func TestJobCreateDedupesSrc(t *testing.T) {
	e := newJobEnv(t)
	a := e.mk(t, "a.txt", "x")
	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{a, a, a}})
	if err != nil {
		t.Fatal(err)
	}
	if j.EntriesTotal != 1 {
		t.Errorf("重复源该被去掉, got %d", j.EntriesTotal)
	}
}

// 未知 id 回可判定的错误（HTTP 层据此回 404，而不是 500）。
func TestJobGetUnknownID(t *testing.T) {
	e := newJobEnv(t)
	if _, err := e.svc.GetJob(context.Background(), 4242); !errors.Is(err, ErrNoJob) {
		t.Fatalf("应 ErrNoJob, got %v", err)
	}
}

// ---------- 列举 ----------

// 空列表必须是空切片不是 nil。
func TestJobListEmptyIsNotNil(t *testing.T) {
	e := newJobEnv(t)
	jobs, err := e.svc.ListJobs(context.Background(), JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if jobs == nil {
		t.Error("nil 会序列化成 null，前端 jobs.map 直接白屏")
	}
}

// 列表按 id 倒序（最新在最前），并且能按状态筛。
//
// 倒序是界面顺序：抽屉里"刚提交的在顶上"。执行顺序相反（FIFO，见
// TestClaimIsFIFO）—— 两者必须分别是自己的顺序，用同一条 SQL 顺手
// 实现会让人以为它们一致。
func TestJobListOrderAndFilter(t *testing.T) {
	e := newJobEnv(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		j, err := e.svc.CreateJob(context.Background(), JobInput{
			Op: OpDelete, Src: []string{e.mk(t, "f"+string(rune('0'+i)), "x")},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	jobs, err := e.svc.ListJobs(context.Background(), JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || jobs[0].ID != ids[2] || jobs[2].ID != ids[0] {
		t.Fatalf("应按 id 倒序: %v", idsOf(jobs))
	}
	// 把中间那个跑完
	if err := e.svc.finishJob(context.Background(), ids[1], JobDone, ""); err != nil {
		t.Fatal(err)
	}
	running, err := e.svc.ListJobs(context.Background(), JobFilter{State: JobPending})
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 2 {
		t.Errorf("按 pending 筛应 2 条, got %d", len(running))
	}
	done, _ := e.svc.ListJobs(context.Background(), JobFilter{State: JobDone})
	if len(done) != 1 || done[0].ID != ids[1] {
		t.Errorf("按 done 筛不对: %v", idsOf(done))
	}
}

// 列表要有上限。
//
// 每次删除都留一行，跑一年就是几万条；抽屉一次拉全量会把首屏拖慢，
// 而用户只看最近几条。上限取"进行中全部 + 最近若干条已完成"，不能简单
// 倒序截断 —— 那样一个跑了很久、排在几万条之前的任务会从界面上消失，
// 用户以为它没了。
func TestJobListCapsHistoryButKeepsActive(t *testing.T) {
	e := newJobEnv(t)
	// 造一批已完成的历史
	for i := 0; i < maxJobHistory+5; i++ {
		j, err := e.svc.CreateJob(context.Background(), JobInput{
			Op: OpDelete, Src: []string{e.mk(t, "old"+itoa(i), "x")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.svc.finishJob(context.Background(), j.ID, JobDone, ""); err != nil {
			t.Fatal(err)
		}
	}
	// 再提交一个仍然 pending 的，然后把已完成的历史堆到超过上限
	early, err := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "还在跑.txt", "x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var doneIDs []int64
	for i := 0; i < maxJobHistory+3; i++ {
		j, err := e.svc.CreateJob(context.Background(), JobInput{
			Op: OpDelete, Src: []string{e.mk(t, "hist"+itoa(i), "x")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.svc.finishJob(context.Background(), j.ID, JobFailed, "boom"); err != nil {
			t.Fatal(err)
		}
		doneIDs = append(doneIDs, j.ID)
	}
	jobs, err := e.svc.ListJobs(context.Background(), JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) > maxJobHistory+1 {
		t.Errorf("列表该有上限（%d 条终态 + 进行中），got %d", maxJobHistory, len(jobs))
	}
	in := map[int64]bool{}
	for _, j := range jobs {
		in[j.ID] = true
	}
	// 进行中的必须在（哪怕它排在几万条历史之前）
	if !in[early.ID] {
		t.Error("仍在排队的任务被历史挤掉了 —— 它会从界面上消失，而它还在跑")
	}
	// 截断必须留**最近**的那些：留最老的话用户打开抽屉看到的是一年前的
	// 记录，而刚才那次删除根本不在里面 —— 条数对了，界面仍然是错的。
	if !in[doneIDs[len(doneIDs)-1]] {
		t.Error("最近一次已完成任务不在列表里：截断留的是最老的而不是最近的")
	}
	if in[doneIDs[0]] {
		t.Errorf("最老的历史仍在列表里（截断方向反了？）")
	}
}

func idsOf(jobs []Job) []int64 {
	out := make([]int64, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}

// ---------- 取任务（worker 的原子领取）----------

// 领取必须按 FIFO，而且两个 worker 不可能拿到同一个任务。
//
// 并发数默认 2（设计 8.4），所以"两个 worker 同时来取"是常态。领到同一个
// 任务的后果是同一批文件被处理两次：copy 会得到一次"目标已存在"的假失败，
// 而 delete 第二次必然 404 —— 用户看到的是一次正常删除配一条红色报错。
//
// 这里真起并发：串行领取测不出竞态（少了交错就没有竞争）。
func TestClaimIsFIFOAndExclusive(t *testing.T) {
	e := newJobEnv(t)
	var want []int64
	for i := 0; i < 24; i++ {
		j, err := e.svc.CreateJob(context.Background(), JobInput{
			Op: OpDelete, Src: []string{e.mk(t, "f"+itoa(i), "x")},
		})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, j.ID)
	}
	const workers = 8
	var mu sync.Mutex
	claimed := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := e.svc.claimJob(context.Background())
				if errors.Is(err, sql.ErrNoRows) {
					return
				}
				if err != nil {
					t.Errorf("领取失败: %v", err)
					return
				}
				mu.Lock()
				claimed[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != len(want) {
		t.Errorf("应恰好领取 %d 个, got %d", len(want), len(claimed))
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("任务 %d 被领了 %d 次：同一批文件会被处理两遍", id, n)
		}
	}
	// 取完再取该明确"没有了"
	if _, err := e.svc.claimJob(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("没有待办时应 sql.ErrNoRows, got %v", err)
	}
	// 被领取的任务都转成 running
	for _, j := range listAll(t, e) {
		if j.State != JobRunning {
			t.Errorf("领取后该是 running, got %s", j.State)
		}
	}
}

// 已完成/已取消的永不被领取。
func TestClaimSkipsFinished(t *testing.T) {
	e := newJobEnv(t)
	j, err := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.finishJob(context.Background(), j.ID, JobDone, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.claimJob(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("已完成的任务不该被再次领取, got %v", err)
	}
}

// ---------- 取消 ----------

// 排队中的任务被取消：先记"用户要取消"，由 worker 落地成 canceled。
//
// 意图必须**落盘**。只把 cancelFunc 放在内存里的话：任务还在排队时面板
// 崩了/升级了，重启后队列把这个用户已经明确说"别做"的 delete 真的执行掉
// —— 那是数据丢失，而且是用户以为自己已经取消掉之后发生的。
func TestRequestCancelQueuedJob(t *testing.T) {
	e := newJobEnv(t)
	j, err := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RequestCancelJob(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.GetJob(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CancelRequested {
		t.Error("取消意图必须落盘")
	}
	if got.State != JobPending {
		t.Errorf("worker 还没跑到，状态不该就地变 canceled: got %s", got.State)
	}
	// 领取时该直接落地成 canceled 而不是被执行
	claimed, err := e.svc.claimJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != JobCanceled {
		t.Errorf("带取消意图的任务被领走时该转为 canceled, got %s", claimed.State)
	}
	if _, err := os.Stat(e.root("a.txt")); err != nil {
		t.Errorf("已取消的任务不该动文件: %v", err)
	}
}

// 取消未知 id 回 ErrNoJob。
func TestRequestCancelUnknownID(t *testing.T) {
	e := newJobEnv(t)
	if err := e.svc.RequestCancelJob(context.Background(), 99); !errors.Is(err, ErrNoJob) {
		t.Fatalf("应 ErrNoJob, got %v", err)
	}
}

// 已完成的任务取消回一个可判定的错误（HTTP 层据此回 409）。
//
// 静默成功更"友好"，但它谎报了：什么都没发生。用户会以为已经中止了，
// 而实际上任务早就跑完了。
func TestRequestCancelFinishedJob(t *testing.T) {
	e := newJobEnv(t)
	j, err := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.finishJob(context.Background(), j.ID, JobDone, ""); err != nil {
		t.Fatal(err)
	}
	err = e.svc.RequestCancelJob(context.Background(), j.ID)
	if !errors.Is(err, ErrJobNotCancellable) {
		t.Fatalf("应 ErrJobNotCancellable, got %v", err)
	}
}

// ---------- 收尾 ----------

// 带取消意图的任务即使 worker 跑到底，也**不能**标成 done。
//
// worker 在块边界观察到取消后清理半成品、然后正常返回 nil —— 如果 finish
// 照单写 done，用户在抽屉里看到的是"已完成"，而实际文件被删了一半。
// 这类"看起来成功了"的假成功比一次红色失败危险得多。
func TestFinishKeepsCanceledForCancelRequested(t *testing.T) {
	e := newJobEnv(t)
	j, err := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := e.svc.claimJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RequestCancelJob(context.Background(), claimed.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.finishJob(context.Background(), claimed.ID, JobDone, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.GetJob(context.Background(), j.ID)
	if got.State != JobCanceled {
		t.Errorf("用户取消过的任务最终该是 canceled, got %s", got.State)
	}
}

// failed 要带上错误文本（抽屉里唯一的线索）。
func TestFinishFailedKeepsError(t *testing.T) {
	e := newJobEnv(t)
	j, _ := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "a.txt", "x")},
	})
	if err := e.svc.finishJob(context.Background(), j.ID, JobFailed, "权限不足"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.GetJob(context.Background(), j.ID)
	if got.State != JobFailed || got.Error != "权限不足" {
		t.Errorf("failed 该带错误文本: %+v", got)
	}
}

// 进度可见（含节流由调用方负责：这里证明"写了就能读到"）。
func TestJobProgressVisible(t *testing.T) {
	e := newJobEnv(t)
	j, _ := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpCopy, Src: []string{e.mk(t, "a.txt", "xxx")}, Dst: "/DISK/d",
	})
	if err := e.svc.setJobProgress(context.Background(), j.ID, 2, 1); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.GetJob(context.Background(), j.ID)
	if got.DoneBytes != 2 || got.EntriesDone != 1 {
		t.Errorf("进度没落库: %+v", got)
	}
}

// ---------- 重启对账 ----------

// 面板重启后 running 必须转 interrupted（设计 8.4）。
//
// 进程死了就是死了：那个 copy 到底复制了几个字节，只有上辈子的内存知道。
// 留成 running 会让抽屉永远显示"进行中"，用户等到天荒地老也不会看到结果；
// 而直接重跑又可能留下半份目标 —— 所以停在这里等用户点重试。
func TestReconcileJobsMarksRunningInterrupted(t *testing.T) {
	e := newJobEnv(t)
	run, _ := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpCopy, Src: []string{e.mk(t, "a.txt", "x")}, Dst: "/DISK/d",
	})
	claimed, err := e.svc.claimJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != run.ID {
		t.Fatal("夹具不对")
	}
	// 模拟重启：换一个 Service 实例，同一份数据库
	e2 := &jobEnv{db: e.db, dir: e.dir, clk: e.clk}
	e2.svc = NewService(Options{DB: e.db, Clock: e.clk.Now})
	n, err := e2.svc.ReconcileJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应有 1 条转 interrupted, got %d", n)
	}
	got, _ := e2.svc.GetJob(context.Background(), run.ID)
	if got.State != JobInterrupted {
		t.Errorf("重启后该是 interrupted, got %s", got.State)
	}
}

// 排队中且带取消意图的任务，重启时直接落 canceled —— 绝不能执行。
//
// 这是"取消意图必须落盘"的另一半：光存不用就等于没存。
func TestReconcileJobsCancelsQueuedCanceled(t *testing.T) {
	e := newJobEnv(t)
	j, _ := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "别动我.txt", "x")},
	})
	if err := e.svc.RequestCancelJob(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	e2 := NewService(Options{DB: e.db, Clock: e.clk.Now})
	if _, err := e2.ReconcileJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e2.GetJob(context.Background(), j.ID)
	if got.State != JobCanceled {
		t.Errorf("用户取消过的排队任务重启后该是 canceled, got %s", got.State)
	}
	if _, err := os.Stat(e.root("别动我.txt")); err != nil {
		t.Errorf("文件被动了: %v", err)
	}
}

// 普通排队任务重启后**保持 pending**（会继续跑），已完成/失败的也不动。
func TestReconcileJobsLeavesOthersAlone(t *testing.T) {
	e := newJobEnv(t)
	waiting, _ := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "排队.txt", "x")},
	})
	fin, _ := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{e.mk(t, "完了.txt", "x")},
	})
	if err := e.svc.finishJob(context.Background(), fin.ID, JobDone, ""); err != nil {
		t.Fatal(err)
	}
	e2 := NewService(Options{DB: e.db, Clock: e.clk.Now})
	n, err := e2.ReconcileJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("没有 running 时不该改任何东西, got %d", n)
	}
	if w, _ := e2.GetJob(context.Background(), waiting.ID); w.State != JobPending {
		t.Errorf("排队任务应保持 pending, got %s", w.State)
	}
	if f, _ := e2.GetJob(context.Background(), fin.ID); f.State != JobDone {
		t.Errorf("已完成不该被动, got %s", f.State)
	}
}

// ---------- 助手 ----------

func (e *jobEnv) root(rel string) string { return filepath.Join(e.dir, rel) }

func listAll(t *testing.T, e *jobEnv) []Job {
	t.Helper()
	jobs, err := e.svc.ListJobs(context.Background(), JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

// ---------- 迁移与约束 ----------

// fs_jobs 表存在，且 CHECK 约束真的在挡非法值。
//
// 约束必须在库里而不只在 Go 代码里：Go 侧的校验可以被任何一个绕过
// （手工 SQL、将来的第二个写入者、一次写错的迁移脚本），而队列的状态列
// 一旦出现第三种取值，worker 的"取 pending"就再也取不到它 —— 那条任务
// 会永远卡在抽屉里，既不执行也不结束，而且没有任何一处会报错。
func TestJobsSchemaAndConstraints(t *testing.T) {
	db := openStoreForJobs(t)
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='fs_jobs'`).Scan(&name)
	if err != nil {
		t.Fatalf("fs_jobs 表不存在: %v", err)
	}
	cases := []struct {
		what string
		sql  string
		ok   bool
	}{
		{"合法 delete 任务",
			`INSERT INTO fs_jobs(op,src,entries_total,state,created_at,updated_at)
			 VALUES('delete','["/a"]',1,'pending',1,1)`, true},
		{"未知 op 被拒",
			`INSERT INTO fs_jobs(op,src,entries_total,state,created_at,updated_at)
			 VALUES('chmod','["/a"]',1,'pending',1,1)`, false},
		{"未知 state 被拒",
			`INSERT INTO fs_jobs(op,src,entries_total,state,created_at,updated_at)
			 VALUES('delete','["/a"]',1,'paused',1,1)`, false},
		{"改状态成非法值也被拒",
			`UPDATE fs_jobs SET state='paused' WHERE op='delete'`, false},
	}
	for _, c := range cases {
		_, err := db.Exec(c.sql)
		if c.ok && err != nil {
			t.Errorf("%s 应成功: %v", c.what, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s 应被 CHECK 拒绝", c.what)
		}
	}
}

// 索引存在（抽屉每次打开都按 state 筛，且要跑在几万行上）。
func TestJobsStateIndexExists(t *testing.T) {
	db := openStoreForJobs(t)
	var n string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='fs_jobs' AND name='idx_fs_jobs_state'`).Scan(&n)
	if err != nil {
		t.Errorf("缺 state 索引: %v", err)
	}
}

func openStoreForJobs(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db.SqlDB()
}
