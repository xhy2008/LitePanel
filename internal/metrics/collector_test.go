package metrics

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource 记录被调用次数，并按调用序号决定是否为「首个采样」。
type fakeSource struct {
	mu    sync.Mutex
	calls int
	// 前 N 次返回 warming（模拟 CPU 还没有差分基线）。
	warmingFor int
	last       Snapshot
}

func (f *fakeSource) Sample() (Snapshot, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	warming := f.calls <= f.warmingFor
	s := f.last
	s.Seq = uint64(f.calls)
	s.Warming = warming
	return s, warming, nil
}

func (f *fakeSource) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeBC struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (b *fakeBC) Broadcast(channel string, payload []byte) {
	if channel != ChannelMetrics {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := append([]byte(nil), payload...)
	b.payloads = append(b.payloads, cp)
}

func (b *fakeBC) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.payloads)
}

func newTestCollector(t *testing.T, src Source, bc Broadcaster) *Collector {
	t.Helper()
	c := NewCollector(src, bc, 10*time.Millisecond)
	t.Cleanup(c.Stop)
	return c
}

// 等条件成立，避免用固定 sleep 造成不稳定测试。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("超时等待: %s", what)
}

// D6 的核心断言：没有订阅者时绝不采样。
// 这是整台机器"无人观看时面板开销归零"的唯一保证。
func TestCollectorStoppedWhenNoSubscribers(t *testing.T) {
	src := &fakeSource{}
	c := newTestCollector(t, src, &fakeBC{})
	c.SetSubscribers(0) // 显式一次，确认"0 订阅"不意外启动循环

	time.Sleep(60 * time.Millisecond)
	if n := src.Calls(); n != 0 {
		t.Fatalf("0 订阅时不该采样, 却采了 %d 次", n)
	}
}

func TestCollectorStartsOnFirstSubscriber(t *testing.T) {
	src := &fakeSource{}
	c := newTestCollector(t, src, &fakeBC{})

	c.SetSubscribers(1)
	waitFor(t, "首轮采样", func() bool { return src.Calls() >= 1 })
}

// 订阅数降到 0 必须真的停表（不只是"少采几次"）。
// 计数来自 hub 回调，异常断开也走这里 —— 泄漏的 goroutine 会让
// 面板在没人看的夜里持续吃 CPU，正是 D6 要消灭的东西。
func TestCollectorStopsOnLastUnsubscribe(t *testing.T) {
	src := &fakeSource{}
	c := newTestCollector(t, src, &fakeBC{})

	c.SetSubscribers(2) // 多个标签页只应有一份采集，不是每页一个 goroutine
	waitFor(t, "开始采样", func() bool { return src.Calls() >= 2 })
	if n := c.liveLoopCount(); n != 1 {
		t.Fatalf("2 个订阅者应只有 1 个采集循环, got %d", n)
	}

	c.SetSubscribers(0)
	settled := src.Calls()
	time.Sleep(80 * time.Millisecond)
	if n := src.Calls(); n != settled {
		t.Fatalf("退订后仍在采样: %d → %d", settled, n)
	}

	// 再订阅必须能重新跑起来（不能"停死"）。
	c.SetSubscribers(1)
	waitFor(t, "重新订阅后恢复采样", func() bool { return src.Calls() > settled })
}

// 反复 0→1→0 不得累积 goroutine。
func TestCollectorRestartDoesNotLeakGoroutines(t *testing.T) {
	src := &fakeSource{}
	c := newTestCollector(t, src, &fakeBC{})

	for i := 0; i < 20; i++ {
		c.SetSubscribers(1)
		c.SetSubscribers(0)
	}
	// goroutine 退出是异步的，给它时间；真有泄漏等多久都不会归零。
	waitFor(t, "所有循环退出", func() bool { return c.liveLoopCount() == 0 })
}

// 停止后要释放上一轮快照：面板长时间无人看时不应把数据一直握在内存里。
func TestSnapshotReleasedOnStop(t *testing.T) {
	src := &fakeSource{last: Snapshot{CPU: &CPUStat{Load1: 1.5}}}
	c := newTestCollector(t, src, &fakeBC{})

	c.SetSubscribers(1)
	waitFor(t, "有快照", func() bool { return c.Latest() != nil })

	c.SetSubscribers(0)
	waitFor(t, "快照被释放", func() bool { return c.Latest() == nil })
}

