package metrics

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Source 是一次采样的来源。抽成接口是为了让采集循环的启停逻辑能脱离 /proc
// 单独测试 —— D6 的正确性（无人看时开销必须归零）比任何单个指标的数值更关键，
// 必须能被快、稳、不依赖 root 地测出来。
//
// 返回的 warming 表示本轮数据不完整（典型是 CPU 还没有差分基线）。
type Source interface {
	Sample() (snap Snapshot, warming bool, err error)
}

// OptionalResetter 是可选接口：持有"跨停表会失真"的基线（CPU 差分）的
// Source 实现它，停表时被调用。
type OptionalResetter interface {
	Reset()
}

// Broadcaster 是对 hub 的最小依赖面。
type Broadcaster interface {
	Broadcast(channel string, payload []byte)
}

// Collector 按订阅计数启停采样（D6）。
//
// 关键不变量：
//   - 订阅数为 0 时不存在采集循环，也就完全不读 /proc；
//   - 无论多少标签页，同时最多一个循环（大家共享同一份数据）；
//   - 停表时释放快照并清掉差分基线。
type Collector struct {
	src      Source
	bc       Broadcaster
	interval time.Duration

	mu      sync.Mutex
	subs    int
	snap    *Snapshot
	seq     uint64
	stopCh  chan struct{}
	stopped bool

	// 按需采样（HTTP 首屏）的节流缓存。
	cached   *Snapshot
	cachedAt time.Time

	// liveLoops 是真实存活的循环数（进入 loop 时 +1，退出时 -1）。
	// 用它而不是"标志位"来验证不泄漏：标志位会被 stopLocked 直接改掉，
	// 那样的测试只是在断言我自己写的赋值，测不出 goroutine 泄漏。
	liveLoops atomic.Int64
}

func NewCollector(src Source, bc Broadcaster, interval time.Duration) *Collector {
	if interval <= 0 {
		interval = time.Second
	}
	return &Collector{src: src, bc: bc, interval: interval}
}

// SetSubscribers 由 hub 的 OnCount 回调驱动。可重入、幂等。
func (c *Collector) SetSubscribers(n int) {
	if n < 0 {
		n = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subs = n
	switch {
	case n == 0:
		c.stopLocked()
	case c.stopCh == nil && !c.stopped:
		ch := make(chan struct{})
		c.stopCh = ch
		go c.loop(ch)
	}
}

func (c *Collector) stopLocked() {
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	// 释放上一轮快照（含按需缓存）：长时间无人看时不该把数据一直握在内存里。
	c.snap = nil
	if r, ok := c.src.(OptionalResetter); ok {
		r.Reset()
	}
}

// Stop 永久停止采集（进程退出用）。
func (c *Collector) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	c.stopLocked()
}

// loop 是采集循环。它只依赖自己那个 stop 通道，因此停/起竞争时
// 旧循环退出不会影响新循环 —— 不靠任何共享标志位来判定"我还是不是当前循环"。
func (c *Collector) loop(stop chan struct{}) {
	c.liveLoops.Add(1)
	defer c.liveLoops.Add(-1)

	tick := time.NewTicker(c.interval)
	defer tick.Stop()

	c.tickOnce(stop)
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			c.tickOnce(stop)
		}
	}
}

func (c *Collector) tickOnce(stop chan struct{}) {
	// 仅为省掉一次无谓的 /proc 读取；正确性不靠它。
	// 真正防“已停表却写回快照”的是下面 tickOnce 末尾对 stopCh 的重查
	// （有测试直接卡住采样中途停表来验）。去掉这里全部测试仍绿，
	// 别误以为删了会出 bug，也别误加了依赖。
	select {
	case <-stop:
		return
	default:
	}

	snap, warming, err := c.sampleSafely()
	if err != nil {
		// 一轮失败不能终止循环：/proc 读失败（挂载点刚被卸载等）下一轮
		// 重试即可，否则仪表会永久停在最后一帧。
		return
	}

	c.mu.Lock()
	if c.stopCh == nil {
		// 采样期间已被停表：丢弃本轮，绝不把快照写回去。
		c.mu.Unlock()
		return
	}
	c.seq++
	snap.Seq = c.seq
	snap.TS = time.Now().Unix()
	snap.Warming = warming
	c.snap = &snap
	c.mu.Unlock()

	payload, err := json.Marshal(snap)
	if err != nil {
		return
	}
	c.broadcastSafely(payload)
}

// sampleSafely 兜住 Source 的 panic：单个指标解析炸了不该带走整个进程。
func (c *Collector) sampleSafely() (snap Snapshot, warming bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("metrics: 采样 panic: %v", r)
		}
	}()
	return c.src.Sample()
}

// broadcastSafely 兜住广播侧 panic：hub 内部出问题不该让采集循环消失，
// 否则仪表静静停在最后一帧，比直接崩掉更难排查。
func (c *Collector) broadcastSafely(payload []byte) {
	defer func() { _ = recover() }()
	c.bc.Broadcast(ChannelMetrics, payload)
}

// Latest 返回最近一次快照；无订阅或已停止时为 nil。
func (c *Collector) Latest() *Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == nil {
		return nil
	}
	cp := *c.snap
	return &cp
}

// liveLoopCount 供测试验证不泄漏 goroutine。
func (c *Collector) liveLoopCount() int { return int(c.liveLoops.Load()) }

// OnDemand 给 HTTP 首屏用：采集器停着（无 WS 订阅者）时也能拿到一份数据。
//
// 两条路径：
//   - 有实时快照（采集器在跑）：直接给，绝不另起采样。并发采样会互抢
//     CPU 差分基线，把 WS 帧的读数一起弄脏；
//   - 采集器停着：采一轮并按 interval 节流。
//
// 节流必须有：这个接口的语义是"给我一份数据"，不设上限的话任何客户端
// 轮询它都会变成对 /proc 的读放大 —— 一个自称轻量的面板，自己却成了
// 能把机器压忙的东西，跟设计初衷正好相反。
//
// 节流周期复用 interval 而不是另开一个参数：比一轮采集更快没有意义，
// 且会产生"HTTP 数据比 WS 帧更新"这种自相矛盾的读数。
func (c *Collector) OnDemand() (*Snapshot, error) {
	if s := c.Latest(); s != nil {
		return s, nil
	}

	c.mu.Lock()
	// 双重检查：取锁期间可能已经有 WS 订阅进来、采集器刚出了帧。
	if c.snap != nil {
		cp := *c.snap
		c.mu.Unlock()
		return &cp, nil
	}
	if c.cached != nil && time.Since(c.cachedAt) < c.interval {
		cp := *c.cached
		c.mu.Unlock()
		return &cp, nil
	}
	c.mu.Unlock()

	// 采样在锁外做：一次 /proc 遍历要几毫秒，持锁会把停表/起表
	// 这些 hub 回调一起堵住。
	snap, warming, err := c.src.Sample()
	if err != nil {
		return nil, err
	}
	snap.Seq = 0 // 不占用 WS 帧的序号空间：两个来源的 seq 混在一起会让前端误判丢帧
	snap.TS = time.Now().Unix()
	snap.Warming = warming

	c.mu.Lock()
	// 期间采集器若已跑起来，优先给实时数据，别让首屏拿到一份"更旧"的。
	if c.snap != nil {
		cp := *c.snap
		c.mu.Unlock()
		return &cp, nil
	}
	cp := snap
	c.cached = &cp
	c.cachedAt = time.Now()
	c.mu.Unlock()
	return &cp, nil
}
