package filemgr

// 任务状态变更的对外通知（WS fsjobs 的领域侧钩子）。
//
// 这里要钉的**不是**"钩子会被调到"，而是"**每一个**改动作物的写点都会
// 调到"。差别很关键：漏掉任何一个写点，表现都是前端任务抽屉停在旧状态
// 默默不再刷新，而任务本身跑得好好的 —— 编译通过、其余测试全绿、日志
// 干净，只有人盯着界面时才暴露。所以本测试走一遍完整生命周期，逐站断言
// 通知序列，而不是抽查几个点。
//
// 第二组断言是载荷的**大小上界**：任务行里存着 src 全量路径数组（一键
// 删除十万个文件时它可能有几 MB），而进度推送频率上限是每任务每 200ms
// 一次。把整行原样推进 WS 通道，等于在大删除任务进行时每秒往每个订阅
// 连接塞几十 MB。载荷必须是有界的，这条得钉住。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type notifyRec struct {
	mu   sync.Mutex
	got  []JobProgress
	file []string // 每条载荷的原始 JSON，用来验"没把 src 塞进去"
}

func (r *notifyRec) fn(p JobProgress) {
	b, _ := json.Marshal(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, p)
	r.file = append(r.file, string(b))
}

func (r *notifyRec) seq(id int64) []JobState {
	var out []JobState
	for _, p := range r.all() {
		if p.ID == id {
			out = append(out, p.State)
		}
	}
	return out
}

func (r *notifyRec) all() []JobProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]JobProgress(nil), r.got...)
}

func (r *notifyRec) raw() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.file, "\n")
}

// 新夹具：带通知钩子的 job 环境。
func newNotifyEnv(t *testing.T) (*jobEnv2, *notifyRec) {
	t.Helper()
	e := newJobEnv2(t)
	r := &notifyRec{}
	e.svc.jobNotifier = r.fn
	return e, r
}

// 不起池跑一遍生命周期：建 → 取消 → 排队中被领走 → 完成。
//
// 分成"不起池"和"起池"两条是**被迫的**，而且这个被迫本身值得记：notifyJob
// 的口径是"回读整行再推"，所以推送内容定义上等于"推送那一刻的库状态"。
// 起了池之后，worker 完全可能在建任务的下一微秒就把状态改成 running，
// 于是那一次推送带回来的就是 running —— 序列里不会出现 pending。这不是
// bug：界面看到的永远是**更新**的状态而不是更旧的，而"绝不留下陈旧状态"
// 才是这套机制要保证的（提交者本来也从 POST 响应里拿到了 pending 那条）。
// 要逐站核对序列，就得让任务在每一站停住，也就是不起池。
func TestNotifyCoversWholeLifecycle(t *testing.T) {
	e, r := newNotifyEnv(t)
	a := e.mkFile(t, "fast", "n1.txt")
	e.svc.StartJobs(mustPoolCtx(t))
	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{a}})
	if err != nil {
		t.Fatal(err)
	}
	// 等的是**通知**而不是库状态。这个区别不是吹毛求疵：通知是在写库
	// **之后**才推的（finishJob 的 defer），所以 waitState 一返回就去读
	// r.got 会偶发读到"库已 done、通知还没推出去"，测试随机红。
	// 等通知才对：漏推 done 时它会超时，而"库里已终态但界面上永远是
	// 进行中"正是这套机制要防的那件事本身。
	waitNotify(t, r, j.ID, JobDone, 5*time.Second)

	got := r.seq(j.ID)
	// 终态必到，且中间不许出现"往回走"的状态。
	if len(got) == 0 || got[len(got)-1] != JobDone {
		t.Errorf("最后一条通知该是 done: %v", got)
	}
	for i := 1; i < len(got); i++ {
		if stateRank(got[i]) < stateRank(got[i-1]) {
			t.Errorf("通知状态倒退了: %v", got)
		}
	}
	if !stateSubsequence(got, JobRunning, JobDone) {
		t.Errorf("通知序列缺站: %v", got)
	}
	// done 那一条必须带最终条目数：抽屉靠它把进度条收尾，
	// 若只推状态不推数字，界面会显示"已完成 0/3"。
	var last *JobProgress
	for i := range r.got {
		if r.got[i].ID == j.ID && r.got[i].State == JobDone {
			last = &r.got[i]
		}
	}
	if last == nil || last.EntriesDone != 1 {
		t.Errorf("done 通知该带最终进度, got %+v", last)
	}
}

