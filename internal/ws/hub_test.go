package ws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---------- 测试脚手架 ----------

type hubFixture struct {
	t     *testing.T
	hub   *Hub
	srv   *httptest.Server
	count struct {
		sync.Mutex
		metrics int // 最近一次 metrics 频道订阅数
		delta   []int
	}
}

func newHubFixture(t *testing.T, authOK bool) *hubFixture {
	t.Helper()
	h := NewHub()
	f := &hubFixture{t: t, hub: h}
	h.OnCount("metrics", func(n int) {
		f.count.Lock()
		f.count.metrics = n
		f.count.delta = append(f.count.delta, n)
		f.count.Unlock()
	})
	f.srv = httptest.NewServer(h.Handler(func(*http.Request) bool { return authOK }))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *hubFixture) url() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws"
}

func (f *hubFixture) dial() *websocket.Conn {
	f.t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(f.url(), nil)
	if err != nil {
		f.t.Fatalf("拨号失败: %v", err)
	}
	f.t.Cleanup(func() { c.Close() })
	return c
}

func (f *hubFixture) countNow() int {
	f.count.Lock()
	defer f.count.Unlock()
	return f.count.metrics
}

func subFrame(ch string) []byte {
	return []byte(`{"ch":"` + ch + `","t":"sub"}`)
}

// readJSON 带超时地读一帧文本，避免测试挂死。
func readJSON(t *testing.T, c *websocket.Conn, timeout time.Duration) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("期望文本帧, got 类型 %d", mt)
	}
	return string(data)
}

// ---------- 测试 ----------

// 未认证的握手必须被拒绝。
func TestUnauthenticatedHandshakeRejected(t *testing.T) {
	f := newHubFixture(t, false)
	_, resp, err := websocket.DefaultDialer.Dial(f.url(), nil)
	if err == nil {
		t.Fatal("未认证拨号应失败")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应返回 401, got %+v", resp)
	}
}

func TestSubscribeReceivesBroadcast(t *testing.T) {
	f := newHubFixture(t, true)
	c := f.dial()
	if err := c.WriteMessage(websocket.TextMessage, subFrame("services")); err != nil {
		t.Fatal(err)
	}
	// 等订阅生效（hub 是并发的，广播前确保计数可见）。
	waitFor(t, func() bool { return f.hub.SubscriberCount("services") == 1 })

	f.hub.Broadcast("services", mustJSON(t, map[string]any{"state": "running"}))
	msg := readJSON(t, c, 2*time.Second)
	if !strings.Contains(msg, `"ch":"services"`) || !strings.Contains(msg, "running") {
		t.Fatalf("广播帧不符: %s", msg)
	}
	if !strings.Contains(msg, `"seq":`) {
		t.Fatalf("帧应带 seq 供重连补帧: %s", msg)
	}
}

