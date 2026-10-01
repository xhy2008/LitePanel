package filemgr

// runJob 分派 + 端到端任务流。
//
// 这个文件测的是"提交 → 后台跑完"这条**完整链路**，而不是任何单个部件。
// 它是 M6-T4 存在的理由本身（设计第 18 行、验收项 500）：复制/移动/删除
// 不因浏览器关闭而中断。前面所有测试都只证明"部件正确"，只有这里能证明
// 接线是对的 —— 接线错的方式很具体：executor 没接、op 分派写漏一支、
// permanent 标志没落库、Job 的 src 解出来是空的……每一个都足以让用户
// 看到"排队中"转到天荒地老，而单元测试全绿。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"litepanel/internal/store"
)

// jobEnv2：带库、带两块逻辑盘、带回收站的服务（跑真执行器）。
type jobEnv2 struct {
	svc    *Service
	db     *store.DB
	fast   string
	slow   string
	clk    *fakeClock
	sameFS bool
}

func newJobEnv2(t testing.TB) *jobEnv2 {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(base, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &jobEnv2{
		db:   db,
		fast: filepath.Join(base, "fast"),
		slow: filepath.Join(base, "slow"),
		clk:  &fakeClock{now: fakeNow},
	}
	for _, d := range []string{e.fast, e.slow} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.svc = NewService(Options{
		DB:    db,
		Clock: e.clk.Now,
		// 默认并发 1：端到端测试里"一条一条跑完"才是可观察的。
		JobConcurrency:  1,
		JobWakeInterval: time.Hour,
		FilesystemRoot: func(path string) (string, error) {
			for _, r := range []string{e.fast, e.slow} {
				if path == r || strings.HasPrefix(path, r+string(os.PathSeparator)) {
					return r, nil
				}
			}
			return "", errors.New("不在假挂载表里")
		},
		TrashRoots: func(context.Context) ([]string, error) {
			return []string{e.fast, e.slow}, nil
		},
		SameFS: func(a, b string) (bool, error) {
			ra, err1 := e.svc.fsRoot(a)
			rb, err2 := e.svc.fsRoot(b)
			if err1 != nil {
				return false, err1
			}
			if err2 != nil {
				return false, err2
			}
			if ra == rb {
				return true, nil
			}
			return e.sameFS, nil
		},
	})
	return e
}

// ---------- 提交后关掉浏览器 ----------

// 端到端：delete 任务提交后，**请求上下文消失**也必须照常跑完。
//
// 这就是 M6-T4 的全部意义。同步端点做不到这一点：r.Context() 会随关标签页、
// 锁屏、代理超时取消，删除就地停住（实测中断时原地剩 2999/3000，而前端只
// 看到一个网络错误）。这里模拟的正是那个时刻：CreateJob 用的 ctx 被取消
// （= 用户关掉了页面），任务必须仍然由 worker 跑到底。
func TestDeleteJobSurvivesRequestCancel(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = true
	var paths []string
	for i := 0; i < 20; i++ {
		paths = append(paths, e.mkFile(t, "fast", "f"+itoa(i)))
	}

	// 请求上下文：CreateJob 用它，随后取消，模拟"提交完就关页面"。
	reqCtx, cancelReq := context.WithCancel(context.Background())
	j, err := e.svc.CreateJob(reqCtx, JobInput{Op: OpDelete, Src: paths})
	if err != nil {
		t.Fatal(err)
	}
	// 池用的是自己的上下文（不是请求上下文）—— 这是整套设计的核心。
	poolCtx, cancelPool := context.WithCancel(context.Background())
	defer cancelPool()
	if !e.svc.StartJobs(poolCtx) {
		t.Fatal("池没接上")
	}
	cancelReq() // ← 浏览器关掉了

	waitState(t, e.svc, j.ID, JobDone, 5*time.Second)
	for _, p := range paths {
		// 进了回收站：原位必须没有了
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s 还在原地（请求上下文一取消就停了？）", p)
		}
	}
	got, _ := e.svc.GetJob(context.Background(), j.ID)
	if got.EntriesDone != len(paths) {
		t.Errorf("进度该记满 %d 条, got %d", len(paths), got.EntriesDone)
	}
}

