// Package ws 实现单连接多路复用的推送层（设计 15 节）。
//
// 控制帧（JSON 文本）：{"ch":"...","t":"sub|unsub|data|err","seq":N,"d":...}
// 数据帧（二进制）  ：[1B 频道名长度][频道名][原始字节]
package ws

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// 每客户端发送缓冲。满了先丢帧；连续丢弃过多则断开（慢客户端保护）。
	sendQueueSize = 256
	// 连续丢弃达到该阈值判定为不可救药的慢客户端。
	dropLimit = 128
	// 服务端 60s 没听到任何消息（含 ping）就断开（设计 15 节）。
	readTimeout = 60 * time.Second
	writeWait   = 10 * time.Second
	pingPeriod  = 25 * time.Second
)

// Frame 是下行控制帧。
type Frame struct {
	Ch  string          `json:"ch"`
	T   string          `json:"t"`
	Seq uint64          `json:"seq,omitempty"`
	D   json.RawMessage `json:"d,omitempty"`
}

type outItem struct {
	mt      int
	payload []byte
}

// Client 是一个浏览器连接。
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan outItem

	closed atomic.Bool // 关闭幂等保护
	drops  atomic.Int32
}

// Hub 维护客户端与频道订阅关系。
type Hub struct {
	mu      sync.Mutex
	clients map[*Client]map[string]bool // client -> 订阅的频道集合
	byCh    map[string]map[*Client]bool // 频道 -> 订阅者
	countCB map[string]func(int)
	joinCB  map[string]func(*Client, string)
	binCB   map[string]func(*Client, string, []byte) // 前缀 -> 入站二进制处理器
	seq     atomic.Uint64

	closed atomic.Bool
}

func NewHub() *Hub {
	return &Hub{
		clients: map[*Client]map[string]bool{},
		byCh:    map[string]map[*Client]bool{},
		countCB: map[string]func(int){},
		joinCB:  map[string]func(*Client, string){},
		binCB:   map[string]func(*Client, string, []byte){},
	}
}

// OnCount 注册频道订阅数变化回调。metrics 频道用它驱动采集器启停（D6）。
// 必须在服务开始前注册完毕。
func (h *Hub) OnCount(channel string, fn func(int)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.countCB[channel] = fn
}

// OnJoin 在「某个客户端新增订阅该频道」时回调（重复 sub 不重复触发）。
// 终端用它做回放：新接入的设备要单独补一份历史，而已经在看的设备不能
// 被重播一遍。
//
// 与 OnBinary 一样按**前缀**登记（取最长匹配）。精确名登记在终端场景下
// 等于永远不登记：会话是运行期创建的，而接线必须在开始服务前做完。
// 允许同时登记精确名与前缀，最长者优先 —— 这样别的模块可以为某一个
// 频道单独接管，不影响其余频道。
//
// 回调收到频道名：前缀登记后一个回调对应多个会话，不带频道名就分不了派。
func (h *Hub) OnJoin(prefix string, fn func(*Client, string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.joinCB[prefix] = fn
}

// OnBinary 注册入站二进制路由，按频道名前缀匹配（终端注册 "term:"）。
// 只有当该客户端确实订阅了这个频道才会投递，否则回 err —— 否则任何已登录
// 连接都能往它没订阅的会话里灌按键。
func (h *Hub) OnBinary(prefix string, fn func(*Client, string, []byte)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.binCB[prefix] = fn
}

// SendBinTo 向单个客户端投递二进制帧，前提是它仍订阅该频道。
// 订阅校验是必须的：join 回调与回放写入之间客户端可能已经退订/断开，
// 此时这一帧应该消失，而不是飘给一个不该收它的连接。
func (h *Hub) SendBinTo(c *Client, channel string, payload []byte) {
	h.mu.Lock()
	subscribed := h.clients[c][channel]
	h.mu.Unlock()
	if !subscribed {
		return
	}
	c.trySend(outItem{mt: websocket.BinaryMessage, payload: EncodeTermFrame(channel, payload)})
}

// routeBinary 找到频道对应的入站处理器；第二个返回值 false 表示应回 err。
func (h *Hub) routeBinary(c *Client, channel string, payload []byte) bool {
	h.mu.Lock()
	var fn func(*Client, string, []byte)
	best := -1
	for prefix, cb := range h.binCB {
		if strings.HasPrefix(channel, prefix) && len(prefix) > best {
			fn, best = cb, len(prefix)
		}
	}
	subscribed := h.clients[c][channel]
	h.mu.Unlock()

	if fn == nil || !subscribed {
		return false
	}
	fn(c, channel, payload)
	return true
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// 同源部署；跨源在开发期由 vite 代理消化。
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Handler 返回 /ws 的 handler。auth 返回 false 时以 401 拒绝握手。
func (h *Hub) Handler(auth func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth != nil && !auth(r) {
			http.Error(w, `{"code":"unauthorized","message":"未登录"}`, http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return // Upgrade 已写响应
		}
		c := &Client{hub: h, conn: conn, send: make(chan outItem, sendQueueSize)}
		h.register(c)
		go c.writePump()
		go c.readPump()
	})
}

// Broadcast 向频道所有订阅者投递 JSON 负载（data 帧，带全局 seq）。
func (h *Hub) Broadcast(channel string, payload []byte) {
	f := Frame{Ch: channel, T: "data", Seq: h.seq.Add(1), D: json.RawMessage(payload)}
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	h.dispatch(channel, outItem{mt: websocket.TextMessage, payload: b})
}

// BroadcastBin 向频道投递二进制帧（终端/服务日志原始字节）。
func (h *Hub) BroadcastBin(channel string, payload []byte) {
	h.dispatch(channel, outItem{mt: websocket.BinaryMessage, payload: EncodeTermFrame(channel, payload)})
}

func (h *Hub) dispatch(channel string, item outItem) {
	h.mu.Lock()
	subs := h.byCh[channel]
	targets := make([]*Client, 0, len(subs))
	for c := range subs {
		targets = append(targets, c)
	}
	h.mu.Unlock()

	for _, c := range targets {
		c.trySend(item)
	}
}

// SubscriberCount 返回频道订阅数。
func (h *Hub) SubscriberCount(channel string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.byCh[channel])
}