func TestUnsubscribeStopsFrames(t *testing.T) {
	f := newHubFixture(t, true)
	c := f.dial()
	_ = c.WriteMessage(websocket.TextMessage, subFrame("services"))
	waitFor(t, func() bool { return f.hub.SubscriberCount("services") == 1 })

	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"ch":"services","t":"unsub"}`))
	waitFor(t, func() bool { return f.hub.SubscriberCount("services") == 0 })

	// 退订后广播不应再送达：读超时即证明没收到。
	f.hub.Broadcast("services", mustJSON(t, map[string]any{"x": 1}))
	if err := c.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("退订后仍收到了帧")
	}
}

// D6 的落点：metrics 频道的订阅计数必须 0→1→0 精确回调。
func TestMetricsSubscriberCountDrivesCollector(t *testing.T) {
	f := newHubFixture(t, true)
	if got := f.countNow(); got != 0 {
		t.Fatalf("初始计数应为 0, got %d", got)
	}
	c1, c2 := f.dial(), f.dial()
	_ = c1.WriteMessage(websocket.TextMessage, subFrame("metrics"))
	waitFor(t, func() bool { return f.countNow() == 1 })
	_ = c2.WriteMessage(websocket.TextMessage, subFrame("metrics"))
	waitFor(t, func() bool { return f.countNow() == 2 })

	c2.Close()
	waitFor(t, func() bool { return f.countNow() == 1 })
	c1.Close()
	waitFor(t, func() bool { return f.countNow() == 0 })

	f.count.Lock()
	seq := fmtSeq(f.count.delta)
	f.count.Unlock()
	if seq != "1,2,1,0" {
		t.Fatalf("计数序列应为 1,2,1,0，实际 %s", seq)
	}
}

// 异常断开（直接关 TCP、不发 unsub）也必须回收计数，这是最容易泄漏的地方。
func TestAbnormalDisconnectDecrementsCount(t *testing.T) {
	f := newHubFixture(t, true)

	// 用不受 t.Cleanup 管理的连接，以便暴力关闭底层 TCP。
	c, _, err := websocket.DefaultDialer.Dial(f.url(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.WriteMessage(websocket.TextMessage, subFrame("metrics"))
	waitFor(t, func() bool { return f.countNow() == 1 })

	mustRawClose(t, c) // 绕过 Close 帧，等价于浏览器被杀进程

	waitFor(t, func() bool { return f.countNow() == 0 })
	if got := f.hub.ClientCount(); got != 0 {
		t.Fatalf("客户端应被回收, 剩余 %d", got)
	}
}

// 慢客户端不能被无限追帧，也不能把 hub 拖死。
// 注意不断言“另一个不读的客户端仍能收帧”：不限速洪水中，任何
// 不被读取的连接都会被丢弃踢出（这是设计行为），那种断言只会
// 跟着调度快慢随机失败。真正要守的是：慢客户端被踢、hub 仍可用。
func TestSlowClientDoesNotBlockHub(t *testing.T) {
	f := newHubFixture(t, true)
	slow := f.dial()
	_ = slow.WriteMessage(websocket.TextMessage, subFrame("metrics"))
	waitFor(t, func() bool { return f.hub.SubscriberCount("metrics") == 1 })

	// slow 完全不读，塞满它的发送缓冲。
	defer func() { _ = slow.Close() }() // 不等 t.Cleanup，避开测试末尾的写报错
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		f.hub.Broadcast("metrics", mustJSON(t, map[string]any{"i": i}))
	}
	// 慢客户端要被踢掉并回收计数，而不是把 hub 内存吃光。
	waitFor(t, func() bool { return f.hub.SubscriberCount("metrics") == 0 })

	// 洪水之后 hub 仍要能服务新客户端。
	fresh := f.dial()
	_ = fresh.WriteMessage(websocket.TextMessage, subFrame("metrics"))
	waitFor(t, func() bool { return f.hub.SubscriberCount("metrics") == 1 })
	read := make(chan string, 1)
	go func() {
		_ = fresh.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := fresh.ReadMessage()
		if err != nil {
			read <- "ERR: " + err.Error()
			return
		}
		read <- string(data)
	}()
	f.hub.Broadcast("metrics", mustJSON(t, map[string]any{"after": "flood"}))
	select {
	case msg := <-read:
		if !strings.Contains(msg, "flood") {
			t.Fatalf("洪水后新客户端收帧异常: %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("洪水后新客户端收不到帧，hub 已被拖死")
	}
}

// 未知频道与非法帧应回 err 帧而不是打断连接。
func TestBadFrameDoesNotKillConnection(t *testing.T) {
	f := newHubFixture(t, true)
	c := f.dial()
	_ = c.WriteMessage(websocket.TextMessage, []byte(`not json`))
	msg := readJSON(t, c, 2*time.Second)
	if !strings.Contains(msg, `"t":"err"`) {
		t.Fatalf("应回 err 帧, got %s", msg)
	}
	// 连接仍可用。
	_ = c.WriteMessage(websocket.TextMessage, subFrame("services"))
	waitFor(t, func() bool { return f.hub.SubscriberCount("services") == 1 })
}

// 二进制终端帧的编解码往返（M5 会依赖）。
func TestBinaryFrameEncodeDecode(t *testing.T) {
	payload := append([]byte{0x1b, '[', '3', '1', 'm', 0x00, 0xff}, []byte("中")...)
	for _, ch := range []string{"term:1", "term:12345"} {
		raw := EncodeTermFrame(ch, payload)
		gotCh, gotPayload, err := DecodeTermFrame(raw)
		if err != nil {
			t.Fatalf("%s: %v", ch, err)
		}
		if gotCh != ch {
			t.Errorf("频道名 = %q, want %q", gotCh, ch)
		}
		if string(gotPayload) != string(payload) {
			t.Errorf("负载不符: %v vs %v", gotPayload, payload)
		}
	}
}

func TestBinaryFrameRejectsTruncated(t *testing.T) {
	if _, _, err := DecodeTermFrame([]byte{}); err == nil {
		t.Fatal("空帧应报错")
	}
	if _, _, err := DecodeTermFrame([]byte{5, 'a'}); err == nil {
		t.Fatal("长度字段与实际不符应报错")
	}
}

// 心跳：客户端 ping 要回 pong，服务端不能因客户端活着而踢它。
func TestClientPingKeepsConnectionAlive(t *testing.T) {
	f := newHubFixture(t, true)
	c := f.dial()
	if err := c.WriteMessage(websocket.PingMessage, []byte("hb")); err != nil {
		t.Fatal(err)
	}
	// gorilla 默认会自动回应 ping；这里断言连接仍可用于订阅。
	_ = c.WriteMessage(websocket.TextMessage, subFrame("downloads"))
	waitFor(t, func() bool { return f.hub.SubscriberCount("downloads") == 1 })
}

// 同一客户端重复订阅同一频道不应重复计数。
func TestDoubleSubscribeIsIdempotent(t *testing.T) {
	f := newHubFixture(t, true)
	c := f.dial()
	_ = c.WriteMessage(websocket.TextMessage, subFrame("metrics"))
	waitFor(t, func() bool { return f.countNow() == 1 })
	_ = c.WriteMessage(websocket.TextMessage, subFrame("metrics"))
	time.Sleep(200 * time.Millisecond)
	if got := f.countNow(); got != 1 {
		t.Fatalf("重复订阅不应把计数变成 %d", got)
	}
}