// 端到端：copy 任务在请求消失后仍然把文件复制到目标盘。
func TestCopyJobSurvivesRequestCancel(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = false
	src := filepath.Join(e.fast, "a.bin")
	writeFileN(t, src, 1<<20)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	j, err := e.svc.CreateJob(reqCtx, JobInput{
		Op: OpCopy, Src: []string{src}, Dst: e.slow,
	})
	if err != nil {
		t.Fatal(err)
	}
	poolCtx, cancelPool := context.WithCancel(context.Background())
	defer cancelPool()
	e.svc.StartJobs(poolCtx)
	cancelReq()

	waitState(t, e.svc, j.ID, JobDone, 10*time.Second)
	if hashFile(t, filepath.Join(e.slow, "a.bin")) != hashFile(t, src) {
		t.Error("副本内容不对")
	}
	// copy 绝不能动源
	if _, err := os.Stat(src); err != nil {
		t.Errorf("copy 把源弄丢了: %v", err)
	}
}

// 端到端：move 任务在请求消失后仍然完成（同盘 rename）。
func TestMoveJobCompletesDespiteRequestCancel(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = true
	src := filepath.Join(e.fast, "m.txt")
	if err := os.WriteFile(src, []byte("搬走我"), 0o644); err != nil {
		t.Fatal(err)
	}
	dstDir := filepath.Join(e.fast, "here")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reqCtx, cancelReq := context.WithCancel(context.Background())
	j, err := e.svc.CreateJob(reqCtx, JobInput{Op: OpMove, Src: []string{src}, Dst: dstDir})
	if err != nil {
		t.Fatal(err)
	}
	poolCtx, cancelPool := context.WithCancel(context.Background())
	defer cancelPool()
	e.svc.StartJobs(poolCtx)
	cancelReq()

	waitState(t, e.svc, j.ID, JobDone, 5*time.Second)
	if _, err := os.Stat(filepath.Join(dstDir, "m.txt")); err != nil {
		t.Errorf("没搬到目标: %v", err)
	}
}

// ---------- op 分派 ----------

// 三种 op 都必须真的被分派到对应的执行体。
//
// 分派表写漏一支是最容易犯又最难被发现的一类错：那条 op 的任务会一路
// pending → running → failed("未支持的任务类型")，用户看到一条谁也看不懂
// 的红色记录，而单元测试（直接调 copyFile/movePath）全绿。
func TestRunJobDispatchesAllOps(t *testing.T) {
	t.Run("copy", func(t *testing.T) {
		e := newJobEnv2(t)
		e.sameFS = false
		src := filepath.Join(e.fast, "c.bin")
		writeFileN(t, src, 4096)
		e.svc.StartJobs(mustPoolCtx(t))
		j, err := e.svc.CreateJob(context.Background(), JobInput{
			Op: OpCopy, Src: []string{src}, Dst: e.slow,
		})
		if err != nil {
			t.Fatal(err)
		}
		got := waitState(t, e.svc, j.ID, JobDone, 5*time.Second)
		if got.DoneBytes != 4096 {
			t.Errorf("copy 该报 4096 字节, got %d", got.DoneBytes)
		}
	})
	t.Run("move", func(t *testing.T) {
		e := newJobEnv2(t)
		e.sameFS = true
		src := filepath.Join(e.fast, "mv.txt")
		if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		e.svc.StartJobs(mustPoolCtx(t))
		j, _ := e.svc.CreateJob(context.Background(), JobInput{
			Op: OpMove, Src: []string{src}, Dst: e.slow,
		})
		waitState(t, e.svc, j.ID, JobDone, 5*time.Second)
		if _, err := os.Stat(filepath.Join(e.slow, "mv.txt")); err != nil {
			t.Errorf("move 没落地: %v", err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		e := newJobEnv2(t)
		p := e.mkFile(t, "fast", "d.txt")
		e.svc.StartJobs(mustPoolCtx(t))
		j, _ := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{p}})
		waitState(t, e.svc, j.ID, JobDone, 5*time.Second)
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Error("delete 没生效")
		}
	})
}

