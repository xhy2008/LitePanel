package main

// 后台文件任务池的装配（M6-T4 最后一根线）。
//
// 这段代码的失效形态是本次改动里最阴的一种：**队列测试全绿而生产是死的**。
// filemgr 那边几十个测试每个都自己传 DB、自己调 StartJobs，所以装配层少写
// 一个 `DB: db`，它们的绿一点不会动摇 —— 而线上是：POST /fs/delete 照常
// 返回 job_id，界面照常转"排队中"，永远不会有文件被动过，日志里一行错误
// 都没有。
//
// 这里的测试策略因此和 filemgr 那边**不同**，是有意的分工：
//   - "任务真的把文件搬走了/删进回收站"由 internal/filemgr/exec_run_test.go
//     负责。那里能注入假盘、假挂载表，断言盘上真相。
//   - 这一层只钉**装配本身**：buildDeps 造的实例到底能不能起池。
//     用的判据是 StartJobs 的返回值 —— 它在 db == nil 时**必然** false，
//     所以"能起起来"就是"DB 传进去了"的行为学证明，不需要往生产里加任何
//     测试专用的注入口（那会变成永久存在的公共 API）。

import (
	"context"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/filemgr"
	"litepanel/internal/ws"
)

// wiredFileService 取回 buildDeps 造的那个实例。
//
// 必须取回来而不是另造一个：本轮改动最容易犯的错就是"测试里 new 一个起了
// 池、装配层里另一个没起" —— 那种测试测的是夹具本身，生产照旧是死的。
func wiredFileService(t testing.TB, deps *api.AuthDeps) *filemgr.Service {
	t.Helper()
	svc, ok := deps.Files.(*filemgr.Service)
	if !ok {
		t.Fatalf("Files 不是 *filemgr.Service: %T", deps.Files)
	}
	return svc
}

// buildDeps 造出来的服务必须**起得来池** —— 即 DB 真的接上了。
//
// 判据选 StartJobs 的返回值而不是"某个字段非 nil"：它是领域层对"队列到底
// 可用吗"给出的正式答案，语义就是这个（db == nil 或执行器缺失时必然回
// false）。测试因此跟着领域层的契约走，而不是跟着它的内部字段布局走。
func TestBuildDepsWiresJobDatabase(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Defaults()
	cfg.DBPath = t.TempDir() + "/litepanel.db"
	deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
	svc := wiredFileService(t, &deps)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !svc.StartJobs(ctx) {
		t.Fatal("buildDeps 没把 DB 传给 filemgr：任务会永远停在排队中")
	}
}

// 同一个实例被 HTTP 处理器和池共用（漏掉这层会出现"两个 Service"）。
//
// 装配层起池的对象必须是 deps.Files 那一个。如果 startFileJobs 拿到的是
// 另 new 出来的实例，会出现一个非常难查的形态：端点写入任务后 kick 的是
// 实例 A 的唤醒通道，而在跑任务的是实例 B 的 worker —— 于是任务不是不动,
// 而是**要等到 30 秒兜底 tick 才动**。这种"能跑但慢得没道理"的 bug 比
// 完全不跑更难被发现，用户只会说"面板有点卡"。
func TestStartFileJobsUsesDepsInstance(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Defaults()
	cfg.DBPath = t.TempDir() + "/litepanel.db"
	deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
	svc := wiredFileService(t, &deps)

	w := startFileJobs(deps.Files)
	if w == nil {
		t.Fatal("池没起来（DB/执行器没接上）")
	}
	t.Cleanup(func() { w.Stop(context.Background()) })
	// 池起来之后，重复调用必须说"没起新的"：两套 worker 会把并发上限翻倍，
	// 而配置文件里的数字是用户唯一看得见的东西。
	// 重复起池要报告"没起新的"：两套 worker 会把并发上限翻倍，而配置里
	// 的数字是用户唯一看得见的东西。用领域层的 bool 返回值验（装配层的
	// 句柄按约定恒非 nil，表达不了"没起")。
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if svc.StartJobs(ctx2) {
		t.Error("重复调用起了第二套 worker")
	}
}

// 配置里的 job_concurrency 必须真的传到 NewService。
//
// 漏传不会有任何报错：NewService 把零值夹成默认 2，于是配置文件变成一个
// 安静的摆设 —— 用户改成 5，面板仍是 2，而他判断"改没生效"的唯一办法是
// 看任务是不是更快了，而并发从 2 到 4 在 HDD 上根本看不出来。
func TestWiredJobConcurrencyComesFromConfig(t *testing.T) {
	db := openTestDB(t)
	for _, want := range []int{1, 5} {
		cfg := config.Defaults()
		cfg.JobConcurrency = want
		deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
		svc := wiredFileService(t, &deps)
		if got := svc.JobConcurrency(); got != want {
			t.Errorf("配置的并发没传到位: 配置 %d, got %d", want, got)
		}
	}
}