// 建任务必须自己推一次 pending。
//
// 不起池是这条测试的全部要点：起了池，worker 会在下一微秒把状态改成
// running，于是"create 有没有推"从外面完全看不出来（先前那条生命周期
// 测试就是这样把 CreateJob 不推这个变异放过去的 —— 它只等 running→done,
// 而这两站队列自己就会推）。这里池压根不起，pending 只可能来自建任务。
func TestNotifyCreatePushesPending(t *testing.T) {
	e, r := newNotifyEnv(t) // 不起池
	a := e.mkFile(t, "fast", "n5.txt")
	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{a}})
	if err != nil {
		t.Fatal(err)
	}
	waitNotify(t, r, j.ID, JobPending, 2*time.Second)
}

// 领取任务必须自己推 running。
//
// 同样不起池、直接调那个写点：领走一条任务没有任何 HTTP 调用方参与，
// 全靠队列自己推，是七处写点里最容易被漏掉的一类。
func TestNotifyClaimPushesRunning(t *testing.T) {
	e, r := newNotifyEnv(t)
	a := e.mkFile(t, "fast", "n6.txt")
	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{a}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.claimJob(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitNotify(t, r, j.ID, JobRunning, 2*time.Second)
}

// 进度落库必须自己推。直接调写点、直接断言推进去的数字：
// 走完整任务做不到这一点 —— 快速任务的全部进度会被节流合并成最后一次
// 写，而那一次的数字恰好也会被 finishJob 的推送顺带带出来（推送是回读
// 整行的），所以"进度写不推"在端到端测试里是不可见的。
func TestNotifyProgressWritePushed(t *testing.T) {
	e, r := newNotifyEnv(t)
	a := e.mkFile(t, "fast", "n7.txt")
	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpCopy, Src: []string{a}, Dst: e.slow})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.setJobProgress(context.Background(), j.ID, 4096, 2); err != nil {
		t.Fatal(err)
	}
	waitNotifyMatch(t, r, func(p JobProgress) bool {
		return p.ID == j.ID && p.DoneBytes == 4096 && p.EntriesDone == 2
	}, 2*time.Second)
}

