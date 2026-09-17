package metrics

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 采集器按 D6 停着（没有 WS 订阅者）时，HTTP 首屏也必须能拿到数据。
// 否则打开面板会先看到一片 --，只能干等 WS 连上后的第一帧。
func TestOnDemandSamplesWhenCollectorIdle(t *testing.T) {
	src := &fakeSource{warmingFor: 1}
	c := newTestCollector(t, src, &fakeBC{})

	snap, err := c.OnDemand()
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil {
		t.Fatal("无订阅时也必须给出一份快照")
	}
	if src.Calls() != 1 {
		t.Errorf("应真实采样 1 次, got %d", src.Calls())
	}
	if snap.TS == 0 {
		t.Error("必须带时间戳：否则陈旧快照和刚采的在协议上无法区分")
	}
}

// 采集器在跑时绝不另起采样：并发采样会互抢 CPU 差分基线，
// 把 WS 帧的读数一起弄脏。
func TestOnDemandUsesLiveSnapshot(t *testing.T) {
	src := &fakeSource{}
	bc := &fakeBC{}
	c := newTestCollector(t, src, bc)
	c.SetSubscribers(1)
	waitFor(t, "有实时快照", func() bool { return c.Latest() != nil })

	before := src.Calls()
	snap, err := c.OnDemand()
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil {
		t.Fatal("应返回实时快照")
	}
	if after := src.Calls(); after != before {
		t.Errorf("有实时快照时不该再采样: %d → %d", before, after)
	}
}

// 按需采样必须节流：这个接口的语义是"给我一份数据"，不设上限的话
// 任何客户端轮询都会变成对 /proc 的读放大 —— 一个自称轻量的面板，
// 自己却成了能把机器压忙的东西。
func TestOnDemandIsThrottled(t *testing.T) {
	src := &fakeSource{}
	c := NewCollector(src, &fakeBC{}, time.Hour) // 一小时只许采一次
	defer c.Stop()

	for i := 0; i < 10; i++ {
		if _, err := c.OnDemand(); err != nil {
			t.Fatal(err)
		}
	}
	if n := src.Calls(); n != 1 {
		t.Errorf("10 次调用应只真实采样 1 次, got %d", n)
	}
}

// 节流期内必须返回同一份内容（字段完整），而不是空壳。
// 前端拿空壳会画 0%；给陈旧但完整的数据至多是晚了几秒。
func TestOnDemandThrottledBodyConsistent(t *testing.T) {
	src := &fakeSource{last: Snapshot{Mem: &MemStat{Total: 42, Used: 21, Percent: 50}}}
	c := NewCollector(src, &fakeBC{}, time.Hour)
	defer c.Stop()

	first, err := c.OnDemand()
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.OnDemand()
	if err != nil {
		t.Fatal(err)
	}
	if first.Mem == nil || first.Mem.Total != 42 {
		t.Fatalf("首次内容不对: %+v", first.Mem)
	}
	if second.Mem == nil || second.Mem.Total != 42 {
		t.Errorf("节流期内应复用同一份: %+v", second.Mem)
	}
}

// 节流必须按时间真的放行，而不是"取到缓存就一直用"。
// 后者会让面板永远显示首屏那一刻的数据，看起来"在跑"其实纹丝不动，
// 比直接报错更难被发现。
func TestOnDemandThrottleExpires(t *testing.T) {
	src := &fakeSource{}
	c := NewCollector(src, &fakeBC{}, 20*time.Millisecond)
	defer c.Stop()

	// 必须持续调用：节流是"到期后才放行下一次"，光等不调用永远不会采。
	c.OnDemand()
	waitFor(t, "节流窗口过期后放行", func() bool {
		c.OnDemand()
		return src.Calls() >= 2
	})
}

// 按需快照不得占用 WS 帧的序号空间：两个来源的 seq 混在一起，
// 前端会据此误判"丢帧"或"重复帧"。
func TestOnDemandDoesNotConsumeSeq(t *testing.T) {
	src := &fakeSource{}
	c := newTestCollector(t, src, &fakeBC{})

	s1, _ := c.OnDemand()
	s2, _ := c.OnDemand()
	if s1.Seq != 0 || s2.Seq != 0 {
		t.Errorf("按需快照 seq 应为 0, got %d / %d", s1.Seq, s2.Seq)
	}
}

