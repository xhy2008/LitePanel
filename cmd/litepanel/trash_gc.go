package main

import (
	"context"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/logx"
)

// trashJanitor 是回收站过期清理需要的最小能力。
//
// 独立于 api.Files（那个接口定义的是"HTTP 层需要什么"）：清理没有 HTTP
// 入口，也不该有 —— 设计 17 节里没有 /fs/trash/scrub 这种端点，让前端能
// 触发一个"绕过保留期删东西"的动作只会多一个可被误点的按钮。
type trashJanitor interface {
	CleanTrash(ctx context.Context) (int, error)
}

// trashJanitorInterval 是回收站清理的间隔（设计 8.6：每小时一次）。
const trashJanitorInterval = time.Hour

// startTrashJanitor 起一个后台清理循环，返回是否真的接上了。
//
// 与 startUploadJanitor 同构：启动时先清一次，不等第一个 tick。面板上次
// 可能整个保留期都没在跑（升级、断电、OOM），期间早就该永久删除的条目
// 一直占着磁盘；进程回来的那一刻它们依然是垃圾，再等一小时没有任何
// 好处。
//
// 返回值不是装饰：漏接清理的 symptoms 要 3 天后才出现，而且方向是"该删
// 的没删"—— 磁盘被用户"已经删掉的文件"慢慢吃掉，而回收站界面里那些条目
// 看起来完全正常（它们确实还在保留期内，只是那个期限永远到不了）。
// trash_retain_days 也会跟着变成摆设：改它没有任何可见后果。所以它必须
// 自己报告在不在跑。
func startTrashJanitor(ctx context.Context, deps api.AuthDeps, interval time.Duration) bool {
	j, ok := deps.Files.(trashJanitor)
	if !ok || deps.Files == nil {
		logx.Info("回收站过期清理未接：超过保留期的条目不会自动永久删除")
		return false
	}
	if interval <= 0 {
		interval = trashJanitorInterval
	}
	sweep := func() {
		// 这里不再加 ctx.Err() 前置检查：CleanTrash 每清一条前自己就查
		// ctx，关停时该停就停（剩下的条目下次启动照样会被认出来清掉，
		// 为它再加一道是重复劳动 —— 与 startUploadJanitor 同一个决定）。
		n, err := j.CleanTrash(ctx)
		if err != nil {
			logx.Info("回收站过期清理中断（已清 %d 条）: %v", n, err)
			return
		}
		if n > 0 {
			logx.Info("回收站过期清理：永久删除 %d 条", n)
		}
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
