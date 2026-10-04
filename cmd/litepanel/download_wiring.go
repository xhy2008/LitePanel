package main

// aria2 下载模块的装配（M7-T3 的最后一根线）。
//
// 与 jobs_wiring.go 同一条教训：**领域层全绿而生产是死的**。download 包里
// 上百个测试每个都自带夹具，buildDeps 里少写一行 `Downloads:` 的后果是
// 下载页整片 501，而用户眼里那就是"面板坏了"；接了 Downloads 但忘了起
// 事件桥/轮询器的后果更阴 —— 列表能出来、能提交任务，但进度永远不动。
//
// 所以这里的测试一律不检查"字段是否非 nil"，只检查端到端：起一个真 aria2
// （dev 机上装有 aria2c 1.37.0），从 deps.Downloads 提交一个真下载，看
// 文件是不是真的落盘了。那一条同时覆盖：RPC 地址传进去了、事件桥起来了、
// 轮询器起来了、进度回调接到了 Service、状态变化推到了 downloads 频道。
//
// 三个后台 goroutine（事件桥、轮询器、对账）都用**独立**上下文：它们的生命
// 周期属于进程而不属于任何请求，关停时由 wiring 显式收（同 fileJobs）。

import (
	"context"
	"net/url"
	"strings"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/download"
	"litepanel/internal/logx"
	"litepanel/internal/store"
	"litepanel/internal/ws"
)

// downloadPollInterval 是进度轮询间隔（有活动任务时才跑，空闲零请求）。
const downloadPollInterval = time.Second

// downloadWiring 攥着两个后台循环的取消函数与 Service 本体。
type downloadWiring struct {
	svc    *download.Service
	cancel context.CancelFunc
}

// Stop 停掉事件桥与轮询器。幂等。
func (w *downloadWiring) Stop() {
	if w == nil || w.cancel == nil {
		return
	}
	w.cancel()
	w.cancel = nil
}

// newDownloadService 装配 aria2 客户端 + 历史表 + 健康检查成一个 Service。
//
// 事件广播的接法：事件桥与轮询器的 notifier 都汇到一个函数，它做两件事：
//  1. 把终态写进本地表（aria2 跨重启不留终态记录，见 tasks.go 头注）；
//  2. 原样推给前端（downloads 频道）。
//
// 顺序必须是先落库后推送：前端收到事件后会立刻拉一次 GET /dl/tasks，若推送
// 先到而落库后到，那次拉取读到的还是旧状态 —— 界面闪一下"下载中"再变"完成"，
// 用户会以为面板在抽风。
func newDownloadService(db *store.DB, cfg config.Config, hub *ws.Hub) *download.Service {
	client, err := download.NewClient(cfg.Aria2RPCURL, cfg.Aria2RPCSecret)
	if err != nil {
		// 不能 panic：下载功能坏了不等于面板该死。也不能悄悄换一个“肯定
		// 连不上”的地址：那样 503 的提示会指向一个用户从没配过的端点，比
		// 报错更糟。这里保留用户填的那个地址（NewClient 只可能因“空/语法不
		// 通”失败，请求根本发不出去，效果就是 503 + 原样的地址），并把真实
		// 原因记进日志。config.Validate 已在启动时拒过一轮，走到这里是双保险。
		logx.Error("aria2 客户端装配失败，下载功能不可用: %v", err)
		// UnavailableClient 而不是 nil：nil 会让每个下载请求在对 nil 调方法
		// 时 panic，把"配置写错了"升级成"面板崩了"。
		client = download.UnavailableClient(err)
	}
	tasks := download.NewTaskStore(db, nil)
	svc := download.NewService(client, tasks, download.ServiceOptions{
		WSURL:        wsURLForRPC(cfg.Aria2RPCURL),
		PollInterval: downloadPollInterval,
	})
	// 落库放这里；Service 保证它返回之后才推 WS（顺序契约见 Service.emit）。
	// 用 context.Background() 而不是任何请求/关停上下文：一条 complete 落到
	// 一半被面板关停打断，后果是它在历史里永远停在"下载中"。
	svc.OnEvent = func(ev download.Event) {
		persistDownloadEvent(context.Background(), tasks, client, ev)
	}
	svc.ViaWS = api.BroadcastDownload(hub)
	return svc
}

