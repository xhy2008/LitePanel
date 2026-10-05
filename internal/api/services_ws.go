package api

import (
	"context"
	"encoding/json"
	"time"

	"litepanel/internal/service"
	"litepanel/internal/ws"
)

// ServiceSupervisor 是 HTTP 层需要的全部服务能力。
//
// 与 MetricsSource 同样的理由：订阅计数、事件来源这些判断依赖
// supervisor 的内部状态，在 HTTP 层重做一遍必然与它漂移。
// *service.Supervisor 天然满足。
type ServiceSupervisor interface {
	Start(svc service.Service) (service.State, error)
	Stop(ctx context.Context, svc service.Service, grace time.Duration) (service.State, error)
	// Grace 是当前默认停止宽限期。handler 用它给整个请求定时，并在调用
	// Stop 时传 0 表示"用面板当前设置"——不这样接的话，设置页上的
	// "停止宽限时长"就只是个存进库没人读的数字。
	Grace() time.Duration
	Log(id int64) *service.LogBuf
	OnEvent(func(service.Event))
	OnLog(func(id int64, lines []string))
}

// svcChannel 是服务状态频道，svclog:{id} 是单服务的日志流频道。
const svcChannel = "services"

func svcLogChannel(id int64) string {
	return "svclog:" + itoa(id)
}

// serviceEvent 通知前端"列表本身变了"（新建/删除）。
// 启停的状态变化由 supervisor 自己发，handler 不重复发，免得两处口径打架。
func (d AuthDeps) serviceEvent(id int64, state string) {
	if d.Hub == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"id": id, "state": state, "reload": true,
	})
	if err != nil {
		return
	}
	d.Hub.Broadcast(svcChannel, payload)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// BroadcastServiceEvent 把状态变化推到 services 频道。
// 帧结构与 metrics 一致（{ch,t,d}），前端只有一套解析逻辑。
func BroadcastServiceEvent(hub *ws.Hub) func(service.Event) {
	// AuthDeps.Hub 允许为 nil（只要 API、不要 WS 的场合）。这里不拦，
	// watcher goroutine 会在服务退出时踩空指针 —— 崩在一个纯可选的配置上。
	if hub == nil {
		return func(service.Event) {}
	}
	return func(ev service.Event) {
		payload, err := json.Marshal(ev)
		if err != nil {
			return
		}
		hub.Broadcast(svcChannel, payload)
	}
}

// BroadcastServiceLog 把新日志行推给订阅了该服务日志的客户端。
// 没有订阅者时 Broadcast 自己就是廉价的，不必额外计数。
func BroadcastServiceLog(hub *ws.Hub) func(id int64, lines []string) {
	if hub == nil {
		return func(int64, []string) {}
	}
	return func(id int64, lines []string) {
		if len(lines) == 0 {
			return
		}
		payload, err := json.Marshal(map[string]any{"id": id, "lines": lines})
		if err != nil {
			return
		}
		hub.Broadcast(svcLogChannel(id), payload)
	}
}
