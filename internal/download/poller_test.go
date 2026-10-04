package download

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 轮询器的存在理由见 events.go 顶部：**aria2 没有进度事件**（实测），
// 所以速度/字节数只能面板自己拿。它的设计约束全部来自"别把开销搞大"：
//   - 只在有活动任务时跑；清空就停（空闲时零请求）
//   - 一次 tellActive 拿全部，而不是每个任务问一次
//   - 只在状态**变化**时上报事件（进度快照另走一条路，不混进事件流）

// pollerEnv 是可编程的状态源。
type pollerEnv struct {
	mu       sync.Mutex
	active   []Status
	stopped  map[string]Status // gid → 终态（gid 从 active 消失后查它）
	calls    int
	statuses int // 单任务查询次数
}

func (p *pollerEnv) list(context.Context) ([]Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return append([]Status(nil), p.active...), nil
}

func (p *pollerEnv) final(_ context.Context, gid string) (*Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses++
	if s, ok := p.stopped[gid]; ok {
		return &s, nil
	}
	s := Status{GID: gid, Status: "removed"}
	return &s, nil
}

func (p *pollerEnv) setActive(g ...Status) {
	p.mu.Lock()
	p.active = append([]Status(nil), g...)
	p.mu.Unlock()
}

func (p *pollerEnv) setStopped(gid string, s Status) {
	p.mu.Lock()
	if p.stopped == nil {
		p.stopped = map[string]Status{}
	}
	p.stopped[gid] = s
	p.mu.Unlock()
}

func (p *pollerEnv) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *pollerEnv) statusCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statuses
}

func newTestPoller(t *testing.T, e *pollerEnv, n notifier) *Poller {
	t.Helper()
	p := NewPoller(e.list, e.final, n, PollerOptions{Interval: 10 * time.Millisecond})
	go p.Run(context.Background())
	t.Cleanup(p.Close)
	return p
}

// kick 复刻生产的接线：装配层把事件桥与添加成功的回调都接到 poller.Wake
// （见 wire_test.go），所以"列表里已经有活动任务"的测试都必须先走一遍
// 这个入口，否则测的是一个生产里不存在的状态。
func kick(p *Poller) { p.Wake() }

// 空闲时必须零请求。
//
// 这是轮询器唯一的"不能违背"的约束：设计承诺"面板开销极低"，而一个每秒
// 问一次 aria2 的常驻循环正是最容易把这个承诺变成空话的地方。任务全下完
// 之后面板还每秒钟发一次 RPC，在一台跑着别的服务的机器上就是纯浪费。
func TestPollerIdleMakesNoRequests(t *testing.T) {
	e := &pollerEnv{}
	p := NewPoller(e.list, e.final, func(Event) {}, PollerOptions{Interval: 5 * time.Millisecond})
	go p.Run(context.Background())
	defer p.Close()
	time.Sleep(120 * time.Millisecond)
	if n := e.callCount(); n != 0 {
		t.Errorf("没有活动任务时不该发任何请求, got %d 次", n)
	}
	if p.IsPolling() {
		t.Error("空闲时 IsPolling 应为 false")
	}
}

// 有活动任务时按间隔轮询，并把进度作为快照交出去。
//
// 注意快照**不是** Event：进度每 tick 都变，混进事件流会让前端把每一帧
// 当成"状态变化"处理（重排列表、触发刷新动画），也让"事件"这个词失去含义。
func TestPollerEmitsProgressWhileActive(t *testing.T) {
	e := &pollerEnv{active: []Status{{GID: "0xa", Status: "active",
		TotalLength: "1000", CompletedLength: "400", DownloadSpeed: "512", Connections: "5"}}}
	var mu sync.Mutex
	var snaps []Progress
	p := NewPoller(e.list, e.final, func(Event) {}, PollerOptions{
		Interval: 10 * time.Millisecond,
		OnProgress: func(items []Progress) {
			mu.Lock()
			snaps = append(snaps, items...)
			mu.Unlock()
		},
	})
	go p.Run(context.Background())
	defer p.Close()
	kick(p)

	waitCond(t, "拿到一次进度快照", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(snaps) > 0
	})
	mu.Lock()
	s := snaps[len(snaps)-1]
	mu.Unlock()
	if s.GID != "0xa" || s.DoneBytes != 400 || s.Speed != 512 || s.Connections != 5 {
		t.Errorf("进度字段不对: %+v", s)
	}
	if !p.IsPolling() {
		t.Error("有活动任务时 IsPolling 应为 true")
	}
}

