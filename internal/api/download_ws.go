package api

import (
	"encoding/json"

	"litepanel/internal/download"
	"litepanel/internal/ws"
)

// downloadsChannel 是下载状态变化的频道（设计 747 行的频道表）。
const downloadsChannel = "downloads"

// BroadcastDownload 把一次下载状态变化推到 downloads 频道。
//
// 载荷是 download.Event 的 JSON（{kind,gid,error,at}），**不带完整任务行**：
// 拼整行要在事件路径上做一次全量合成，而 remove 之类会连发一串事件，那会把
// 一次操作放大成 N 次 tellActive + 历史查询。前端的正确用法是收到事件后合并
// 成一次列表刷新（多个事件 → 一次 GET /dl/tasks）。
//
// hub 为 nil 时返回空函数而不是 nil：与 BroadcastJob / BroadcastServiceEvent
// 同规，"只要 API 不要 WS"的装配不能崩在可选配置上。
func BroadcastDownload(hub *ws.Hub) func(download.Event) {
	if hub == nil {
		return func(download.Event) {}
	}
	return func(ev download.Event) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		hub.Broadcast(downloadsChannel, b)
	}
}