// persistDownloadEvent 把状态变化落到历史表。
//
// 非终态（started/paused/stopped）只改 state 不写 finished_at：paused 之后
// resume，用户要看到它继续走，而不是"完成于刚才"。终态要带上大小：完成后
// 若不把 total/done 写进去，历史里的条目会显示 0 B —— 而这是它唯一能留下的
// 事实（aria2 那边马上就会忘掉它）。
func persistDownloadEvent(ctx context.Context, tasks *download.TaskStore, client *download.Client, ev download.Event) {
	switch ev.Kind {
	case download.EventComplete:
		total, done := finalSizes(ctx, client, ev.GID)
		_ = tasks.SetTerminal(ctx, ev.GID, download.StateComplete, "", total, done)
	case download.EventError:
		total, done := finalSizes(ctx, client, ev.GID)
		_ = tasks.SetTerminal(ctx, ev.GID, download.StateError, ev.Error, total, done)
	case download.EventStopped:
		// stop ≠ error：用户主动停的与坏掉的是两回事（设计 753 行的分组）。
		total, done := finalSizes(ctx, client, ev.GID)
		_ = tasks.SetTerminal(ctx, ev.GID, download.StateRemoved, "", total, done)
	case download.EventPaused:
		_ = tasks.SetState(ctx, ev.GID, download.StatePaused)
	case download.EventStart:
		_ = tasks.SetState(ctx, ev.GID, download.StateActive)
	}
}

// finalSizes 在落终态前抓最后一次 tellStatus 拿大小。
//
// 为什么不等轮询器的快照：complete 事件到达时轮询器可能还没到下一 tick，
// 而 aria2 对已完成任务的 tellStatus 立即有效（stopped 记录还在）。错过这个
// 窗口（比如 aria2 紧接着被清理）就只能落 0。查不到不报错：落 0 的代价只是
// 历史里少一个大小，报错的代价是整条终态写不进去（任务永远显示"下载中"）。
func finalSizes(ctx context.Context, client *download.Client, gid string) (int64, int64) {
	st, err := client.TellStatus(ctx, gid)
	if err != nil || st == nil {
		return 0, 0
	}
	return download.A2int(st.TotalLength), download.A2int(st.CompletedLength)
}

// startDownloads 起事件桥 + 轮询器 + 启动对账，返回关停句柄。
//
// svc 为 nil（装配失败）时返回空句柄而不是 panic：下载是可选功能，面板
// 其余部分没有理由陪它死。
// 参数是 any 而不是 *download.Service：调用方手里只有 deps.Downloads 那个
// **接口字段**（路由要的是 api.Downloads）。在这里做断言，是为了让"装配接错
// 类型"退化成"下载功能不可用"而不是编译期才暴露的耦合 —— 与 startFileJobs
// 收 any 同规。
func startDownloads(dep any) *downloadWiring {
	svc, _ := dep.(*download.Service)
	if svc == nil {
		return &downloadWiring{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc.Run(ctx)
	// 启动对账在 ctx 起来之后、且**异步**跑：它要问 aria2，而 aria2 慢或没起
	// 时不能把面板启动堵住（面板起来了而下载 503，是用户可以接受的状态；
	// 面板整个起不来，就不是了）。
	go func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rcancel()
		if n, err := svc.Reconcile(rctx); err != nil {
			logx.Info("下载历史启动对账跳过: %v", err)
		} else if n > 0 {
			logx.Info("下载历史对账：%d 条面板记得而 aria2 已不认的任务被标为中断", n)
		}
	}()
	return &downloadWiring{svc: svc, cancel: cancel}
}

// wsURLForRPC 从 HTTP RPC 地址推出事件 WS 地址（设计 485 行：同一个端点，
// 不同协议）。https → wss 必须一起换：协议不匹配的失败是握手超时一类的
// 难查错误，而不是"证书不受信"那种一眼话。
func wsURLForRPC(rpcURL string) string {
	u, err := url.Parse(strings.TrimSpace(rpcURL))
	if err != nil || u.Host == "" {
		return ""
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return ""
	}
	return u.String()
}