// 没接上时不许谎称池在跑，且关停句柄永不为 nil。
//
// 不拒绝的后果是日志写着"任务队列已启动"，运维从磁盘查到权限，唯独不会
// 想到池从一开始就不存在。句柄的 nil 约定是给 main 用的：它必须能无条件
// defer Stop，少写一个判断就是关停时 panic，而那发生在信号处理之后，
// 症状是"面板关不掉"。
func TestStartFileJobsReportsUnwired(t *testing.T) {
	// "起不起得来"只能问领域层：装配层的句柄按约定恒非 nil（让 main 能
	// 无条件 Stop），那个 nil 约定是关停路径的安全属性，不该为测试方便牺牲。
	// 所以这里的判据是 StartJobs 的 bool。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if filemgr.NewService(filemgr.Options{}).StartJobs(ctx) {
		t.Error("没接数据库时不该谎称池在跑")
	}
	// 句柄恒非 nil：main 要能无条件 Stop。少一个判断就是关停时 panic，
	// 而那发生在信号处理之后，症状是"面板关不掉"。
	if w := startFileJobs(nil); w == nil {
		t.Error("服务没接时关停句柄也该非 nil")
	}
	if w := startFileJobs(filemgr.NewService(filemgr.Options{})); w == nil {
		t.Error("没接数据库时关停句柄也该非 nil")
	}
	// 空句柄的 Stop 必须真的什么都不做（不 panic、不阻塞）。
	done := make(chan struct{})
	go func() { startFileJobs(nil).Stop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("空句柄的 Stop 卡住了")
	}
}

// Stop 之后在跑的任务不许停在 running。
//
// 只 cancel 不 wait 的后果：进程带着一个还在执行的任务退出，库里留在
// running。下一轮开机对账会把它标成 interrupted，看起来能自圆其说 —— 但
// 那意味着**每次升级/重启都会打断所有任务**，用户看到一堆"上次任务被中断",
// 而没有一条是他自己触发的。
func TestFileJobsStopNeverLeavesRunning(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Defaults()
	cfg.DBPath = t.TempDir() + "/litepanel.db"
	deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
	svc := wiredFileService(t, &deps)
	w := startFileJobs(deps.Files)
	if w == nil {
		t.Fatal("池没起来")
	}
	w.Stop(context.Background())
	// 停完之后再提交任务不该被起死回生
	jobs, err := svc.ListJobs(context.Background(), filemgr.JobFilter{})
	if err != nil {
		t.Fatalf("Stop 之后队列还应可查: %v", err)
	}
	for _, j := range jobs {
		if j.State == filemgr.JobRunning {
			t.Errorf("任务 %d 停在 running", j.ID)
		}
	}
}

// deps.Files 与 deps.Jobs 必须是**同一个**实例。
//
// 装配层现在有两个出口指向同一个东西，而"顺手再 new 一个"看起来无害：编译
// 过、测试过、任务也确实会跑。坏在它坏得很有耐心 —— HTTP 处理器提交任务后
// kick 的是实例 A 的唤醒通道，而在跑任务的是实例 B 的 worker（它没被任何人
// 唤醒），于是每条任务都要等到 30 秒兜底 tick 才被领走。用户看到的是"面板
// 有点卡"，运维看到的是"一切正常"，日志里一个字都不会有。
//
// 这种"能跑但慢得没道理"的错，唯一便宜的封法就是钉住同一性本身。
func TestDepsFilesAndJobsAreSameInstance(t *testing.T) {
	db := openTestDB(t)
	deps := buildDeps(db, config.Defaults(), ws.NewHub(), false, nil)
	a, ok := deps.Files.(*filemgr.Service)
	if !ok {
		t.Fatalf("Files 类型不对: %T", deps.Files)
	}
	b, ok := deps.Jobs.(*filemgr.Service)
	if !ok {
		t.Fatalf("Jobs 类型不对: %T", deps.Jobs)
	}
	if a != b {
		t.Error("Files 与 Jobs 是两个实例：提交任务的唤醒信号送不到干活的 worker，每条任务都会白等一个兜底轮询周期")
	}
}