// aria2 的字段全是字符串。把 "400" 直接塞进 JSON 再让前端 Number() 是
// 一种选择，但速度、剩余时间、百分比都要算 —— 在边界上转一次、失败就当 0，
// 比让每个消费方各写一遍字符串转数字更省事（也更容易测）。
func TestNumericFieldsParsed(t *testing.T) {
	e := &pollerEnv{active: []Status{{GID: "0xb", Status: "active",
		TotalLength: "not-a-number", CompletedLength: "10", DownloadSpeed: ""}}}
	var mu sync.Mutex
	var got []Progress
	p := NewPoller(e.list, e.final, func(Event) {}, PollerOptions{
		Interval: 10 * time.Millisecond,
		OnProgress: func(items []Progress) {
			mu.Lock()
			got = append(got, items...)
			mu.Unlock()
		},
	})
	go p.Run(context.Background())
	defer p.Close()
	kick(p)
	waitCond(t, "拿到快照", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	})
	mu.Lock()
	s := got[0]
	mu.Unlock()
	if s.TotalBytes != 0 {
		t.Errorf("非法数字应降级为 0 而不是报错, got %d", s.TotalBytes)
	}
	if s.DoneBytes != 10 {
		t.Errorf("合法数字要解析对, got %d", s.DoneBytes)
	}
}

// 任务变空后必须停下来。
//
// 这条和"空闲时零请求"是一对：只在**开始时**检查空闲的轮询器，会在最后
// 一个任务结束后继续每秒问一次，永远停不下来。
func TestPollerStopsWhenNoActiveTasks(t *testing.T) {
	e := &pollerEnv{active: []Status{{GID: "0xa", Status: "active"}}}
	p := newTestPoller(t, e, func(Event) {})
	kick(p)
	waitCond(t, "进入轮询", func() bool { return p.IsPolling() })

	e.setActive() // 任务全部结束
	waitCond(t, "退出轮询", func() bool { return !p.IsPolling() })
	before := e.callCount()
	time.Sleep(80 * time.Millisecond)
	if after := e.callCount(); after != before {
		t.Errorf("清空后应停止轮询, 又多请求了 %d 次", after-before)
	}
}

// 新任务出现要能把停掉的轮询器叫醒。
//
// 否则"下完一个再点一个"之后界面永远不动：轮询器已经停了，而唯一的唤醒
// 来源是事件桥里的 started。
func TestWakeStartsPollingAgain(t *testing.T) {
	e := &pollerEnv{}
	p := newTestPoller(t, e, func(Event) {})
	time.Sleep(30 * time.Millisecond)
	if p.IsPolling() {
		t.Fatal("初始应空闲")
	}
	before := e.callCount()
	p.Wake()
	// 断的是**可观察效果**（发出了请求）而不是 IsPolling：任务列表还是空的，
	// 那一轮 tick 跑完会立刻自己停下来，标志位只在极短时间窗里为 true，
	// 拿它做断言会得出一条“偶尔红”的测试。真正要钉的是 Wake 能推动轮询。
	waitCond(t, "Wake 后确实去问了 aria2", func() bool { return e.callCount() > before })
}

