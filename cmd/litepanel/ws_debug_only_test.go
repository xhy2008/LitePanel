//go:build debug

package main

// 端到端验证"访问日志中间件挂在链上时 WebSocket 仍能升级与收发"。
// 仅调试构建：release 里 accessLog 被 logx.Enabled 编译期剥离，
// 链上没有包装器，这个场景不存在；statusWriter 的 Hijack 透传由
// internal/api 的 accesslog_hijack_test.go 在两种构建下共同把关。

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"litepanel/internal/config"
	"litepanel/internal/metrics"
	"litepanel/internal/ws"

	"github.com/gorilla/websocket"
)

// -debug 打开时 WebSocket 必须仍然能升级。
//
// M1 遗留缺陷：访问日志中间件用 statusWriter 包住 ResponseWriter，
// 而它内嵌的是 http.ResponseWriter 接口，方法集里没有 Hijack。
// gorilla 靠 w.(http.Hijacker) 抢裸 TCP，断言失败就回
// 500 "websocket: response does not implement http.Hijacker"。
// 后果是：一旦开 -debug 排查问题，WS 就全挂 —— 而需要 -debug 的
// 时候恰恰都是问题最刁钻的时候。
func TestWebSocketWorksWithDebugAccessLog(t *testing.T) {
	db := openTestDB(t)
	const pwd = "debug-ws-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	hub := ws.NewHub()
	var log bytes.Buffer
	// 接上采集器：没有订阅者驱动的广播时，“读不到帧”无法区分
	// “中间件拆断了连接”与“本来就没东西可推”，测不出东西。
	col := wireMetrics(hub, &probeSource{}, 10*time.Millisecond)
	defer col.Stop()
	// debug=true 是本用例的全部重点。
	deps := buildDeps(db, config.Config{}, hub, true, &log)
	deps.Metrics = col
	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	// 握手必须成功（dialWS 内部会 fatal）。
	c := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", token)
	defer c.Close()

	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"ch":"`+metrics.ChannelMetrics+`","t":"sub"}`)); err != nil {
		t.Fatal(err)
	}
	// 必须真的收到 metrics 数据帧：这才证明中间件没拆断双向流量。
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var frame struct {
			T  string `json:"t"`
			Ch string `json:"ch"`
		}
		if err := c.ReadJSON(&frame); err != nil {
			t.Fatalf("-debug 下读不到帧: %v", err)
		}
		if frame.T == "data" && frame.Ch == metrics.ChannelMetrics {
			break
		}
	}
	// -debug 下访问日志仍要记到东西（否则修 Hijack 时把日志顺带改没了也不会发现）。
	// 断言的是具体行为，不是"日志里有 /ws" —— 后者连"→"那半行都能满足，
	// 把 hijacked 分支整段废掉也照样绿，等于没测。
	out := log.String()
	if !strings.Contains(out, "↔ GET /ws") {
		t.Errorf("应记下接管行 ↔ /ws, got %q", out)
	}
	// 绝不能出现"← /ws | 200"这种完成行：它看着像请求已正常结束，
	// 而实际长连接还在跑 —— 这正是本 bug 最需要避免的误导。
	if strings.Contains(out, "← GET /ws") {
		t.Errorf("被接管的连接不该记完成行, got %q", out)
	}
}