// 排队中点取消：意图落库的那一刻也要推，否则抽屉上"取消中…"永远不出现
// （worker 可能还要几秒才轮到这条，中间界面毫无反馈）。
func TestNotifyCancelIntentPushed(t *testing.T) {
	e, r := newNotifyEnv(t) // 不起池：任务停在 pending
	a := e.mkFile(t, "fast", "n2.txt")
	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{a}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RequestCancelJob(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range r.all() {
		if p.ID == j.ID && p.CancelRequested {
			return
		}
	}
	t.Errorf("取消意图没推出去: %s", r.raw())
}

// 重启对账把 running 改写成 interrupted：这批是**批量 UPDATE**，没有现成的
// id 列表，最容易写成"改了库忘了推"。而它偏偏是最需要推的一站 —— 用户
// 重启后打开抽屉，旧任务应当立刻变成"已中断（可重试）"。
func TestNotifyReconcilePushesInterrupted(t *testing.T) {
	e, r := newNotifyEnv(t)
	a := e.mkFile(t, "fast", "n3.txt")
	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: []string{a}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.SqlDB().Exec(`UPDATE fs_jobs SET state='running' WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReconcileJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stateSubsequence(r.seq(j.ID), JobInterrupted) {
		t.Errorf("对账改写后没推 interrupted: %s", r.raw())
	}
}

// 重试推的是 pending **且 resumed=true**。
//
// 只等 pending 是不够的：建任务时已经推过一次 pending 了，那条在这儿
// 一直躺着（实测就是这样让"重试不推"的变异活下来的）。resumed 只有重试
// 会置，所以它才是这条推送的指纹。
func TestNotifyRetryPushed(t *testing.T) {
	e, r := newNotifyEnv(t)
	j := e.mustInterrupted(t, JobInput{Op: OpDelete, Src: []string{"/whatever"}})
	if _, err := e.svc.RetryJob(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	waitNotifyMatch(t, r, func(p JobProgress) bool {
		return p.ID == j.ID && p.State == JobPending && p.Resumed
	}, 2*time.Second)
}

// 载荷必须与 GET /fs/jobs 的行同名字段。
//
// 前端要做的是"收到推送就把列表里那条覆盖掉"，字段名一旦有两套，抽屉
// 就会有一半字段永远不更新（这种 bug 只在界面上看得见）。
func TestNotifyFieldNamesMatchJobJSON(t *testing.T) {
	var a, b map[string]any
	mustUnmarshal(t, Job{ID: 1, State: JobDone}, &a)
	mustUnmarshal(t, JobProgress{ID: 1, State: JobDone}, &b)
	for k := range b {
		if _, ok := a[k]; !ok {
			t.Errorf("JobProgress 多出 Job 没有的字段 %q（两套口径）", k)
		}
	}
}

// 载荷大小有界：src 全量路径不得进推送。
func TestNotifyPayloadCarriesNoSourceList(t *testing.T) {
	e, r := newNotifyEnv(t)
	var paths []string
	for i := 0; i < 500; i++ {
		// 文件名必须真的互不相同：建任务会对 src 去重（见 normalized），
		// 造出重名会让 entries_total 远小于 500，那时下面那条
		// "entries_total=500" 就是在测一个根本没被满足的前提。
		name := fmt.Sprintf("f%04d%s.txt", i, strings.Repeat("p", 40))
		p := filepath.Join(e.fast, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	e.svc.StartJobs(mustPoolCtx(t))
	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpDelete, Src: paths})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, e.svc, j.ID, JobDone, 20*time.Second)
	waitNotify(t, r, j.ID, JobDone, 5*time.Second)
	// 条目数必须推（界面靠它显示 500 项），但路径本身一条都不许出现。
	// 注意别在持有 r.mu 的时候调 r.raw()：notify 回调也要那把锁，
	// 自己等自己是死锁（先前就这么把整包测试卡到超时的）。
	if n := strings.Count(r.raw(), ".txt"); n != 0 {
		t.Errorf("推送里带了源路径（载荷无上界）: 命中 %d 次", n)
	}
	found := false
	for _, p := range r.all() {
		if p.ID == j.ID && p.EntriesTotal == 500 {
			found = true
		}
	}
	if !found {
		t.Error("该推 entries_total=500")
	}
}

// waitNotifyMatch 等一条满足条件的推送。
func waitNotifyMatch(t testing.TB, r *notifyRec, ok func(JobProgress) bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, p := range r.all() {
			if ok(p) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%dms 内没等到期望的推送", d.Milliseconds())
}

// waitNotify 等某条任务的某个状态被推出来。
func waitNotify(t testing.TB, r *notifyRec, id int64, want JobState, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, p := range r.all() {
			if p.ID == id && p.State == want {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%dms 内没收到任务 %d 的 %s 通知（已收到: %v）",
		d.Milliseconds(), id, want, r.seq(id))
}

// stateRank 给任务状态一个"只会往前走"的序，用来验通知不倒退。
//
// 倒退是真 bug 的形状：notifyJob 是"回读再推"，两个写点并发时，先写的那
// 个可能后推（被抢占），界面就会看到 done 之后又跳回 running。当前实现
// 里没有跨写点的并发推送（每个任务只有一个 worker，取消意图那一路改的是
// 未领走的任务），但这条序一旦哪天被破坏，界面会当场抽风。
func stateRank(s JobState) int {
	switch s {
	case JobPending:
		return 0
	case JobRunning:
		return 1
	default: // done/failed/canceled/interrupted 互斥，同层
		return 2
	}
}

func mustUnmarshal(t *testing.T, v any, out *map[string]any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
}

// stateSubsequence 判 want 是否按序（可不连续）出现在 got 里。
func stateSubsequence(got []JobState, want ...JobState) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
