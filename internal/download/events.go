package download

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"litepanel/internal/logx"
)

// 事件桥：aria2 的 WebSocket 通知 → 领域 Event。
//
// 为什么值得单独一层而不是在 handler 里现读现转：aria2 的通知有三个实测出来
// 的坑（见 events_test.go 顶部），每一个的失败形态都是"静默不工作"——
//   1. gid 在 params[0] 这个**对象**的 gid 键里（实测 1.37.0：
//      `{"method":"aria2.onDownloadStart","params":[{"gid":"..."}]}`）。
//      照 JSON-RPC 直觉取 params[0] 得到的是 map；取 params[0].params[0]
//      （早期设计写的）则根本不存在。取不到 gid 就是每条事件都被丢弃，
//      而通知没有 id、错误无处可回，界面只表现为"永远在下载中"。
//   2. **不存在 onDownloadProgress**。aria2 的通知全集只有 start / pause /
//      stop / complete / error / btComplete 六个（实测：把 12MB 限速拖成
//      24s，全程只收到一条 start）。所以速度和进度靠 Events 之外的轮询器
//      （poller.go）拿，本层只管"状态变了"这种必须立即反映的边沿。
//   3. onDownloadError **只带 gid，不带原因**。不补一次 tellStatus 就只能
//      显示"下载失败"而不说为什么。
//
// 上行接缝是 func(Event)（与 filemgr 的 JobNotifier 同一套做法）：领域包
// 不 import ws，序列化与频道名归 api 包。

// EventKind 是事件的语义分类。注意它是**领域**分类而不是 aria2 的方法名：
// onBtDownloadComplete 与 onDownloadComplete 都归 EventComplete —— 前端只需
// 要知道"完成了"，让它去区分 BT 与普通下载毫无收益（而漏接 btComplete 的
// 后果是磁力链接任务永远显示下载中）。
type EventKind string

const (
	EventStart    EventKind = "started"
	EventComplete EventKind = "completed"
	EventError    EventKind = "error"
	EventPaused   EventKind = "paused"
	EventStopped  EventKind = "stopped"
)

// Event 是一条下载状态变化。
type Event struct {
	Kind EventKind `json:"kind"`
	GID  string    `json:"gid"`
	// Error 是 aria2 给的失败原因原文（仅 EventError）。带原因而非只报
	// "失败"是硬性要求；查不到时是兜底文案，不会是空串。
	Error string `json:"error,omitempty"`
	// At 是面板收到事件的时刻（秒）。aria2 不提供事件时间戳。
	At int64 `json:"at"`
}

// 无原因时的兜底文案。空串会让前端只能显示"失败"两个字。
const unknownReason = "aria2 未提供失败原因（任务记录可能已被清理）"

// logf 统一日志出口。
//
// 事件桥的故障几乎都是静默的（解析失败、断流、查不到原因），没有日志就只能
// 靠"界面不对"反推。logx 在发布构建下编译为空操作，所以这里可以放开写；
// 但**只报一次性的状态变化**，循环体内每条报文都记会刷爆 journald。
func logf(format string, args ...any) { logx.Warn(format, args...) }

// notifier 是上行接缝。
type notifier func(Event)

// lookupFunc 按 gid 查状态（用来给 error 事件补原因）。
type lookupFunc func(context.Context, string) (*Status, error)

// syncFunc 拉全量状态（重连/首连后补齐）。
type syncFunc func(context.Context) ([]Status, error)

// EventBridgeOptions 是桥的依赖与节奏。
type EventBridgeOptions struct {
	// FullSync 拉一次全量任务状态。必填与否取决于部署：为 nil 时桥只做
	// 事件转发，重连后不补状态（那会留下"断线期间的完成永远看不到"的洞，
	// 所以生产上必须给）。
	FullSync syncFunc
	// Lookup 给 error 事件补原因；nil 时用兜底文案。
	Lookup lookupFunc
	// MinReconnect / MaxReconnect 是重连退避的两端。默认 1s → 60s：
	// 下限太低会在 aria2 启动期间猛撞，上限太高则 aria2 恢复后界面迟迟不回。
	MinReconnect time.Duration
	MaxReconnect time.Duration
}

// EventBridge 维持与 aria2 的 WebSocket 订阅，断线自动重连。
type EventBridge struct {
	url string
	n   notifier
	opt EventBridgeOptions

	dial *websocket.Dialer

	mu       sync.Mutex
	closed   bool
	conn     *websocket.Conn
	attempts int
	backoff  time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewEventBridge 建桥。不立即连接，由 Run 负责。
func NewEventBridge(wsURL string, n notifier, opt EventBridgeOptions) *EventBridge {
	if opt.MinReconnect <= 0 {
		opt.MinReconnect = time.Second
	}
	if opt.MaxReconnect < opt.MinReconnect {
		opt.MaxReconnect = 60 * time.Second
	}
	return &EventBridge{
		url:  wsURL,
		n:    n,
		opt:  opt,
		dial: websocket.DefaultDialer,
		// 初始 backoff 从 MinReconnect 起，每次失败翻倍。
		backoff: opt.MinReconnect,
		done:    make(chan struct{}),
	}
}

// Attempts 返回累计连接次数（测试用来观察退避是否生效）。
func (b *EventBridge) Attempts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// CurrentBackoff 返回当前退避时长。
func (b *EventBridge) CurrentBackoff() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.backoff
}