// 采样失败必须把错误交给调用方，让它回 500。
// 返回一份空快照会被前端渲染成"各项 0%"，看起来像机器空闲，
// 比一个明确的错误危险得多。
func TestOnDemandPropagatesError(t *testing.T) {
	src := &failingSource{}
	c := newTestCollector(t, src, &fakeBC{})

	snap, err := c.OnDemand()
	if err == nil {
		t.Fatal("应返回错误")
	}
	if snap != nil {
		t.Error("出错时不该给出快照")
	}
	if !errors.Is(err, errOnDemand) {
		t.Errorf("错误应可被上层判别, got %v", err)
	}
}

// 失败不得污染节流缓存：否则一次偶发的 /proc 读失败会让面板
// 在整个节流周期里持续报错，而重试本可以成功。
func TestOnDemandErrorNotCached(t *testing.T) {
	src := &flakyOnDemandSource{failFirst: 1}
	c := NewCollector(src, &fakeBC{}, time.Hour)
	defer c.Stop()

	if _, err := c.OnDemand(); err == nil {
		t.Fatal("首次应失败")
	}
	if _, err := c.OnDemand(); err != nil {
		t.Errorf("失败不该被缓存，重试应成功: %v", err)
	}
}

// 停表时必须连按需缓存一起丢掉。
// 只清实时快照的话，重启后仍可能把停表前很久的陈旧数据再供出去，
// 而面板上完全看不出这份数据有多旧。
func TestOnDemandCacheClearedOnStop(t *testing.T) {
	src := &fakeSource{}
	c := NewCollector(src, &fakeBC{}, 5*time.Millisecond)
	defer c.Stop()

	c.SetSubscribers(1)
	waitFor(t, "有实时快照", func() bool { return c.Latest() != nil })
	c.SetSubscribers(0)

	before := src.Calls()
	snap, err := c.OnDemand()
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil {
		t.Fatal("应重新采样而非复用陈旧缓存")
	}
	if src.Calls() <= before {
		t.Error("停表后应重新采样，不能供停表前的缓存")
	}
}

// 按需路径不能把采集器的启停堵住：采样要几毫秒，
// 若持锁做，hub 的订阅回调会一起被卡住（取消订阅都要等一次 /proc 遍历）。
func TestOnDemandDoesNotBlockSubscriberChanges(t *testing.T) {
	src := &slowSource2{delay: 40 * time.Millisecond}
	c := NewCollector(src, &fakeBC{}, time.Hour)
	defer c.Stop()

	done := make(chan struct{})
	go func() { c.OnDemand(); close(done) }()

	waitFor(t, "进入采样", func() bool { return src.entered.Load() })

	start := time.Now()
	c.SetSubscribers(1) // 不该被按需采样堵住
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Errorf("订阅回调被按需采样堵住: %v", d)
	}
	<-done
}

// 并发按需调用必须安全：无 panic、无死锁、每个调用都拿到完整快照。
//
// 刻意不断言"只采一次"。一次 /proc 遍历约 0.1–0.5ms，8 个标签页同时开首屏
// 共几毫秒，为它加锁等待是过度设计；真正要防的是串行轮询的读放大，
// 那由 interval 节流负责（见 TestOnDemandIsThrottled）。
func TestOnDemandConcurrentIsSafe(t *testing.T) {
	src := &slowSource2{delay: 10 * time.Millisecond}
	c := NewCollector(src, &fakeBC{}, time.Hour)
	defer c.Stop()

	var wg sync.WaitGroup
	bad := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := c.OnDemand()
			if err != nil {
				bad <- "出错: " + err.Error()
				return
			}
			if snap == nil || snap.Mem == nil {
				bad <- "拿到空快照"
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("并发调用卡住了")
	}
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}
}

var errOnDemand = errors.New("按需采样失败")

type failingSource struct{}

func (failingSource) Sample() (Snapshot, bool, error) { return Snapshot{}, false, errOnDemand }

type flakyOnDemandSource struct {
	mu        sync.Mutex
	calls     int
	failFirst int
}

func (s *flakyOnDemandSource) Sample() (Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.failFirst {
		return Snapshot{}, false, errOnDemand
	}
	return Snapshot{Mem: &MemStat{Total: 1}}, false, nil
}

// slowSource2 记录"已进入采样"，让测试能精确对齐并发窗口。
type slowSource2 struct {
	delay   time.Duration
	entered atomic.Bool
	calls   atomic.Int64
}

func (s *slowSource2) Sample() (Snapshot, bool, error) {
	s.calls.Add(1)
	s.entered.Store(true)
	time.Sleep(s.delay)
	return Snapshot{Mem: &MemStat{Total: 7}}, false, nil
}
