package api_test

import (
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/service"
)

// AuthDeps.Hub 是可选的（只要 API、不要 WS 的部署）。漏了这层判空的话，
// 服务一退出，watcher goroutine 就在 Broadcast 上踩空 —— 面板整体崩，
// 而且只在一个"看起来无关"的可选配置下崩。
func TestBroadcastHelpersTolerateNilHub(t *testing.T) {
	api.BroadcastServiceEvent(nil)(service.Event{ID: 1, State: "running"})
	api.BroadcastServiceLog(nil)(1, []string{"x"})

	// 空行批次不该产生帧（也不该 panic）。
	api.BroadcastServiceLog(nil)(1, nil)
}