// Close 停止桥并断开连接。幂等；Run 会因此退出。
func (b *EventBridge) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	c := b.conn
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if c != nil {
		_ = c.Close()
	}
}

func (b *EventBridge) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// Run 循环连接直到 Close。调用方负责起 goroutine。
func (b *EventBridge) Run(ctx context.Context) {
	defer close(b.done)
	ctx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.cancel = cancel
	b.mu.Unlock()

	for {
		if b.isClosed() {
			return
		}
		// 阻塞时长的失败处理都收在下面：连上→serve；断开→按退避等待→再连。
		if err := b.session(ctx); err != nil {
			if ctx.Err() != nil || b.isClosed() {
				return
			}
		}
		if ctx.Err() != nil || b.isClosed() {
			return
		}
		wait := b.takeBackoff()
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// takeBackoff 返回本次等待时长并把退避翻倍（封顶 MaxReconnect）。
// 只在**连接失败/断开后**调用；连上时 resetBackoff 把它归零，否则一次偶发
// 抖动之后桥就永远按最大间隔重连，aria2 恢复后要等一分钟界面才回来。
func (b *EventBridge) takeBackoff() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	w := b.backoff
	if b.backoff *= 2; b.backoff > b.opt.MaxReconnect {
		b.backoff = b.opt.MaxReconnect
	}
	return w
}

func (b *EventBridge) resetBackoff() {
	b.mu.Lock()
	b.backoff = b.opt.MinReconnect
	b.mu.Unlock()
}

// session 建立一次连接并持续读，直到出错或关闭。
func (b *EventBridge) session(ctx context.Context) error {
	conn, _, err := b.dial.DialContext(ctx, b.url, nil)
	if err != nil {
		if ctx.Err() == nil && !b.isClosed() {
			logf("连接 aria2 事件流失败: %v", err)
		}
		return err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = conn.Close()
		return context.Canceled
	}
	b.conn = conn
	b.mu.Unlock()
	b.resetBackoff()
	defer func() {
		b.mu.Lock()
		if b.conn == conn {
			b.conn = nil
		}
		b.mu.Unlock()
		_ = conn.Close()
	}()

	// 读超时：aria2 空闲时可能几分钟不推任何东西，所以不能设短超时（会让
	// 桥在正常状态下反复重连）。这里不设读超时，靠 Close()/ctx 取消来退出：
	// ctx 取消时 goroutine 会卡在 ReadMessage —— 因此另起一个 goroutine 在
	// ctx 结束时关闭连接，把读调用顶回来。否则"面板优雅关闭"会挂在这个
	// ReadMessage 上直到进程被 SIGKILL。
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	// 首连/重连都要补一次全量：
	//   - 面板刚启动时 aria2 里的任务还在跑，界面得立刻反映；
	//   - 断线期间推的事件全丢了，其中最重要的是 complete —— 不补拉，那个
	//     已经下完的任务会永远显示"下载中"，而且再也等不到纠正它的事件。
	b.fullSync(ctx)

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() == nil && !b.isClosed() {
				logf("aria2 事件流断开: %v", err)
			}
			return err
		}
		b.handle(ctx, raw)
	}
}

func (b *EventBridge) fullSync(ctx context.Context) {
	if b.opt.FullSync == nil {
		return
	}
	items, err := b.opt.FullSync(ctx)
	if err != nil {
		if ctx.Err() == nil && !b.isClosed() {
			logf("补齐下载任务状态失败: %v", err)
		}
		return
	}
	for _, st := range items {
		if kind, ok := kindFromAria2Status(st.Status); ok {
			b.publish(Event{Kind: kind, GID: st.GID, Error: st.ErrorMessage, At: time.Now().Unix()})
		}
	}
}

// kindFromAria2Status 把 aria2 的 status 字符串映射成事件分类。
// 返回 ok=false 表示"不是一个需要通知的状态"。
func kindFromAria2Status(s string) (EventKind, bool) {
	switch s {
	case "complete":
		return EventComplete, true
	case "error":
		return EventError, true
	case "paused":
		return EventPaused, true
	case "active":
		return EventStart, true
	case "waiting":
		// 等待中不是一个"变化"：它既没开始也没结束。发一条 started 会让
		// 前端把排队任务画成正在下载（并发上限 5 时，第 6 个之后的任务
		// 全部看起来在跑，而速度明明是 0）。
		return "", false
	case "removed":
		return EventStopped, true
	}
	return "", false
}