// 第一帧必须标 warming。
// CPU 利用率要两次采样差分才有意义，第一帧拿不到基线；若填 0，
// 前端会画出一个"系统完全空闲"的仪表，比显示 -- 更误导。
func TestFirstFrameMarkedWarming(t *testing.T) {
	src := &fakeSource{warmingFor: 1}
	bc := &fakeBC{}
	c := newTestCollector(t, src, bc)

	c.SetSubscribers(1)
	waitFor(t, "两帧", func() bool { return bc.count() >= 2 })

	var first, second Snapshot
	if err := json.Unmarshal(bc.payloads[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bc.payloads[1], &second); err != nil {
		t.Fatal(err)
	}
	if !first.Warming {
		t.Error("第一帧应标 warming")
	}
	if second.Warming {
		t.Error("第二帧不应再标 warming")
	}
}

// 重启采集后第一帧要重新回到 warming：
// 若把上一轮的 CPU 基线留在内存里，恢复后的第一帧会拿"停表期间"
// 的累计差分，算出一个跨了很久的平均利用率，读数严重失真。
func TestBaselineClearedOnStop(t *testing.T) {
	src := &resettableSource{}
	c := newTestCollector(t, src, &fakeBC{})

	c.SetSubscribers(1)
	waitFor(t, "采到基线", func() bool { return src.Calls() >= 2 })
	c.SetSubscribers(0)
	waitFor(t, "停止", func() bool { return c.Latest() == nil })

	if src.Resets() == 0 {
		t.Error("停表时应清掉 CPU 差分基线")
	}
}

// 采样出错不能终止采集循环：/proc 偶尔读失败（挂载点被卸载等）
// 就该下一轮重试，而不是让仪表永久停在最后一帧。
func TestSourceErrorDoesNotStopLoop(t *testing.T) {
	src := &flakySource{failFirst: 3}
	bc := &fakeBC{}
	c := newTestCollector(t, src, bc)

	c.SetSubscribers(1)
	waitFor(t, "出错后仍能出帧", func() bool { return bc.count() >= 1 })
	if src.Calls() < 4 {
		t.Fatalf("出错后应继续采样, 只跑了 %d 次", src.Calls())
	}
}

// 广播不能阻塞采集循环：hub 对慢客户端会丢弃，但这里要确保
// 即便广播里做了额外工作也不会把 tick 拖成长期积压。
func TestBroadcastPanicDoesNotKillCollector(t *testing.T) {
	bc := &panickingBC{}
	src := &fakeSource{}
	c := newTestCollector(t, src, bc)

	c.SetSubscribers(1)
	waitFor(t, "广播 panic 后仍在采样", func() bool { return src.Calls() >= 3 })
}

// resettableSource 验证停表时基线被清掉（Reset 被调用）。
// resettableSource 验证停表时基线被清掉（Reset 被调用）。
type resettableSource struct {
	mu         sync.Mutex
	calls      int
	resetCalls int
}

func (s *resettableSource) Sample() (Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return Snapshot{CPU: &CPUStat{}}, s.calls == 1, nil
}

func (s *resettableSource) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetCalls++
}

func (s *resettableSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *resettableSource) Resets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetCalls
}

// flakySource：前 N 轮采样失败，验证错误不会终止采集循环。
type flakySource struct {
	mu        sync.Mutex
	calls     int
	failFirst int
}

func (s *flakySource) Sample() (Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.failFirst {
		return Snapshot{}, true, errFake
	}
	return Snapshot{}, false, nil
}

func (s *flakySource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type panickingBC struct{ n atomic.Int64 }

func (b *panickingBC) Broadcast(string, []byte) {
	if b.n.Add(1) == 1 {
		panic("hub 炸了")
	}
}

var errFake = errors.New("采样失败")

// SetSubscribers 由 hub 回调驱动，而采样跑在另一个 goroutine 里。
// 这里压并发：本机不支持 -race，但能抓到 panic、死锁与 goroutine 泄漏 ——
// 这三种都是"订阅数快速变化"时真正会出的事（手机切后台、标签页反复开关）。
func TestConcurrentSubscribeResetDoesNotLeak(t *testing.T) {
	src := &fakeSource{}
	c := newTestCollector(t, src, &fakeBC{})

	done := make(chan struct{})
	for g := 0; g < 4; g++ {
		go func() {
			for i := 0; i < 200; i++ {
				c.SetSubscribers(i % 3)
			}
			done <- struct{}{}
		}()
	}
	for g := 0; g < 4; g++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("并发下卡住了")
		}
	}

	c.SetSubscribers(0)
	waitFor(t, "全部循环退出", func() bool { return c.liveLoopCount() == 0 })
}

