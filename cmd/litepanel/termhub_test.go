package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"litepanel/internal/ws"
)

// termHub 适配器的最小验收：订阅者要原样穿过去，频道名不能丢。
//
// 为什么单独测它：桥接的 Hub 接口把订阅者收成 any，适配器里那句
// sub.(*ws.Client) 一旦写错，编译不会错、桥接单测（用假 hub）不会错、
// 连 WS 端到端测试也照旧绿 —— 因为按键路径走 BroadcastBin，只有
// "新设备接入补历史"这一条会静默失效：新标签页一片空白，
// 要等下一条命令才有字。
func TestTermHubDeliversToExactSubscriber(t *testing.T) {
	h := ws.NewHub()

	// 先登记 join 回调，再让浏览器订阅：这样订阅者令牌是从 hub 的回调里
	// 拿到的**真令牌**，测试不用自己造 *ws.Client（自造就测不到"适配器
	// 认不认得 hub 实际发放的令牌"这件事）。
	var mu sync.Mutex
	var subs []any
	h.OnJoin("term:", func(c *ws.Client, ch string) {
		mu.Lock()
		subs = append(subs, any(c))
		mu.Unlock()
	})

	srv := httptest.NewServer(h.Handler(func(*http.Request) bool { return true }))
	defer srv.Close()
	dial := func() *websocket.Conn {
		t.Helper()
		c, _, err := websocket.DefaultDialer.Dial(
			"ws"+strings.TrimPrefix(srv.URL, "http")+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := dial(), dial()
	defer a.Close()
	defer b.Close()
	for _, c := range []*websocket.Conn{a, b} {
		if err := c.WriteMessage(websocket.TextMessage,
			[]byte(`{"t":"sub","ch":"term:1"}`)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return h.SubscriberCount("term:1") == 2 }, "两个订阅者登记")

	mu.Lock()
	got := append([]any(nil), subs...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("join 回调该给 2 个令牌, got %d", len(got))
	}

	th := termHub{h: h}
	// 只发给其中一个订阅者：另一个不能收到（回放语义 = 只补刚接入的那台）。
	// 不断言"一定是 a"：join 回调的顺序与两条拨号顺序无关，写死 a/b
	// 会造出一条本身随机红的测试。
	th.SendBinTo(got[0], "term:1", []byte("ONLY-FIRST"))
	if n := countReceivers([]*websocket.Conn{a, b}, "ONLY-FIRST", 3*time.Second); n != 1 {
		t.Fatalf("单点投递有 %d 台收到，应为 1（0=适配器丢了订阅者，2=变成广播了）", n)
	}

	// 频道名必须原样传递：前缀登记下同一个回调对应多个会话，
	// 适配器把频道写死或丢掉，桥接就没法按会话分派。
	// 这里换一个**没被任何人订阅**的频道：若适配器把频道丢掉/写死成
	// join 时那个，广播就会错误地落到 term:1 的订阅者身上。
	th.BroadcastBin("term:2", []byte("CH-TWO"))
	if n := countReceivers([]*websocket.Conn{a, b}, "CH-TWO", 400*time.Millisecond); n != 0 {
		t.Fatalf("term:2 的广播漏进了 term:1 的订阅者（频道名串了）: %d 台收到", n)
	}
	// 真订阅 term:2 的设备必须收到，否则"没收到"只是因为没人订
	c := dial()
	defer c.Close()
	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"t":"sub","ch":"term:2"}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.SubscriberCount("term:2") == 1 }, "term:2 订阅登记")
	th.BroadcastBin("term:2", []byte("CH-TWO-2"))
	if !readUntil(c, "CH-TWO-2", 3*time.Second) {
		t.Fatal("term:2 的订阅者没收到本频道的广播")
	}

	// 类型不对的令牌必须被安全忽略，而不是 panic 或误投
	th.SendBinTo("不是客户端", "term:1", []byte("NOPE"))
	if n := countReceivers([]*websocket.Conn{a, b}, "NOPE", 300*time.Millisecond); n != 0 {
		t.Fatalf("非 *ws.Client 的令牌竟然投递给了 %d 台", n)
	}
}

// countReceivers 数有多少条连接收到了含 want 的帧。
func countReceivers(cs []*websocket.Conn, want string, budget time.Duration) int {
	n := 0
	for _, c := range cs {
		if readUntil(c, want, budget) {
			n++
		}
	}
	return n
}

// readUntil 读到出现 want 为止；返回是否读到。
// 出错立即返回 false：连接已死时继续读，gorilla 累计 1000 次失败会主动 panic。
func readUntil(c *websocket.Conn, want string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(budget))
		_, data, err := c.ReadMessage()
		if err != nil {
			return false
		}
		if strings.Contains(string(data), want) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}