// Wake 之后新任务必须能连续拿到进度（而不是只问一次又停）。
//
// 上面那条只能证明“问了一次”；这条钉住“有活动任务就持续问”——
// 否则“下完一个再点一个”之后进度条会动一下然后永远不动。
func TestWakeThenActiveKeepsPolling(t *testing.T) {
	e := &pollerEnv{}
	var mu sync.Mutex
	var snaps int
	p := NewPoller(e.list, e.final, func(Event) {}, PollerOptions{
		Interval:   10 * time.Millisecond,
		OnProgress: func(items []Progress) { mu.Lock(); snaps += len(items); mu.Unlock() },
	})
	go p.Run(context.Background())
	defer p.Close()

	e.setActive(Status{GID: "0xnew", Status: "active", CompletedLength: "1"})
	p.Wake()
	waitCond(t, "新任务持续拿到进度", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return snaps >= 2
	})
}

// 事件说完成、轮询随后又看到它在 active 时，不能把状态倒回去。
//
// 真实场景：aria2 处理"移除任务"与新快照之间有窗口期，两轮来源的顺序没有
// 保证。倒回去会让已完成的卡片重新变成进度条，然后下次轮询又跳回完成 ——
// 用户看到的是任务在"完成/下载中"之间反复抖。
func TestStateNeverRegressesFromTerminal(t *testing.T) {
	e := &pollerEnv{active: []Status{{GID: "0xg", Status: "active", CompletedLength: "1"}}}
	var mu sync.Mutex
	var evs []Event
	n := func(ev Event) {
		mu.Lock()
		evs = append(evs, ev)
		mu.Unlock()
	}
	p := NewPoller(e.list, e.final, n, PollerOptions{Interval: 10 * time.Millisecond})
	// 先由事件桥告知"这条完成了"。
	p.NoteState("0xg", EventComplete)
	go p.Run(context.Background())
	defer p.Close()
	// 必须真的跑过 tick 才算验证了"不倒退"：kick 一下模拟 started 事件
	// 之外的后续轮询（比如另一条任务的存在让轮询继续）。
	kick(p)
	time.Sleep(60 * time.Millisecond)
	if e.callCount() == 0 {
		t.Fatal("轮询根本没跑，这条测试什么都没验证")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range evs {
		if ev.GID == "0xg" && ev.Kind == EventStart {
			t.Errorf("已上报 complete 的任务不该再报 started: %+v", evs)
		}
	}
}

// gid 从 active 列表里消失时，必须问一次终态。
//
// 只报"不见了"不够：aria2 里 complete 与 error 都表现为"从 tellActive 消失"，
// 不问一句就分不清是下完了还是失败了 —— 而后者必须带原因。
func TestDisappearedGidResolvedToFinalState(t *testing.T) {
	e := &pollerEnv{active: []Status{{GID: "0xd", Status: "active"}}}
	e.setStopped("0xd", Status{GID: "0xd", Status: "error",
		ErrorCode: "22", ErrorMessage: "Resource not found."})
	var mu sync.Mutex
	var evs []Event
	p := NewPoller(e.list, e.final, func(ev Event) {
		mu.Lock()
		evs = append(evs, ev)
		mu.Unlock()
	}, PollerOptions{Interval: 10 * time.Millisecond})
	go p.Run(context.Background())
	defer p.Close()
	kick(p)

	waitCond(t, "第一轮：进入 active", func() bool { return p.IsPolling() })
	e.setActive() // 消失了
	waitCond(t, "消失后被解析为 error", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range evs {
			if ev.GID == "0xd" && ev.Kind == EventError {
				return true
			}
		}
		return false
	})
	mu.Lock()
	defer mu.Unlock()
	for _, ev := range evs {
		if ev.GID == "0xd" && ev.Kind == EventError {
			if ev.Error == "" {
				t.Errorf("error 事件必须带原因: %+v", ev)
			}
		}
	}
}

// 同一个终态不能每个 tick 重报一次。
//
// 轮询是循环，不做去重的话一个已完成的任务会在 tellStopped 里被反复问、
// 反复推，前端每 10ms 收到一条"完成"。
func TestTerminalStateReportedOnce(t *testing.T) {
	e := &pollerEnv{active: []Status{{GID: "0xo", Status: "active"}}}
	e.setStopped("0xo", Status{GID: "0xo", Status: "complete"})
	var mu sync.Mutex
	var n int
	p := NewPoller(e.list, e.final, func(ev Event) {
		if ev.GID == "0xo" && ev.Kind == EventComplete {
			mu.Lock()
			n++
			mu.Unlock()
		}
	}, PollerOptions{Interval: 5 * time.Millisecond})
	go p.Run(context.Background())
	defer p.Close()
	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n > 1 {
		t.Errorf("终态只该报一次, got %d 次", n)
	}
}