// 停表之后不允许再有快照写回。
//
// 必须"确定性地"制造窗口：用一个阻塞的 Source，等它真的进了 Sample
// 再停表，然后才放行。先前用 sleep 压窗口的写法是假测试 —— 实测
// goroutine 还没被调度起来、stop 已关闭，Sample 一次都没被调用，
// 断言恒真（把守卫删掉也照样绿）。
func TestNoSnapshotWrittenAfterStop(t *testing.T) {
	src := &blockingSource{entered: make(chan struct{}), release: make(chan struct{})}
	c := newTestCollector(t, src, &fakeBC{})

	c.SetSubscribers(1)
	select {
	case <-src.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("采集循环没进入 Sample")
	}

	c.SetSubscribers(0) // 正卡在采样中途时停表
	close(src.release)

	// 给足时间让那一轮走到底并尝试写回。
	time.Sleep(50 * time.Millisecond)
	if snap := c.Latest(); snap != nil {
		t.Fatalf("已停表却写回了快照 seq=%d", snap.Seq)
	}
}

// blockingSource 让测试能精确控制"采样进行中"这个时刻。
type blockingSource struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingSource) Sample() (Snapshot, bool, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return Snapshot{CPU: &CPUStat{}}, false, nil
}

// 改间隔必须重启 ticker。
//
// 只改字段不重启的后果：新值要等进程重启才生效。这是设置页里最显眼的一项，
// 用户改完会**立刻盯着仪表跳动速度**确认，所以它是那种"当场就被发现"的失效
// —— 但也正因为看起来太容易实现，最容易写成只存字段。
func TestSetIntervalChangesTickRate(t *testing.T) {
	src := &fakeSource{}
	bc := &fakeBC{}
	c := NewCollector(src, bc, 10*time.Millisecond)
	defer c.Stop()
	c.SetSubscribers(1)
	waitFor(t, "第一帧", func() bool { return bc.count() >= 1 })

	c.SetInterval(200 * time.Millisecond)

	// 慢间隔下，固定窗口里到帧的数量应该明显少。判据用"帧数"而不是"时间戳
	// 差"：后者在调度抖动下会误报，而 10ms vs 200ms 是 20 倍差，用数量区分
	// 不需要卡时间。
	base := bc.count()
	time.Sleep(120 * time.Millisecond)
	if got := bc.count() - base; got > 3 {
		t.Errorf("改成 200ms 后 120ms 内还在出 %d 帧，说明 ticker 没换", got)
	}
}

// 没有订阅者时改间隔不得把采集循环起来（D6）。
// 为改一个数字而起一个循环，等于把"0 客户端 CPU ≈ 0%"这条指标弄没了。
func TestSetIntervalDoesNotStartCollector(t *testing.T) {
	src := &fakeSource{}
	c := NewCollector(src, &fakeBC{}, 10*time.Millisecond)
	defer c.Stop()
	c.SetSubscribers(0)
	c.SetInterval(20 * time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	if n := src.Calls(); n != 0 {
		t.Fatalf("改间隔时起了采集循环，0 订阅也采了 %d 次", n)
	}
	if n := c.liveLoopCount(); n != 0 {
		t.Fatalf("残留 %d 个循环", n)
	}
}

// 换间隔时旧循环必须退干净（不能靠"标志位看起来停了"糊过去）。
// 泄漏的形态：每改一次设置就多一个循环，用户来回调几次采样间隔，
// /proc 的读取量就成倍上去，而界面上什么也看不出来。
func TestSetIntervalDoesNotLeakLoops(t *testing.T) {
	c := NewCollector(&fakeSource{}, &fakeBC{}, 5*time.Millisecond)
	defer c.Stop()
	c.SetSubscribers(1)
	waitFor(t, "循环起来", func() bool { return c.liveLoopCount() == 1 })
	for _, ms := range []time.Duration{5, 8, 3, 20, 5} {
		c.SetInterval(ms * time.Millisecond)
	}
	// 任意时刻最多一个循环。
	waitFor(t, "只剩一个循环", func() bool { return c.liveLoopCount() == 1 })
	time.Sleep(30 * time.Millisecond)
	if n := c.liveLoopCount(); n != 1 {
		t.Fatalf("换过 5 次间隔后存活 %d 个循环", n)
	}
}

// 非法间隔回落到 1s 而不是停摆或崩溃（库里可能留着旧版本写的坏数字）。
func TestSetIntervalClampsInvalid(t *testing.T) {
	c := NewCollector(&fakeSource{}, &fakeBC{}, 10*time.Millisecond)
	defer c.Stop()
	for _, bad := range []time.Duration{0, -1, -time.Second} {
		c.SetInterval(bad)
		if got := c.Interval(); got != time.Second {
			t.Errorf("间隔 %v 应回落到 1s，得 %v", bad, got)
		}
	}
}
