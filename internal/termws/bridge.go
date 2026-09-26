// Package termws 把 terminal 的 tmux 控制会话桥接到 ws 频道（设计 7.2、15 节）。
//
// 形状：一个终端会话 ↔ 一条 tmux control 连接 ↔ 一个 `term:{id}` 频道 ↔ N 个
// 浏览器。为什么要中间这层，而不是每个浏览器各开一条 control 连接：实测普通
// attach 只有最后一个活动客户端能收到输出，而 control mode 的 %output 会广播
// 给所有控制连接 —— 面板侧维持一条连接再自己扇出，回放/限流/断线都不牵扯 tmux。
//
// 三条实测约束决定了这里的写法：
//
//   - 一个 pane 只有一套字符网格，做不到"每设备各画各的"（设计 7.2）。尺寸
//     跟着"最后一次按键的那个设备"走；纯观看的设备靠 xterm 软折行看全内容
//     （实测 120 列输出喂给 60 列设备，内容一字不丢）。
//   - control mode 的 attach 不补绘已有屏幕，回放必须自己攒字节流。
//   - 攒的是 %output 原始流而不是 capture-pane 结果：capture 的回放带按 pane
//     原宽计算的绝对定位，喂给窄设备会把已写内容覆盖掉（实测丢数据）；原始流
//     只含相对移动与 \r，折成几行都对。
package termws

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"litepanel/internal/logx"

	"litepanel/internal/terminal"
)

// MaxReplayBytes 是每会话回放缓冲的上限。
//
// 没有上限，一个跑 `yes` 或长编译的会话就能把 12GB 的机器吃穿。
// 超限保尾去头：重连的人最关心"刚才跑到哪了"。
const MaxReplayBytes = 256 * 1024

// attachTimeout 限制一次接管（fork tmux + 控制模式握手）的耗时。
// 没有它，tmux server 卡住会让这台设备的读泵永远停在这里。
const attachTimeout = 15 * time.Second

// Hub 是桥接需要的最小推送能力（真实现是 *ws.Hub）。
//
// 订阅者是不透明令牌：桥接只把它当 map key 与"最后按键设备"的记号，
// 不知道也不需要知道它是 *ws.Client。
type Hub interface {
	BroadcastBin(ch string, payload []byte)
	SendBinTo(sub any, ch string, payload []byte)
	OnJoin(prefix string, fn func(sub any, ch string))
	OnBinary(prefix string, fn func(sub any, ch string, payload []byte))
}

// Session 是桥接持有的一条 tmux 控制会话（实现者是 *terminal.Session）。
type Session interface {
	// Ready 在 attach 握手块结束时关闭。接管必须等它：见 Manager.adopt。
	Ready() <-chan struct{}
	Events() <-chan terminal.Event
	Exited() <-chan struct{}
	SendText(ctx context.Context, text string) error
	Resize(ctx context.Context, cols, rows int) error
	CaptureAll(ctx context.Context) (string, error)
	Close() error
	Name() string
}

// Manager 管着所有在线终端会话。
type Manager struct {
	hub    Hub
	bin    string
	prefix string // tmux 会话名前缀

	mu   sync.Mutex
	sess map[string]*entry // term id -> 会话
	// pending 是"正在被别的连接接管"的会话：两个标签页同时打开同一个
	// 会话时，只有第一个真去 attach，第二个等它（否则各开一条 control
	// 连接，同一份输出会被读两遍）。
	pending map[string]*attempt

	// 会话工厂：测试不需要它们（集成测试直接跑真 tmux），装配层可替换。
	userFn func(id string, o terminal.SessionOpts) (Session, error)
	attFn  func(id string) (Session, error)
	listFn func() ([]string, error)
}

type entry struct {
	sess Session

	// 回放缓冲：只存 %output 原始字节（原因见包注释）。因为存的字节与浏览器
	// 收到的完全同源，"回放 + 之后的实时流"在接缝处不会缺字符也不会重。
	buf []byte

	// 尺寸状态：每台设备上报过的尺寸 + 当前生效尺寸及其归属设备。
	size      map[any]dim
	lastInput dim // 当前生效尺寸
	owner     any // 这个尺寸是谁定的（nil = 还没人按过键）
}

// attempt 是一次进行中的接管，供并发等待者取结果。
type attempt struct {
	done chan struct{}
	err  error
}