// notification 是 aria2 推的 JSON-RPC 通知（**没有 id**）。
type notification struct {
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (b *EventBridge) handle(ctx context.Context, raw []byte) {
	var n notification
	if err := json.Unmarshal(raw, &n); err != nil {
		// 不是合法 JSON-RPC：可能是别的进程占了这个端口，也可能是 aria2
		// 升级后改了格式。跳过这一条而不是退出 —— 桥一死，下载页就永远
		// 定格在最后一帧，而这比"少一条事件"严重得多。
		logf("忽略无法解析的 aria2 报文: %v", err)
		return
	}
	if !strings.HasPrefix(n.Method, "aria2.") {
		return
	}
	kind, ok := kindFromMethod(n.Method)
	if !ok {
		// 未知通知（aria2 新增事件）。静默跳过，但不能当成错误刷日志：
		// 那是正常情况，刷屏会淹没真正的故障。
		return
	}
	gid := extractGID(n.Params)
	if gid == "" {
		logf("aria2 事件里没有 gid，跳过: %s", n.Method)
		return
	}
	ev := Event{Kind: kind, GID: gid, At: time.Now().Unix()}
	if kind == EventError {
		ev.Error = b.reasonFor(ctx, gid)
	}
	b.publish(ev)
}

// reasonFor 给错误事件补原因。
//
// 必须补：实测 onDownloadError 的载荷**只有 gid**。不查就得不到原因，
// 界面只能显示"下载失败"—— 用户明确反对过这种没有原因的错误提示。
// 查失败（gid 已被 removeDownloadResult 清掉等）也要给出兜底文案，
// 而不是让事件消失或留空。
func (b *EventBridge) reasonFor(ctx context.Context, gid string) string {
	if b.opt.Lookup == nil {
		return unknownReason
	}
	st, err := b.opt.Lookup(ctx, gid)
	if err != nil || st == nil {
		return unknownReason
	}
	return reasonFromStatus(st)
}

// reasonFromStatus 从状态行里拼出人看的失败原因。
//
// 与事件桥共用一份：两个来源（error 事件 / 轮询发现任务消失）都要给出原因，
// 文案不一致会让用户以为是两个不同的故障。永远不返空串 —— 空串会让前端
// 只能显示“失败”两个字。
func reasonFromStatus(st *Status) string {
	msg := strings.TrimSpace(st.ErrorMessage)
	if msg == "" {
		if st.ErrorCode != "" {
			return fmt.Sprintf("aria2 错误码 %s（无文字说明）", st.ErrorCode)
		}
		return unknownReason
	}
	if st.ErrorCode != "" {
		return fmt.Sprintf("%s (code %s)", msg, st.ErrorCode)
	}
	return msg
}

func (b *EventBridge) publish(e Event) {
	if b.n == nil {
		return
	}
	b.n(e)
}

func kindFromMethod(m string) (EventKind, bool) {
	switch m {
	case "aria2.onDownloadStart":
		return EventStart, true
	case "aria2.onDownloadComplete", "aria2.onBtDownloadComplete":
		// btComplete 归入完成：前端只需要"完成了"。漏接它的代价是磁力
		// 链接任务永远显示"下载中"（普通 complete 对 BT 不触发）。
		return EventComplete, true
	case "aria2.onDownloadError":
		return EventError, true
	case "aria2.onDownloadPause":
		return EventPaused, true
	case "aria2.onDownloadStop":
		return EventStopped, true
	}
	return "", false
}

// extractGID 从通知参数里取 gid。形状兼容按证据分级：
//
//	params[0] = {"gid":"0x.."}      ✅ 实测 1.37.0（dev/aria2probe 抓的原始报文）
//	params[0] = {"params":["0x.."]}  ⚠ 未实测，照 JSON-RPC 常见嵌套形状防御
//	params[0] = "0x.."              ⚠ 未实测，裸串防御
//
// 后两种没在真 aria2 上见过，保留的理由是失败代价不对称：多接一种形状只花
// 二十行；认错形状的代价是**每条事件都被静默丢弃**（通知没有 id、错误无处
// 反馈），界面只表现为“永远在下载中”。两种防御形状都有测试钉住，不是为了
// 证明它们存在，而是为了证明解析器真能处理它们。
func extractGID(params []json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	// 裸字符串。
	var s string
	if json.Unmarshal(params[0], &s) == nil && s != "" {
		return s
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(params[0], &obj) != nil {
		return ""
	}
	if g := gidString(obj["gid"]); g != "" {
		return g
	}
	// 嵌套形状：{"params":[gid, ...]}
	var inner []json.RawMessage
	if json.Unmarshal(obj["params"], &inner) == nil && len(inner) > 0 {
		return gidString(inner[0])
	}
	return ""
}

func gidString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// aria2 的某些字段可能带数字形态的 id；不是字符串就当没有。
	return ""
}
