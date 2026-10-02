package api

import (
	"encoding/json"

	"litepanel/internal/filemgr"
	"litepanel/internal/ws"
)

// jobsChannel 是后台文件任务的进度频道（设计 747 行的频道表）。
const jobsChannel = "fsjobs"

// BroadcastJob 把一次任务状态变更推到 fsjobs 频道。
//
// 返回的是一个闭包，交给 filemgr 当 jobNotifier（NewService 装配时）。
// 与 BroadcastServiceEvent 同一套：hub 为 nil 时返回空函数而不是 nil，
// 否则"只要 API 不要 WS"的场合会在第一次任务进度时踩空指针 —— 崩在一
// 个纯可选的配置上。
//
// 载荷就是 JobProgress 的 JSON（字段名与 GET /fs/jobs 的行同名，前端
// 直接覆盖列表项）。帧的外层 {ch,t,seq,d} 由 hub.Broadcast 统一加，
// 这里只管 d。
func BroadcastJob(hub *ws.Hub) func(filemgr.JobProgress) {
	if hub == nil {
		return func(filemgr.JobProgress) {}
	}
	return func(p filemgr.JobProgress) {
		b, err := json.Marshal(p)
		if err != nil {
			return
		}
		hub.Broadcast(jobsChannel, b)
	}
}
