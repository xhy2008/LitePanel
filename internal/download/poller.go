package download

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// 轮询器：进度与速度的唯一来源。
//
// 它存在的理由是实测事实：**aria2 没有进度事件**（1.37.0 的通知全集只有
// start/pause/stop/complete/error/btComplete；把 12MB 限速拖成 24s 的下载
// 全程只推了一条 start，见 dev/aria2probe）。设计与早期计划里"速度与进度是
// aria2 主动推的、面板不轮询"是错的，进度条只能靠面板自己拿。
//
// 既然必须轮询，就不能把设计承诺的"面板开销极低"变成空话，因此三条硬约束
// 都有测试钉住：
//   - **只在有活动任务时跑**，任务清空即停；空闲时零请求。
//   - 每 tick 只发一次 tellActive（一把拿全部），不逐任务 tellStatus。
//   - 只在状态**变化**时发 Event；每 tick 的数字走 OnProgress 快照，
//     不混进事件流（否则前端每帧都把"有进度"当成"状态变了"去重排列表）。

// Progress 是一条任务的进度快照。
//
// aria2 把这些字段全以字符串返回。在边界上转一次数字、失败当 0，比让每个
// 消费方各写一遍 strconv 更省事：速度、剩余时间、百分比本来就要算。
type Progress struct {
	GID         string `json:"gid"`
	TotalBytes  int64  `json:"total_bytes"`
	DoneBytes   int64  `json:"done_bytes"`
	Speed       int64  `json:"speed"`
	Connections int    `json:"connections"`
}

// listFunc 拿当前活动任务（tellActive）。
type listFunc func(context.Context) ([]Status, error)

// PollerOptions 是轮询器的依赖与节奏。
type PollerOptions struct {
	// Interval 是轮询间隔。默认 1s：再快对 HDD 上的下载没有信息增益（aria2
	// 自己的速度统计也是秒级窗口），再慢则速度条看起来一卡一卡。
	Interval time.Duration
	// OnProgress 收到本 tick 的进度快照（可能为空，表示"该清空了"）。
	OnProgress func([]Progress)
}

// Poller 按需轮询活动任务的进度。可并发使用。
type Poller struct {
	list  listFunc
	final lookupFunc
	n     notifier
	opt   PollerOptions

	mu      sync.Mutex
	closed  bool
	polling bool
	// known 记录每个 gid 上一次上报过的状态，用来去重与**阻止倒退**。
	known map[string]EventKind
	// lastActive 是上一轮 tellActive 里的 gid 集合，用来发现"消失了"的任务。
	lastActive map[string]bool
	wake       chan struct{}
	done       chan struct{}
}

// NewPoller 建轮询器。final 用于把"从 active 里消失"的 gid 解析成终态；
// 可为 nil（那只能报"消失"而报不出原因）。
func NewPoller(list listFunc, final lookupFunc, n notifier, opt PollerOptions) *Poller {
	if opt.Interval <= 0 {
		opt.Interval = time.Second
	}
	return &Poller{
		list:       list,
		final:      final,
		n:          n,
		opt:        opt,
		known:      map[string]EventKind{},
		lastActive: map[string]bool{},
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
}

// Wake 让空闲的轮询器重新开始跑。
//
// 必须由事件桥的 started 事件（或添加任务成功）调用：轮询器自己发现不了
// "有新任务了"，因为它空闲时根本不发请求 —— 而那正是它的设计要点。
func (p *Poller) Wake() {
	select {
	case p.wake <- struct{}{}:
	default: // 已经排了一次唤醒，不必叠加
	}
}

// NoteState 让事件桥告诉轮询器"这条的权威状态已经是 X 了"。
//
// 两个来源的顺序没有保证：aria2 可能在"发出 complete 通知"与"下一轮
// tellActive 快照"之间有窗口期。不接受这个提示的话，已完成的卡片会被随后
// 那帧 active 快照倒回成进度条，再下一轮又跳回完成 —— 用户看到任务在两个
// 状态之间反复抖。
func (p *Poller) NoteState(gid string, kind EventKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if kind == EventStart {
		// 重新开始下载（例如重试）要清掉旧的终态标记，否则这条永远被
		// 当成"已终结"而不再上报。
		delete(p.known, gid)
		p.markPollingLocked()
		return
	}
	p.known[gid] = kind
}

// IsPolling 报告当前是否在轮询（测试与设置页诊断用）。
func (p *Poller) IsPolling() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.polling
}

func (p *Poller) markPollingLocked() { p.polling = true }

// Close 停止轮询。幂等。
func (p *Poller) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	close(p.done)
}

func (p *Poller) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Run 跑轮询循环直到 Close。调用方负责起 goroutine。
func (p *Poller) Run(ctx context.Context) {
	interval := time.NewTicker(p.opt.Interval)
	defer interval.Stop()
	for {
		if p.isClosed() || ctx.Err() != nil {
			return
		}
		if !p.IsPolling() {
			// 空闲：不睡觉轮询，而是阻塞等唤醒。这里的 select 是"空闲时
			// 零请求"的实现要点 —— 写成 ticker + 判断会每秒白醒一次并
			// 让人误以为随时可能发请求。
			select {
			case <-p.wake:
				p.mu.Lock()
				p.polling = true
				p.mu.Unlock()
			case <-p.done:
				return
			case <-ctx.Done():
				return
			}
			continue
		}
		p.tick(ctx)
		// tick 期间可能收到唤醒（连续添加任务），不排空的话下一次只能等
		// 满一个间隔。
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			return
		case <-interval.C:
		case <-p.wake:
		}
	}
}