type dim struct{ cols, rows int }

func (d dim) valid() bool { return d.cols > 0 && d.rows > 0 }

// NewManager 建管理器并注册上行路由。
func NewManager(hub Hub, bin string) *Manager {
	m := &Manager{
		hub:     hub,
		bin:     bin,
		prefix:  "lp-",
		sess:    map[string]*entry{},
		pending: map[string]*attempt{},
	}
	m.userFn = m.defaultCreate
	m.attFn = m.defaultAttach
	m.listFn = m.defaultList
	// 上行与回放钩子都只注册一次、都按前缀：终端会话是运行期出现的，
	// 而 hub 的接线必须在开始服务前做完。
	hub.OnBinary("term:", m.onBinary)
	hub.OnJoin("term:", m.onJoin)
	return m
}

// Channel 是终端会话对应的 ws 频道名。
func Channel(id string) string { return "term:" + id }

// Create 新建终端会话并开始扇出。
func (m *Manager) Create(ctx context.Context, id string, o terminal.SessionOpts) error {
	sess, err := m.openNew(id, o)
	if err != nil {
		return err
	}
	return m.adopt(ctx, id, sess)
}

func (m *Manager) openNew(id string, o terminal.SessionOpts) (Session, error) {
	m.mu.Lock()
	_, exists := m.sess[id]
	m.mu.Unlock()
	if exists {
		return nil, fmt.Errorf("终端会话 %s 已存在", id)
	}
	return m.userFn(id, o)
}

