package download

// 事件桥的测试替身：假的 aria2 WS server。
//
// 用真 WS server 而不是把"解析事件"抽成纯函数单测：aria2 的通知格式有两个
// 反直觉之处，只有过真连接才测得到 ——
//   1. 它是 JSON-RPC **notification**（没有 id 字段），响应式的 handler 写法
//      在这里会静默什么都不做；
//   2. 真实载荷嵌了一层：params[0] 是 {"params":[gid,...]}，gid 不在顶层。
//      照 JSON-RPC 的直觉去读 params[0] 会拿到一个对象而不是 gid。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// recorder 收集桥推给前端的**领域事件**。
//
// 领域包不 import ws（与 filemgr 的 JobNotifier 同一套接缝）：事件桥只往上
// 交类型化的 Event，序列化与频道名归 api 包。接缝放这里的好处是“改了状态
// 没推”会变成“没调用 notifier”，在包内就能测出来，而不是等到装配层。
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) notify(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// waitFor 等“出现满足条件的事件”。事件桥是异步的，用固定 sleep 猜时长在慢
// 机器上会随机失败（表现为“隔天红一次”），必须轮询 + 超时。
func (r *recorder) waitFor(t *testing.T, what string, pred func(Event) bool) Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range r.all() {
			if pred(e) {
				return e
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("3s 内没等到%s", what)
	return Event{}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// fakeAria2 是一个能被服务端主动推送的假 aria2。
type fakeAria2 struct {
	srv   *httptest.Server
	up    websocket.Upgrader
	mu    sync.Mutex
	conns []*websocket.Conn
	reqs  []map[string]any // 桥发过来的 RPC 请求（重连后补状态用的）
	// dropOnConnect：一连上就掐断，用来测“反复重连 + 退避”。
	dropOnConnect bool
	// onConnect 在每次连接建立时调用（推送、或故意断开以模拟掉线）。
	onConnect func(f *fakeAria2, c *websocket.Conn)
}

func newFakeAria2(t *testing.T) *fakeAria2 {
	f := &fakeAria2{up: websocket.Upgrader{
		// 面板是同机自建连接，没有浏览器 Origin；这里放开是因为测试要
		// 直连，生产代码的 Origin 检查在 hub 那边（面板自己的 server）。
		CheckOrigin: func(*http.Request) bool { return true },
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := f.up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		h := f.onConnect
		drop := f.dropOnConnect
		f.mu.Unlock()
		if drop {
			_ = c.Close()
			return
		}
		if h != nil {
			h(f, c)
		} else {
			// 默认只读不发，保持连接活着。
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *fakeAria2) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeAria2) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

func (f *fakeAria2) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.reqs...)
}

// notify 按 aria2 的**真实**格式推一条通知。
//
// 形状是实测出来的（aria2 1.37.0，见 dev/aria2probe 抓到的原始报文）：
//
//	{"jsonrpc":"2.0","method":"aria2.onDownloadStart","params":[{"gid":"1df9..."}]}
//
// params[0] 是一个 **带 gid 键的对象**，不是裸 gid、也不是再嵌一层
// {"params":[gid]}。早期设计照 JSON-RPC 直觉以为要取 params[0].params[0]，
// 实测证明不对——解析器必须两种都接（不同 aria2 版本/配置下形状会变），
// 但假 aria2 默认发**实测形状**，免得拿一个猜测的形状自测自我。
func (f *fakeAria2) notify(method string, params ...any) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"method":  "aria2." + method,
		"params":  []any{map[string]any{"gid": firstOrEmpty(params)}},
	}
	f.mu.Lock()
	conns := append([]*websocket.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.WriteMessage(websocket.TextMessage, mustJSON(body))
	}
}

func firstOrEmpty(a []any) any {
	if len(a) == 0 {
		return ""
	}
	return a[0]
}

// notifyNested 发另一种历史形状 params[0].params[0]=gid（老版本/某些配置）。
// 解析器要同时接得下，这条测试钉住向后兼容。
func (f *fakeAria2) notifyNested(method string, gid string) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"method":  "aria2." + method,
		"params":  []any{map[string]any{"params": []any{gid}}},
	}
	f.mu.Lock()
	conns := append([]*websocket.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.WriteMessage(websocket.TextMessage, mustJSON(body))
	}
}

// serveRequests 让假 aria2 回应桥发来的 RPC（重连补状态要发 tellActive）。
func (f *fakeAria2) serveRequests(result any) {
	f.mu.Lock()
	f.onConnect = func(fa *fakeAria2, c *websocket.Conn) {
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			_ = json.Unmarshal(raw, &req)
			fa.mu.Lock()
			fa.reqs = append(fa.reqs, req)
			res := result
			fa.mu.Unlock()
			_ = c.WriteMessage(websocket.TextMessage, mustJSON(map[string]any{
				"jsonrpc": "2.0", "id": req["id"], "result": res,
			}))
		}
	}
}

// notifyRaw 发一条**形状不对**的报文，测桥能不能安全略过而不是崩或退出。
func (f *fakeAria2) notifyRaw(body string) {
	f.mu.Lock()
	conns := append([]*websocket.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.WriteMessage(websocket.TextMessage, []byte(body))
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// waitCond 轮询到条件成立（用于"重连发生了"这类异步观察）。
func waitCond(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("3s 内条件未成立：%s", what)
}