// Close 后循环必须退出（否则面板优雅关闭时这个 goroutine 永不结束）。
func TestPollerCloseStopsLoop(t *testing.T) {
	e := &pollerEnv{active: []Status{{GID: "0xa", Status: "active"}}}
	p := NewPoller(e.list, e.final, func(Event) {}, PollerOptions{Interval: 5 * time.Millisecond})
	go p.Run(context.Background())
	kick(p)
	waitCond(t, "进入轮询", func() bool { return p.IsPolling() })
	p.Close()
	time.Sleep(30 * time.Millisecond)
	before := e.callCount()
	time.Sleep(60 * time.Millisecond)
	if after := e.callCount(); after != before {
		t.Errorf("Close 后仍在轮询: %d → %d", before, after)
	}
}

// 一次 tick 只发一次 list 请求，而不是每个任务问一次。
//
// 16 个任务 × 每秒一次 = 每秒 16 次 RPC；tellActive 一把拿全部才是设计里
// "面板开销极低"的依据。
// 跨来源的终态去重。
//
// 上一版这条测试写成了“任务消失后不要重报”，但消失本身只会发生一次（
// 解析完就从 lastActive 移除了），所以它根本测不到去重逻辑——变异把整个
// 去重判断删掉都能过。
//
// 去重真正防的是跨来源的竞态：事件桥推了 complete，而同一时刻轮询那一轮
// 的快照里这条还在 active（aria2 内部处理有窗口期）；或反过来，
// resolveFinal 正在查状态时事件到了。不拦住就会双发：前端弹两次通知、
// 重拉两次列表。
func TestTerminalNotReportedTwiceAcrossSources(t *testing.T) {
	e := &pollerEnv{active: []Status{{GID: "0xr", Status: "active"}}}
	e.setStopped("0xr", Status{GID: "0xr", Status: "complete"})
	var mu sync.Mutex
	var kinds []EventKind
	// p 要在 NewPoller 返回后才能被 final 回调引用，所以先留个把手。
	var p *Poller
	notify := func(ev Event) {
		mu.Lock()
		kinds = append(kinds, ev.Kind)
		mu.Unlock()
	}
	p = NewPoller(e.list, func(ctx context.Context, gid string) (*Status, error) {
		// 模拟竞态：查状态的这段时间里，事件桥把终态送过来了（真实桥既会
		// 推事件也会 NoteState，两边都要做，否则测的不是同一个场景）。
		notify(Event{Kind: EventComplete, GID: gid})
		p.NoteState(gid, EventComplete)
		st := Status{GID: gid, Status: "complete"}
		return &st, nil
	}, notify, PollerOptions{Interval: 5 * time.Millisecond})
	go p.Run(context.Background())
	defer p.Close()

	kick(p)
	waitCond(t, "第一轮看到过它", func() bool { return e.callCount() >= 1 })
	e.setActive() // 从 active 消失 → resolveFinal 路径
	kick(p)
	waitCond(t, "终态被报出来", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(kinds) >= 1
	})
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	var completes int
	for _, k := range kinds {
		if k == EventComplete {
			completes++
		}
	}
	if completes != 1 {
		t.Errorf("跨来源的终态只该报一条, got %d 条: %v", completes, kinds)
	}
}

