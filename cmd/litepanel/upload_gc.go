package main

import (
	"context"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/logx"
)

// uploadJanitor 是清理暂存所需的最小能力。
//
// 不塞进 api.Files：那个接口的定义是"HTTP 层需要什么"，而清理没有任何
// HTTP 入口（设计 17 节里也没有 /fs/uploads/scrub 之类的端点 —— 让前端
// 能触发清理只会多出一个可被误点的按钮）。把它做成独立接口，"面板能不能
// 清暂存"与"面板能不能传文件"就各自独立演进。
type uploadJanitor interface {
	GCUploads(ctx context.Context)
	SweepStale(ctx context.Context) error
}

// uploadJanitorInterval 是定时清理的间隔。
//
// 与回收站的每小时清理（设计 8.6）取同一个节奏：两者是同一类工作
// （扫一个目录、按时间判死活、删），不同节奏只会让"面板在后台干活"
// 这件事有两个互不相干的唤醒时刻，而没人能说出第二个的理由。
const uploadJanitorInterval = time.Hour

// startUploadJanitor 起一个后台清理循环，返回是否真的接上了。
//
// 启动时先扫一次，不等第一个 tick：面板上次是被升级/OOM/重启打断的，
// 目标目录里那个 .lp-part 半成品从进程回来的那一刻起就是垃圾，让它
// 再待一小时没有任何好处（同 bootServices / tw.Reconcile 的启动对账）。
//
// 顺序是先 SweepStale 再 GCUploads：前者收拾目标目录里的半成品（那才是
// 落在用户盘上的东西），后者清暂存目录里过期的会话。反过来也能工作，
// 但这个顺序让"最刺眼的残留"先被处理。
//
// 返回值不是装饰：这是唯一一个"漏接没有任何当下症状"的接线。其他漏接
// 是 501 或空页面，这个是几个月后磁盘莫名少了几个 G，而 du 一翻才发现
// 是一堆没人能看见的 .chunk。所以它必须自己报告在不在跑。
func startUploadJanitor(ctx context.Context, deps api.AuthDeps, interval time.Duration) bool {
	j, ok := deps.Files.(uploadJanitor)
	if !ok || deps.Files == nil {
		logx.Info("上传暂存清理未接：暂存目录不会自动回收")
		return false
	}
	if interval <= 0 {
		interval = uploadJanitorInterval
	}
	// ctx 的取消由 domain 层逐条处理（GCUploads 每清一项前查一次
	// ctx.Err()）：关停时正好打断在"删了一半"上是允许的 —— 剩下的
	// 那半个目录下次启动照样会被认出来并清掉，为它在这里再加一道
	// 前置检查是重复劳动（第一版加了，三次 -count 跑下来没有任何测试
	// 能区分加与不加，说明它没有行为，删）。
	sweep := func() {
		if err := j.SweepStale(ctx); err != nil {
			logx.Info("清理遗留的上传半成品: %v", err)
		}
		j.GCUploads(ctx)
	}
	sweep()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sweep()
			}
		}
	}()
	return true
}
