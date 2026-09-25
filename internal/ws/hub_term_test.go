package ws

// 终端桥接需要 hub 提供三个现在没有的能力（M5-T5）：
//
//  1. 新订阅者通知：回放只能发给刚接入的那台设备，不能广播 —— 否则 PC 上
//     正在跑的会话被手机一连就整屏重播一遍。
//  2. 单点投递：回放的落点，配合 1。
//  3. 入站二进制路由：浏览器按键走二进制帧上行，现在 readPump 一律回 err。

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 1. 新订阅者通知
func TestJoinCallbackFiresPerNewSubscriber(t *testing.T) {
	f := newHubFixture(t, true)

	var mu sync.Mutex
	var joined []*Client
	f.hub.OnJoin("term:1", func(c *Client) {
		mu.Lock()
		joined = append(joined, c)
		mu.Unlock()
	})

	c1 := f.dial()
	if err := c1.WriteMessage(websocket.TextMessage, subFrame("term:1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.hub.SubscriberCount("term:1") == 1 })
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(joined) == 1
	})

	// 重复 sub 同一频道不得再触发（否则重连风暴会把历史重播 N 遍）
	if err := c1.WriteMessage(websocket.TextMessage, subFrame("term:1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.hub.SubscriberCount("term:1") == 1 })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(joined) != 1 {
		mu.Unlock()
		t.Fatalf("重复订阅不应重复触发 join, got %d 次", len(joined))
	}
	mu.Unlock()

	// 第二台设备接入要触发（回放只发给它）
	c2 := f.dial()
	if err := c2.WriteMessage(websocket.TextMessage, subFrame("term:1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(joined) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	if joined[0] == joined[1] {
		t.Fatal("两次 join 应是不同的 client")
	}
}

// 2. 单点投递：只给一个客户端，其他订阅者收不到
func TestSendBinToReachesOnlyTarget(t *testing.T) {
	f := newHubFixture(t, true)

	c1 := f.dial()
	c2 := f.dial()
	for _, c := range []*websocket.Conn{c1, c2} {
		if err := c.WriteMessage(websocket.TextMessage, subFrame("term:1")); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return f.hub.SubscriberCount("term:1") == 2 })

	// 取 c1 对应的 *Client：join 回调是唯一把 hub 内部 client 交给外部的通道
	var mu sync.Mutex
	var target *Client
	f.hub.OnJoin("term:9", func(c *Client) {
		mu.Lock()
		target = c
		mu.Unlock()
	})
	c3 := f.dial()
	if err := c3.WriteMessage(websocket.TextMessage, subFrame("term:9")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return target != nil
	})
	// 注意：target 订的是 term:9，向 term:1 单点投递不该碰到它
	f.hub.SendBinTo(target, "term:1", []byte("replay"))

	// c1/c2 都不该收到任何东西
	_ = c2.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if mt, data, err := c2.ReadMessage(); err == nil {
		t.Fatalf("非目标客户端收到了帧: type=%d %q", mt, data)
	}

	// 换个正确频道再投，确认 SendBinTo 本身是通的（排除"永远发不出去"这种假通过）
	f.hub.SendBinTo(target, "term:9", []byte("hello-replay"))
	_ = c3.SetReadDeadline(time.Now().Add(2 * time.Second))
	mt, data, err := c3.ReadMessage()
	if err != nil {
		t.Fatalf("目标客户端没收到单点帧: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("应为二进制帧, got type=%d", mt)
	}
	ch, payload, err := DecodeTermFrame(data)
	if err != nil {
		t.Fatalf("帧解析失败: %v", err)
	}
	if ch != "term:9" || string(payload) != "hello-replay" {
		t.Fatalf("帧内容不符: ch=%q payload=%q", ch, payload)
	}
}

// 3. 入站二进制路由
func TestInboundBinaryRoutedByChannel(t *testing.T) {
	f := newHubFixture(t, true)

	type got struct {
		ch, payload string
	}
	var mu sync.Mutex
	var gotCh []got
	f.hub.OnBinary("term:", func(c *Client, ch string, payload []byte) {
		mu.Lock()
		gotCh = append(gotCh, got{ch, string(payload)})
		mu.Unlock()
	})

	c := f.dial()
	if err := c.WriteMessage(websocket.TextMessage, subFrame("term:7")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.hub.SubscriberCount("term:7") == 1 })

	if err := c.WriteMessage(websocket.BinaryMessage, EncodeTermFrame("term:7", []byte("ls\r"))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(gotCh) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if gotCh[0].ch != "term:7" || gotCh[0].payload != "ls\r" {
		t.Fatalf("上行路由结果不符: %+v", gotCh[0])
	}
}

// 前缀已注册但该客户端没订阅这个频道：不得路由（变异验证：去掉 routeBinary
// 里的订阅校验，只有这条会红 —— 前缀未注册那条测不到它）。
// 否则任何已登录连接都能往它没订阅的会话里灌按键。
func TestInboundBinaryRequiresSubscription(t *testing.T) {
	f := newHubFixture(t, true)

	var mu sync.Mutex
	hit := 0
	f.hub.OnBinary("term:", func(*Client, string, []byte) {
		mu.Lock()
		hit++
		mu.Unlock()
	})

	c := f.dial()
	if err := c.WriteMessage(websocket.TextMessage, subFrame("term:7")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.hub.SubscriberCount("term:7") == 1 })

	// 同前缀、不同会话：前缀匹配但不该路由
	if err := c.WriteMessage(websocket.BinaryMessage, EncodeTermFrame("term:8", []byte("rm -rf /\r"))); err != nil {
		t.Fatal(err)
	}
	msg := readJSON(t, c, 2*time.Second)
	if !strings.Contains(msg, `"err"`) {
		t.Fatalf("未订阅频道应回 err, got %s", msg)
	}
	mu.Lock()
	defer mu.Unlock()
	if hit != 0 {
		t.Fatalf("未订阅频道不该投递给处理器, 命中 %d 次", hit)
	}
}

// 没有注册处理器的频道前缀：必须回 err 而不是静默丢弃，也不能崩
func TestInboundBinaryUnknownChannelGetsError(t *testing.T) {
	f := newHubFixture(t, true)
	f.hub.OnBinary("term:", func(*Client, string, []byte) {
		t.Error("不该路由到 term 处理器")
	})

	c := f.dial()
	if err := c.WriteMessage(websocket.BinaryMessage, EncodeTermFrame("metrics", []byte("x"))); err != nil {
		t.Fatal(err)
	}
	msg := readJSON(t, c, 2*time.Second)
	if !strings.Contains(msg, `"err"`) {
		t.Fatalf("未注册的入站频道应回 err 帧, got %s", msg)
	}
}