// delete 任务默认进回收站（可还原），不是直接抹掉。
//
// 判据不是"文件不见了"——永久删除也会让它不见。判据是回收站里**有这一条**
// 且能还原回原路径。写错成永久删除的话，用户点一次删除就再也没有后悔机会，
// 而这个差别只有在他误删之后才会被发现。
func TestDeleteJobGoesToTrashByDefault(t *testing.T) {
	e := newJobEnv2(t)
	p := e.mkFile(t, "fast", "keep-me.txt")
	e.svc.StartJobs(mustPoolCtx(t))
	j, _ := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{p}})
	waitState(t, e.svc, j.ID, JobDone, 5*time.Second)

	items, err := e.svc.ListTrash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("回收站该有 1 条, got %d", len(items))
	}
	if _, err := e.svc.RestoreTrash(context.Background(), items[0].ID); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(p); err != nil || string(got) != "keep-me.txt" {
		t.Errorf("还原失败: %q %v", got, err)
	}
}

// permanent 任务必须**真的**不落回收站（不可撤销，界面上已经二次确认过）。
//
// 方向反过来也要成立：permanent=0 绝不能顺手永久删（上面那条测的就是这个
// 方向）。两条凑齐才说明这个布尔被真的传到了执行体，而不是"参数在链路某处
// 被吞掉、恰好走了默认分支"。
func TestPermanentDeleteJobSkipsTrash(t *testing.T) {
	e := newJobEnv2(t)
	p := e.mkFile(t, "fast", "gone.txt")
	e.svc.StartJobs(mustPoolCtx(t))
	j, err := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{p}, Permanent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, e.svc, j.ID, JobDone, 5*time.Second)
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Error("永久删除没生效")
	}
	items, err := e.svc.ListTrash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("permanent 删除不该进回收站, got %d 条", len(items))
	}
}

// permanent 标志必须**落库**（重启后仍知道这是永久删除）。
//
// 与 cancel_requested 同一条理由：只放在内存/函数参数里的话，任务排在队里
// 时面板崩掉，重启后队列会按默认分支（进回收站）执行 —— 用户明确选了"不可
// 撤销地删除"，重启却偷偷给了他一个可还原的删除，两边都不对。
func TestPermanentFlagPersistsAcrossReopen(t *testing.T) {
	e := newJobEnv2(t)
	p := e.mkFileSync(t, "fast", "perm.txt")
	j, err := e.svc.CreateJob(context.Background(), JobInput{
		Op: OpDelete, Src: []string{p}, Permanent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 重新装配一个 Service（模拟进程重启，读同一份库）
	again := e.reassemble(t)
	got, err := again.GetJob(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Permanent {
		t.Error("permanent 没落库：重启后这条永久删除会被当成普通删除执行")
	}
}

// ---------- 进度与取消 ----------

// delete 任务的进度要按**条**推进。
//
// 界面在"删除 5000 个文件"时需要"已删 1200/5000"，只有终态没有过程的话，
// 用户面对一个几分钟不动的进度条只会以为面板挂了。
func TestDeleteJobReportsPerItemProgress(t *testing.T) {
	e := newJobEnv2(t)
	var paths []string
	for i := 0; i < 12; i++ {
		paths = append(paths, e.mkFile(t, "fast", "p"+itoa(i)))
	}
	var mu sync.Mutex
	var seen []int
	e.svc.progressObserver = func() {}
	e.svc.executor = func(ctx context.Context, j Job, report progressFunc) error {
		for i := 1; i <= len(j.Src); i++ {
			if err := report(0, i); err != nil {
				return err
			}
			mu.Lock()
			seen = append(seen, i)
			mu.Unlock()
		}
		return nil
	}
	e.svc.StartJobs(mustPoolCtx(t))
	j, _ := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: paths})
	waitState(t, e.svc, j.ID, JobDone, 5*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("批量删除只上报了 %d 次进度", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("条目进度没单调推进: %v", seen)
		}
	}
}

// 排队中的 delete 任务可以被取消，且一个文件都不许动。
func TestDeleteJobCancelBeforeStartTouchesNothing(t *testing.T) {
	e := newJobEnv2(t)
	e.svc.wakeInterval = time.Hour
	var paths []string
	for i := 0; i < 5; i++ {
		paths = append(paths, e.mkFile(t, "fast", "k"+itoa(i)))
	}
	// 先用诱饵占住唯一的 worker（gate 不开）
	decoy := e.mkFileSync(t, "fast", "decoy.txt")
	dj, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{decoy}})
	if err != nil {
		t.Fatal(err)
	}
	var release = make(chan struct{})
	e.svc.executor = func(ctx context.Context, j Job, report progressFunc) error {
		if j.ID == dj.ID {
			<-release
		}
		return nil
	}
	e.svc.StartJobs(mustPoolCtx(t))
	waitState(t, e.svc, dj.ID, JobRunning, 5*time.Second)

	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: paths})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RequestCancelJob(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	got := waitState(t, e.svc, j.ID, JobCanceled, 5*time.Second)
	if got.State != JobCanceled {
		t.Fatalf("got %s", got.State)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("被取消的任务动了文件: %v", err)
		}
	}
}