// tick 跑一轮：拿全量活动任务 → 发进度快照 → 解析消失的 gid。
func (p *Poller) tick(ctx context.Context) {
	items, err := p.list(ctx)
	if err != nil {
		if ctx.Err() == nil && !p.isClosed() {
			logf("轮询下载进度失败: %v", err)
		}
		if IsUnavailable(err) {
			// aria2 掉了：停止轮询，等事件桥重连后再 Wake。继续跑只会每秒
			// 撞一次死端口 —— 正是健康检查那边同样的"故障放大"问题。
			p.mu.Lock()
			p.polling = false
			p.mu.Unlock()
		}
		return
	}

	nowActive := map[string]bool{}
	var snaps []Progress
	for _, st := range items {
		p.mu.Lock()
		seen := p.known[st.GID]
		p.mu.Unlock()
		if p.isTerminal(seen) {
			// 已按权威状态报过完成/失败：这一轮即使 aria2 仍把它列在 active
			// 里，也不发事件、不发进度 —— 否则界面会在"完成"与"下载中"
			// 之间来回抖。
			continue
		}
		nowActive[st.GID] = true
		snaps = append(snaps, progressOf(st))
		if kind, ok := kindFromAria2Status(st.Status); ok && string(seen) != string(kind) {
			p.emit(st.GID, kind, "")
		}
	}

	// 上一轮还在、这一轮不见了的任务：必须问一次终态。aria2 里"下完了"与
	// "失败了"都表现为从 tellActive 消失，不问一句就分不清，而失败必须带原因。
	p.mu.Lock()
	gone := make([]string, 0, len(p.lastActive))
	for gid := range p.lastActive {
		if !nowActive[gid] && !p.isTerminal(p.known[gid]) {
			gone = append(gone, gid)
		}
	}
	p.mu.Unlock()
	for _, gid := range gone {
		p.resolveFinal(ctx, gid)
	}

	p.mu.Lock()
	p.lastActive = nowActive
	p.mu.Unlock()

	p.publishProgress(snaps)

	if len(nowActive) == 0 {
		// 全部结束：停轮询，并交一份空快照让前端清掉残留的进度条。
		p.mu.Lock()
		wasPolling := p.polling
		p.polling = false
		p.mu.Unlock()
		if wasPolling {
			p.publishProgress(nil)
		}
	}
}

func (p *Poller) isTerminal(k EventKind) bool {
	switch k {
	case EventComplete, EventError, EventStopped:
		return true
	}
	return false
}

// resolveFinal 把消失的 gid 解析成终态事件。
func (p *Poller) resolveFinal(ctx context.Context, gid string) {
	kind := EventStopped
	reason := ""
	if p.final != nil {
		st, err := p.final(ctx, gid)
		if err == nil && st != nil {
			if k, ok := kindFromAria2Status(st.Status); ok {
				kind = k
			}
			if kind == EventError {
				reason = reasonFromStatus(st)
			}
		}
	}
	// 先占位再发：事件桥可能在 resolveFinal 查状态的间隙里已经报过这条的
	// 终态，那就不该再发第二遍（终态只能有一条，重复会让前端弹两次通知）。
	//
	// 注意判的是「已经是终态」而不是「曾经报过」：任务从 active 消失时，
	// known 里几乎总是留着一个 started（上一轮发的），用“非空”作条件会把
	// 真正的 error/complete 一并挡掉 —— 那就是“下载失败但界面永不说它失败”，
	// 测试 TestDisappearedGidResolvedToFinalState 捕到的就是这个。
	// 占位与判断必须在同一个锁里：先查后写的话，两者之间收到一条事件就会双发。
	p.mu.Lock()
	if p.isTerminal(p.known[gid]) {
		p.mu.Unlock()
		return
	}
	p.known[gid] = kind
	n := p.n
	p.mu.Unlock()
	if n == nil {
		return
	}
	n(Event{Kind: kind, GID: gid, Error: reason, At: time.Now().Unix()})
}

func (p *Poller) emit(gid string, kind EventKind, reason string) {
	p.mu.Lock()
	p.known[gid] = kind
	p.mu.Unlock()
	if p.n == nil {
		return
	}
	p.n(Event{Kind: kind, GID: gid, Error: reason, At: time.Now().Unix()})
}

func (p *Poller) publishProgress(snaps []Progress) {
	if p.opt.OnProgress == nil {
		return
	}
	p.opt.OnProgress(snaps)
}

// progressOf 把 aria2 的字符串字段转成数字。解析失败当 0：aria2 在任务刚
// 建好时确实会回空串，为一个空串把整条进度丢掉不值得。
func progressOf(st Status) Progress {
	return Progress{
		GID:         st.GID,
		TotalBytes:  a2int(st.TotalLength),
		DoneBytes:   a2int(st.CompletedLength),
		Speed:       a2int(st.DownloadSpeed),
		Connections: int(a2int(st.Connections)),
	}
}

// A2int 解析 aria2 的字符串数字（空串/非法值一律 0，与所有字段解析一致）。
//
// 导出是为了装配层落终态时能复用**同一份**解析：手搓一份会在前缀 0、空串、
// 溢出这些边角上与轮询器行为不同，而那种差异只在真实数据上才暴露。
func A2int(s string) int64 { return a2int(s) }

func a2int(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