// ClientCount 返回在线连接数。
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// ---------- 内部 ----------

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = map[string]bool{}
}

// subscribe/unsubscribe 返回该频道的新订阅数（无回调时为 -1 之外的实际值）。
func (h *Hub) subscribe(c *Client, ch string) {
	h.mu.Lock()
	subs, ok := h.clients[c]
	if !ok || subs[ch] {
		h.mu.Unlock()
		return
	}
	subs[ch] = true
	if h.byCh[ch] == nil {
		h.byCh[ch] = map[*Client]bool{}
	}
	h.byCh[ch][c] = true
	n := len(h.byCh[ch])
	cb := h.countCB[ch]
	join := h.matchJoinLocked(ch)
	h.mu.Unlock()
	if cb != nil {
		cb(n)
	}
	if join != nil {
		join(c, ch)
	}
}

// matchJoinLocked 取最长前缀匹配的 join 回调。
func (h *Hub) matchJoinLocked(ch string) func(*Client, string) {
	var fn func(*Client, string)
	best := -1
	for prefix, cb := range h.joinCB {
		if strings.HasPrefix(ch, prefix) && len(prefix) > best {
			fn, best = cb, len(prefix)
		}
	}
	return fn
}

func (h *Hub) unsubscribe(c *Client, ch string) {
	h.mu.Lock()
	h.dropSubLocked(c, ch)
	h.mu.Unlock()
}

// dropSubLocked 删除订阅并在计数跨越时触发回调（回调在解锁后统一调用）。
func (h *Hub) dropSubLocked(c *Client, ch string) []func() {
	subs := h.clients[c]
	if subs == nil || !subs[ch] {
		return nil
	}
	delete(subs, ch)
	n := 0
	if m := h.byCh[ch]; m != nil {
		delete(m, c)
		n = len(m)
		if n == 0 {
			delete(h.byCh, ch)
		}
	}
	if cb := h.countCB[ch]; cb != nil {
		return []func(){func() { cb(n) }}
	}
	return nil
}

func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	var cbs []func()
	for ch := range h.clients[c] {
		cbs = append(cbs, h.dropSubLocked(c, ch)...)
	}
	delete(h.clients, c)
	h.mu.Unlock()
	c.closeOnce()
	for _, cb := range cbs {
		cb()
	}
}

// trySend 非阻塞投递：满缓冲即丢帧，连续丢弃过多则踢掉该客户端。
// 这是"慢客户端丢弃而非阻塞全 hub"的实现点。
func (c *Client) trySend(item outItem) {
	select {
	case c.send <- item:
		c.drops.Store(0)
	default:
		if c.drops.Add(1) > dropLimit {
			c.closeOnce() // 读泵会随之退出并做订阅回收
		}
	}
}

func (c *Client) closeOnce() {
	if c.closed.CompareAndSwap(false, true) {
		_ = c.conn.Close()
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.remove(c) // defer 保证异常断开也回收计数（D6 的泄漏防线）
	}()
	c.conn.SetReadLimit(1 << 20)
	_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	})
	for {
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			return // 断线/超时：defer 回收
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		if mt == websocket.BinaryMessage {
			ch, payload, err := DecodeTermFrame(data)
			if err != nil || !c.hub.routeBinary(c, ch, payload) {
				c.sendErr(ch, "该频道不接受二进制帧")
			}
			continue
		}
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			c.sendErr("", "控制帧不是合法 JSON")
			continue
		}
		switch f.T {
		case "sub":
			if f.Ch == "" {
				c.sendErr("", "sub 帧缺少 ch")
				continue
			}
			c.hub.subscribe(c, f.Ch)
		case "unsub":
			c.hub.unsubscribe(c, f.Ch)
		default:
			c.sendErr(f.Ch, "不支持的帧类型: "+f.T)
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.closeOnce()
	}()
	for {
		select {
		case item, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok { // hub 主动关闭
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(item.mt, item.payload); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) sendErr(ch, msg string) {
	b, _ := json.Marshal(Frame{Ch: ch, T: "err", D: json.RawMessage(`{"message":` + quote(msg) + `}`)})
	c.trySend(outItem{mt: websocket.TextMessage, payload: b})
}

// quote 是 json.Marshal(string) 的最小包装，避免手工拼 JSON。
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