// 重试（重新开下）必须能清除旧的终态标记。
//
// 不清的话，“失败 → 点重试”之后这条任务在 known 里仍是 error，
// 新那一轮的进度与完成会被当成“已经报过”全部吞掉：用户点了重试，
// 界面永远不动。
func TestNoteStateStartAllowsReprogress(t *testing.T) {
	e := &pollerEnv{active: []Status{{GID: "0xrt", Status: "active", CompletedLength: "5"}}}
	var mu sync.Mutex
	var done int
	p := NewPoller(e.list, e.final, func(ev Event) {
		mu.Lock()
		done++
		mu.Unlock()
	}, PollerOptions{Interval: 10 * time.Millisecond})
	go p.Run(context.Background())
	defer p.Close()
	// 先标记为已终结，再“重试”。
	p.NoteState("0xrt", EventError)
	kick(p)
	time.Sleep(30 * time.Millisecond)
	p.NoteState("0xrt", EventStart) // 重试
	kick(p)
	waitCond(t, "重试后重新上报事件", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return done >= 1
	})
}

// aria2 不可达时轮询器必须停下。
//
// 不停的话，aria2 挂掉那段时间面板会每秒对一个死端口发一次 RPC：
// 与健康检查那边同一个“故障放大”问题，而且没有超时的话会把
// HTTP 连接池占满。
func TestPollerStopsWhenAria2Unavailable(t *testing.T) {
	fail := false
	var mu sync.Mutex
	list := func(context.Context) ([]Status, error) {
		mu.Lock()
		doFail := fail
		mu.Unlock()
		if doFail {
			return nil, &unavailableError{err: context.DeadlineExceeded}
		}
		return []Status{{GID: "0xa", Status: "active"}}, nil
	}
	p := NewPoller(list, nil, func(Event) {}, PollerOptions{Interval: 5 * time.Millisecond})
	go p.Run(context.Background())
	defer p.Close()
	kick(p)
	waitCond(t, "进入轮询", func() bool { return p.IsPolling() })
	mu.Lock()
	fail = true
	mu.Unlock()
	waitCond(t, "aria2 挂了要停止轮询", func() bool { return !p.IsPolling() })
}

// 非法/负数的数字字段降级为 0。
//
// aria2 在任务刚建好时确实会回空串；而负数一旦漏到前端，进度百分比会
// 算出负值（width: -3%），进度条直接消失而不报错——很难从画面上反推到
// 是一个字符串解析问题。
func TestNegativeNumbersClampToZero(t *testing.T) {
	if got := a2int("-5"); got != 0 {
		t.Errorf("负数应降级为 0, got %d", got)
	}
	if got := a2int(""); got != 0 {
		t.Errorf("空串应为 0, got %d", got)
	}
	if got := a2int("123"); got != 123 {
		t.Errorf("正常数字要解析对, got %d", got)
	}
}

// 每 tick 恰好一次 list 请求，且与任务数无关。
//
// 判据用"请求数 == 快照发布次数"而不是"请求数小于某个阈值"：阈值断言很容
// 易定得太松（把 tick 里的请求写成两次照样通过，那正是"面板开销极低"这条
// 承诺被悄悄打破的方式）。每次 tick 恰好发一次 list、发一次快照，两者数量
// 必须相等，多一个就红。
func TestOneRequestPerTickRegardlessOfTaskCount(t *testing.T) {
	var many []Status
	for i := 0; i < 20; i++ {
		many = append(many, Status{GID: string(rune('a' + i)), Status: "active"})
	}
	e := &pollerEnv{active: many}
	var mu sync.Mutex
	var publishes int
	p := NewPoller(e.list, e.final, func(Event) {}, PollerOptions{
		Interval: 10 * time.Millisecond,
		OnProgress: func(items []Progress) {
			mu.Lock()
			publishes++
			mu.Unlock()
		},
	})
	go p.Run(context.Background())
	defer p.Close()
	kick(p)
	waitCond(t, "跑过几轮", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return publishes >= 3
	})
	p.Close()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	pub := publishes
	mu.Unlock()
	calls := e.callCount()
	if calls != pub {
		t.Errorf("每 tick 应恰好一次 list 请求: 请求 %d 次 vs 快照 %d 次", calls, pub)
	}
	if n := e.statusCount(); n != 0 {
		t.Errorf("活动任务不该逐个 tellStatus, got %d 次", n)
	}
}