// 失败的任务要把原因写进抽屉（这里是盘根不可写 → 回收站建不起来）。
//
// ErrTrashUnwritable 是同步路径上专门设计的哨兵（422 + "可以选永久删除"）。
// 走队列之后它变成了一个 error 字符串，用户仍然要能看出**为什么**失败并有
// 下一步可走；被压成"任务失败"四个字的错误在批量删除场景里等于让人重头试。
func TestDeleteJobFailureCarriesReason(t *testing.T) {
	e := newJobEnv2(t)
	p := e.mkFile(t, "fast", "x.txt")
	// 让 fast 盘的回收站建不起来：把它换成一个**普通文件**占了目录名
	trash := filepath.Join(e.fast, e.svc.TrashDirName())
	if err := os.WriteFile(trash, []byte("占了回收站的名字"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.svc.StartJobs(mustPoolCtx(t))
	j, _ := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{p}})
	got := waitState(t, e.svc, j.ID, JobFailed, 5*time.Second)
	if !strings.Contains(got.Error, "回收站") {
		t.Errorf("失败原因要说清是回收站的问题, got %q", got.Error)
	}
	// 源必须完好：失败的删除不能把文件弄丢
	if _, err := os.Stat(p); err != nil {
		t.Errorf("删除失败却把文件弄丢了: %v", err)
	}
}

// ---------- 助手 ----------

func (e *jobEnv2) mkFile(t testing.TB, disk, name string) string {
	t.Helper()
	return e.mkFileSync(t, disk, name)
}

func (e *jobEnv2) mkFileSync(t testing.TB, disk, name string) string {
	t.Helper()
	p := filepath.Join(e.fast, name)
	if disk == "slow" {
		p = filepath.Join(e.slow, name)
	}
	if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// reassemble 用同一份库另建一个 Service（模拟进程重启）。
func (e *jobEnv2) reassemble(t testing.TB) *Service {
	t.Helper()
	return NewService(Options{
		DB:    e.db,
		Clock: e.clk.Now,
		FilesystemRoot: func(path string) (string, error) {
			for _, r := range []string{e.fast, e.slow} {
				if path == r || strings.HasPrefix(path, r+string(os.PathSeparator)) {
					return r, nil
				}
			}
			return "", errors.New("不在假挂载表里")
		},
		TrashRoots: func(context.Context) ([]string, error) {
			return []string{e.fast, e.slow}, nil
		},
	})
}

func mustPoolCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
