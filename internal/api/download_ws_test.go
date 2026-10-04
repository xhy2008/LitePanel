package api_test

// WS 频道 downloads 的广播层（设计 747 行的频道表）。
//
// 为什么下载要有 WS 推送而不是让前端每 2s 拉一次：下载页在几个标签页里开着
// 就是每 2s 好几次全量列表请求，而绝大多数次返回的内容一个字都没变。事件推送
// 把"变了"与"没变"分开，进度那部分仍走面板侧轮询，前端只在状态变化时收到一次
// 唤醒、然后自己去拉一次列表。
//
// 载荷只带 {kind,gid,…} 而**不带完整任务行**，这是刻意的取舍：事件的形状由
// aria2 决定，它给的就是 gid；把整行拼进去要在事件路径上做一次全量合成
// （tellActive + 历史查询），而事件是可能连发一串的（一次 remove 会连带
// stop），那会把一次操作放大成 N 次全量查询。
//
// 用真 hub + 真 WS 连接而不是往 hub 里塞假客户端：Hub 没有导出注册方法，
// 手搓一个"订阅者"要绕过它自己的登记逻辑，测出来的"能收到"就与被测系统无关
// （同 cmd/litepanel 的 WS 接线测试用真拨号的理由）。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"litepanel/internal/api"
	"litepanel/internal/download"
	"litepanel/internal/ws"
)

// dlWS 起一个只挂 hub 的测试服务并拨一条已订阅 downloads 的连接。
func dlWS(t *testing.T, hub *ws.Hub) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(hub.Handler(nil))
	t.Cleanup(srv.Close)
	c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("WS 握手失败: %v (%v)", err, resp)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"ch":"downloads","t":"sub"}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hub.SubscriberCount("downloads") == 1 {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("订阅 downloads 未生效")
	return nil
}

// readDLFrame 读到下一个 downloads 的 data 帧（跳过心跳之类的无关帧）。
func readDLFrame(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := c.ReadMessage()
		if err != nil {
			continue
		}
		var f map[string]any
		if json.Unmarshal(msg, &f) != nil {
			continue
		}
		if f["ch"] == "downloads" && f["t"] == "data" {
			return f
		}
	}
	t.Fatal("5s 内没收到 downloads 的 data 帧")
	return nil
}

// 状态变化要落到 downloads 频道，且 gid/kind 原样可达。
func TestBroadcastDownloadPushesToChannel(t *testing.T) {
	// Hub 没有 Close：它是长命单例，连接随客户端断开而回收。
	hub := ws.NewHub()
	c := dlWS(t, hub)
	api.BroadcastDownload(hub)(download.Event{Kind: download.EventComplete, GID: "0xa", At: 100})

	f := readDLFrame(t, c)
	d, _ := f["d"].(map[string]any)
	if d["gid"] != "0xa" || d["kind"] != "completed" {
		t.Errorf("载荷不对: %v", f["d"])
	}
}

// error 事件的**原因**要原样过线。
//
// 这是"aria2 的失败原因必须可见"这条硬要求在 WS 那一侧的落点：前端收到 error
// 事件时若没有原因，就只能显示"失败"两个字，而用户要知道是 404 还是磁盘满。
func TestBroadcastDownloadKeepsErrorReason(t *testing.T) {
	// Hub 没有 Close：它是长命单例，连接随客户端断开而回收。
	hub := ws.NewHub()
	c := dlWS(t, hub)
	reason := "The remote resource was not found"
	api.BroadcastDownload(hub)(download.Event{Kind: download.EventError, GID: "0x2", Error: reason, At: 7})

	raw, _ := json.Marshal(readDLFrame(t, c))
	if !strings.Contains(string(raw), reason) {
		t.Errorf("失败原因要原样送到前端, got %s", raw)
	}
}

// hub 为 nil 时返回空函数而不是 nil（"只要 API 不要 WS"是合法装配；返回 nil
// 会让第一次推送踩空指针，崩在一个纯可选的配置上。同 BroadcastJob）。
func TestBroadcastDownloadNilHubSafe(t *testing.T) {
	api.BroadcastDownload(nil)(download.Event{Kind: download.EventStart, GID: "0x1"})
}

// 广播不能阻塞。
//
// 要紧的理由：事件从 aria2 的 WS 读循环里同步调用，若广播在慢客户端上阻塞，
// 一条卡住的浏览器连接会让整个下载事件桥停摆（aria2 那边开始积压）。这里故意
// 不消费帧，把 hub 的发送队列灌满后再广播。
func TestBroadcastDownloadDoesNotBlock(t *testing.T) {
	// Hub 没有 Close：它是长命单例，连接随客户端断开而回收。
	hub := ws.NewHub()
	c := dlWS(t, hub)
	_ = c // 只订阅、不读
	done := make(chan struct{})
	go func() {
		bc := api.BroadcastDownload(hub)
		for i := 0; i < 2000; i++ {
			bc(download.Event{Kind: download.EventStart, GID: "0xg", At: int64(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("广播被不消费的订阅者堵住了")
	}
}