// Ensure 接管一个面板还没接管的会话（浏览器点开、或启动对账之后 tmux 里
// 新出现的）。
//
// 为什么需要它：面板只在启动时对账一次，而用户完全可能在 tmux 里自己
// `new-session -s lp-42`。没有按需接管，表现是"侧栏列出了这个会话，点进去
// 一片空白、敲键没反应"，而且不报任何错。
//
// 并发去重是必须的：同一会话开两个标签页是常态，各 attach 一条 control
// 连接不会报错，只会让同一份输出成倍重复。
func (m *Manager) Ensure(ctx context.Context, id string) error {
	m.mu.Lock()
	if _, ok := m.sess[id]; ok {
		m.mu.Unlock()
		return nil
	}
	if a, ok := m.pending[id]; ok {
		m.mu.Unlock()
		select {
		case <-a.done:
			return a.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a := &attempt{done: make(chan struct{})}
	m.pending[id] = a
	m.mu.Unlock()

	a.err = m.attachNow(ctx, id)

	m.mu.Lock()
	delete(m.pending, id)
	m.mu.Unlock()
	close(a.done)
	return a.err
}

func (m *Manager) attachNow(ctx context.Context, id string) error {
	sess, err := m.attFn(id)
	if err != nil {
		return err
	}
	return m.adopt(ctx, id, sess)
}

// adopt 接管一个已打开的会话：等握手完成，再登记并起扇出泵。
//
// 必须等 Ready。`tmux -CC attach` 是个子进程：Attach 返回时进程刚 fork 出来，
// tmux 服务端还没把这个 client 注册上。不等就返回，“接管成功”只是我们
// map 里记了一笔，而 tmux 那边此刻还没有这条连接。后果不是测试红一个数字，
// 而是紧跟着发的 resize / 按键落进握手的空窗里 —— 控制模式**不回放历史**，
// 这些字节就永久丢了（面板启动对账后头一次打开会话时的尺寸错位就是这么来的）。
//
// 失败路径不会卡死：attach 直接失败时 Ready 也会关闭（见 terminal.Session），
// 调用方随后从 Exited / 事件流分辨原因。
func (m *Manager) adopt(ctx context.Context, id string, sess Session) error {
	select {
	case <-sess.Ready():
	case <-ctx.Done():
		_ = sess.Close()
		return fmt.Errorf("等终端 %s 握手: %w", id, ctx.Err())
	}
	e := &entry{sess: sess, size: map[any]dim{}}
	m.mu.Lock()
	if _, dup := m.sess[id]; dup {
		m.mu.Unlock()
		_ = sess.Close()
		return fmt.Errorf("终端会话 %s 已存在", id)
	}
	m.sess[id] = e
	m.mu.Unlock()

	go m.pump(id, e)
	return nil
}

// Reconcile 在面板启动时接管 tmux 里已有的 lp-* 会话（D5：面板重启终端不死）。
// 返回接管的会话 id 列表。
func (m *Manager) Reconcile(ctx context.Context) ([]string, error) {
	names, err := m.listFn()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		id := strings.TrimPrefix(name, m.prefix)
		if id == "" || id == name && !strings.HasPrefix(name, m.prefix) {
			continue
		}
		m.mu.Lock()
		_, have := m.sess[id]
		m.mu.Unlock()
		if have {
			continue
		}
		sess, err := m.attFn(id)
		if err != nil {
			// 单个会话接不上不该拖垮整个启动流程；但要留话：静默跳过正是
			// “侧栏列出了这个会话、点进去一片空白”的唯一成因。
			logx.Info("接管终端会话 %s 失败: %v", id, err)
			continue
		}
		if err := m.adopt(ctx, id, sess); err != nil {
			logx.Info("接管终端会话 %s 失败: %v", id, err)
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// pump 是 Events 的唯一消费者：扇出 + 攒回放 + 退出清理。
//
// 单消费者是硬约束：terminal.Session.Events 是单消费者 channel，多一个读取方
// 就会把输出随机劈成两半（写探针时踩过：一边收一边等，等待方永远收不到）。
func (m *Manager) pump(id string, e *entry) {
	ch := Channel(id)
	for {
		select {
		case ev, ok := <-e.sess.Events():
			if !ok {
				m.retire(id)
				return
			}
			m.fanout(e, ch, ev)
		case <-e.sess.Exited():
			m.drain(e, ch) // Exited 关闭时可能还有已解码未派发的输出
			m.retire(id)
			return
		}
	}
}

func (m *Manager) fanout(e *entry, ch string, ev terminal.Event) {
	if ev.Kind != terminal.EvOutput {
		return // %window-add 之类对前端没意义
	}
	m.hub.BroadcastBin(ch, []byte(ev.Data))
	m.appendBuf(e, ev.Data)
}

func (m *Manager) drain(e *entry, ch string) {
	for {
		select {
		case ev, ok := <-e.sess.Events():
			if !ok {
				return
			}
			m.fanout(e, ch, ev)
		default:
			return
		}
	}
}

func (m *Manager) appendBuf(e *entry, data string) {
	m.mu.Lock()
	e.buf = append(e.buf, data...)
	if over := len(e.buf) - MaxReplayBytes; over > 0 {
		// 保尾去头。截在多字节字符中间也无妨：xterm 对残缺前缀是丢弃处理，
		// 紧随其后的实时流会重写那一行。
		e.buf = e.buf[over:]
	}
	m.mu.Unlock()
}

// retire 在 tmux 侧会话消失后释放桥接侧资源。
//
// 这里**不**杀 tmux 会话：断开 control 连接等价于 detach，shell 继续跑，
// 这正是"关浏览器再进来还在"的机制。真删除只走 Delete（用户在面板点删除）。
func (m *Manager) retire(id string) {
	m.mu.Lock()
	e, ok := m.sess[id]
	if ok {
		delete(m.sess, id)
	}
	m.mu.Unlock()
	if ok {
		_ = e.sess.Close()
	}
}

// Delete 真的删掉 tmux 会话（面板点"删除"）。
func (m *Manager) Delete(id string) error {
	m.retire(id)
	return terminal.KillSession(m.bin, m.prefix+id)
}

// Close 释放所有控制连接（面板退出时）。tmux 会话继续存活。
func (m *Manager) Close() {
	m.mu.Lock()
	all := make([]*entry, 0, len(m.sess))
	for id, e := range m.sess {
		all = append(all, e)
		delete(m.sess, id)
	}
	m.mu.Unlock()
	for _, e := range all {
		_ = e.sess.Close()
	}
}

// onBinary 处理上行按键与尺寸上报。
// onJoin：设备订阅某个会话时，先确保面板接管了它，再给它补一份历史。
//
// 接管必须在回放之前：会话可能刚由 REST 建好（或对账之后用户在 tmux 里
// 自己新建），还没人读它的输出；没接管就回放，能回放的只有空缓冲，
// 表现是"点进去一片空白、敲键盘没反应"且全程无错误。
//
// 回放只补这一份：广播会把正在看的设备整屏重播一遍。
//
// 同步执行是刻意的：hub 在客户端自己的读泵里调它，阻塞的只是这台设备，
// 换来的是"回放一定发生在接管之后、且发生在任何后续按键之前"。
// 异步化会引入"按键先于回放到达"的乱序，那比多等一秒糟得多。
func (m *Manager) onJoin(sub any, ch string) {
	id, ok := strings.CutPrefix(ch, "term:")
	if !ok || id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachTimeout)
	defer cancel()
	if err := m.Ensure(ctx, id); err != nil {
		return // 会话不存在/接不上：不回放进一个不存在的会话
	}
	m.ReplayFor(id, sub)
}

func (m *Manager) onBinary(sub any, ch string, payload []byte) {
	id := strings.TrimPrefix(ch, "term:")
	in, err := ParseInbound(payload)
	if err != nil {
		return
	}

	m.mu.Lock()
	e, ok := m.sess[id]
	m.mu.Unlock()
	if !ok {
		return // 未知会话：丢弃，不 panic，也不影响别的会话
	}

	if in.IsResize {
		d := dim{in.Cols, in.Rows}
		if !d.valid() {
			return
		}
		owner := func() any {
			m.mu.Lock()
			defer m.mu.Unlock()
			e.size[sub] = d
			// 只有"还没有主人"或"上报者就是主人"时才改 pane。
			// 否则手机 merely 打开页面（xterm fit 会自动上报尺寸）就把
			// 桌面上正在跑的程序压成 12 行。
			if e.owner == nil || e.owner == sub {
				e.owner, e.lastInput = sub, d
				return sub
			}
			return nil
		}()
		if owner != nil {
			m.resize(e, d)
		}
		return
	}

	// 按键：这台设备成为尺寸主人（"最后按键的设备赢"，见包注释）。
	d, need := func() (dim, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		e.owner = sub
		d := e.size[sub]
		if d.valid() && d != e.lastInput {
			e.lastInput = d
			return d, true
		}
		return dim{}, false
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.sess.SendText(ctx, string(in.Keys)); err != nil {
		return
	}
	if need {
		m.resize(e, d)
	}
}

// resize 把尺寸报给 tmux（refresh-client -C）。调用方负责只在"真的变了"
// 时才调：尺寸未变时 tmux 视为 no-op，但面板也不该每次 fit 都发控制命令。
func (m *Manager) resize(e *entry, want dim) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = e.sess.Resize(ctx, want.cols, want.rows)
}

// ClientGone 在某设备的所有连接断开时调用。
//
// 刻意什么都不做：终端会话属于 tmux，不属于任何设备。在这里"顺手清理"
// 就会让"关浏览器再进来"失效。
func (m *Manager) ClientGone(sub any) {}

// ReplayFor 把缓冲的字节流单点投递给刚接入的订阅者。
//
// 缓冲为空时退回 capture-pane：面板重启后内存流是空的，而 tmux 里的 shell
// 还带着完整历史，这时 capture 是唯一来源。capture 按 pane 原宽排版，窄设备
// 上不如原始流理想，但"看得到历史"比"重排完美"重要。
func (m *Manager) ReplayFor(id string, sub any) {
	m.mu.Lock()
	e, ok := m.sess[id]
	var snap []byte
	if ok {
		snap = append([]byte(nil), e.buf...)
	}
	m.mu.Unlock()
	if !ok {
		return
	}

	ch := Channel(id)
	if len(snap) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cap2, err := e.sess.CaptureAll(ctx); err == nil && cap2 != "" {
			m.hub.SendBinTo(sub, ch, []byte(cap2))
		}
		return
	}
	m.hub.SendBinTo(sub, ch, snap)
}

// Sessions 返回当前桥接中的会话 id（诊断用）。
func (m *Manager) Sessions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.sess))
	for id := range m.sess {
		out = append(out, id)
	}
	return out
}

// ---- 默认实现：真 tmux ----

func (m *Manager) defaultCreate(id string, o terminal.SessionOpts) (Session, error) {
	return terminal.CreateSession(m.bin, m.prefix+id, o)
}

func (m *Manager) defaultAttach(id string) (Session, error) {
	return terminal.Attach(m.bin, m.prefix+id)
}

func (m *Manager) defaultList() ([]string, error) {
	return terminal.ListSessions(m.bin, m.prefix)
}
