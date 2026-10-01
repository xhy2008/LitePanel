package main

// 后台文件任务池的装配（M6-T4 的最后一根线）。
//
// 这一段的失效形态值得单独写清楚，因为它是本次改动里最阴的一种：
// **队列全绿而生产是死的**。filemgr 那一侧几十个测试全过（每个夹具都自己
// 传 DB、自己调 StartJobs），而 buildDeps 里少写一个 `DB: db` 的后果是 ——
// POST /fs/delete 照常返回 job_id，界面照常显示"排队中"转圈，永远不会有
// 文件被动过，日志里一行错误都没有。
//
// 所以这里的测试一律不检查"字段是不是非 nil"，只检查**端到端**：从
// deps.Files 提交一条任务，看文件是不是真的在盘上被搬走了。那个断言同时
// 覆盖四件事：DB 传进去了、池起来了、起来的是处理器手里**那一个**实例、
// 分派对 op 有效。少任何一件都到不了 done。

import (
	"context"
	"time"

	"litepanel/internal/filemgr"
	"litepanel/internal/logx"
)

// jobPoolWaitTimeout 是关停时给在跑任务收尾的时间。
//
// 它不是"尽量等完"的意思：一个正在复制大镜像的任务不会在这点时间里跑完，
// 而 systemd 默认 90 秒后 SIGKILL。真等下去会让整台面板的关停被一个任务
// 拖死，托管的 nginx 也跟着多活一分半。等不到就取消池上下文，任务被落终态
// 成 interrupted（finalize 里那条"面板关闭"分支），用户在任务抽屉里看到
// "面板关闭，任务未完成"和一条可点重试的路 —— 比"状态停在 running、下次
// 开机才发现它其实没在跑"诚实。
const jobPoolWaitTimeout = 10 * time.Second

// fileJobsWiring 攥着任务池的取消函数：池是独立上下文（不挂请求），只有
// 装配层知道该在什么时候关掉它。
type fileJobsWiring struct {
	svc    *filemgr.Service
	cancel context.CancelFunc
}

// startFileJobs 起 Background 文件任务池，返回关停句柄。
//
// 返回值**永远**非 nil（没起起来时是一个 cancel 为空操作的空句柄），这样
// main 里可以无条件 defer Stop —— 漏一个 nil 判断就是关停时 panic，而
// panic 发生在信号处理后，表现是"面板关不掉"。
func startFileJobs(files any) *fileJobsWiring {
	svc, _ := files.(*filemgr.Service)
	noop := &fileJobsWiring{svc: svc, cancel: func() {}}
	if svc == nil {
		return noop
	}
	// 池用**独立**的上下文，绝不挂在请求上下文上：整套设计的立论就是
	// "提交任务的那个 HTTP request 会先死"（用户关掉浏览器）。
	ctx, cancel := context.WithCancel(context.Background())
	if !svc.StartJobs(ctx) {
		// 起不起来是装配问题（多半是没接数据库）。必须写进日志：漏接的
		// 当下毫无症状，运维会从磁盘、权限一路查下去，唯独不会想到池
		// 根本没开。
		cancel()
		logx.Error("后台文件任务池未启动：任务会停在排队中（检查数据库是否接上）")
		return noop
	}
	return &fileJobsWiring{svc: svc, cancel: cancel}
}

// Stop 取消池上下文并等在跑的任务落终态。
func (w *fileJobsWiring) Stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.cancel()
	if w.svc == nil {
		return
	}
	if err := w.svc.WaitJobs(ctx, jobPoolWaitTimeout); err != nil {
		logx.Info("等待后台任务收尾: %v", err)
	}
}
