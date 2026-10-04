package download

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 事件桥的两条职责，各自对应一类真机故障：
//   1. 通知形状。实测 aria2 1.37.0（dev/aria2probe 抓的原始报文）：
//      `{"method":"aria2.onDownloadStart","params":[{"gid":"1df9..."}]}`，
//      即 gid 在 params[0] 这个**对象**的 gid 键里。照 JSON-RPC 直觉去取
//      params[0] 会拿到一个 map；取 params[0].params[0]（早期设计的写法）
//      则根本不存在。取不到 gid 的后果是每条事件都静默丢弃（通知没有 id，
//      错不回），下载页表现为“任务永远停在下载中”。
//   2. 错误事件不带原因。实测 `onDownloadError` 的载荷只有一个 gid，没有
//      errorCode / errorMessage。不补一次 tellStatus，界面就只能显示
//      “下载失败”而不说为什么——正是用户明确反对过的那种“只报失败”。
//   3. 断线重连 + 补拉。aria2 被 systemctl restart 时 WS 必断；不重连则
//      下载页从此不再更新；重连了但不补拉全量，则断线期间完成的任务永远
//      停在“下载中”（M7-T4 的人工验收项正是这条）。

func startBridge(t *testing.T, f *fakeAria2, r *recorder) *EventBridge {
	t.Helper()
	b := NewEventBridge(f.wsURL(), r.notify, EventBridgeOptions{
		// 测试里把重连间隔压到毫秒级：默认退避从秒起，等它会让每条测试
		// 都跑好几秒，而退避本身另有专门的测试覆盖。
		MinReconnect: 5 * time.Millisecond,
		MaxReconnect: 50 * time.Millisecond,
		FullSync: func(context.Context) ([]Status, error) {
			return nil, nil
		},
	})
	go b.Run(context.Background())
	t.Cleanup(b.Close)
	return b
}

func TestEventStartParsed(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥已连上假 aria2", func() bool { return f.connCount() >= 1 })

	f.notify("onDownloadStart", "0xabc123")
	e := r.waitFor(t, "一条 start 事件", func(e Event) bool { return e.Kind == EventStart })
	if e.GID != "0xabc123" {
		t.Errorf("gid 要从 params[0].params[0] 里取出来, got %q", e.GID)
	}
}

func TestEventCompleteAndErrorParsed(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })

	f.notify("onDownloadComplete", "0xdead")
	f.notify("onDownloadError", "0xbeef")
	r.waitFor(t, "complete 事件", func(e Event) bool { return e.Kind == EventComplete && e.GID == "0xdead" })
	ge := r.waitFor(t, "error 事件", func(e Event) bool { return e.Kind == EventError })
	if ge.GID != "0xbeef" {
		t.Errorf("error 事件的 gid 不对, got %q", ge.GID)
	}
}

// btComplete 是 BT 特有的"种子做种完成"，语义上等同完成。漏接它的话，
// 下磁力链接时任务永远显示"下载中"（普通 complete 对 BT 不触发）。
func TestEventBtCompleteCountsAsComplete(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })
	f.notify("onBtDownloadComplete", "0xbt")
	r.waitFor(t, "bt 完成事件", func(e Event) bool {
		return e.Kind == EventComplete && e.GID == "0xbt"
	})
}

// pause / stop 也必须接。设计里它们没被列进通知清单（只列了 start/complete/
// error），但 aria2 确实会推：漏接的后果是用户在 aria2 侧（或别的客户端）
// 暂停任务后，面板这里还显示"下载中"。
func TestEventPauseAndStop(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })
	f.notify("onDownloadPause", "0xp")
	f.notify("onDownloadStop", "0xs")
	r.waitFor(t, "pause 事件", func(e Event) bool { return e.Kind == EventPaused && e.GID == "0xp" })
	r.waitFor(t, "stop 事件", func(e Event) bool { return e.Kind == EventStopped && e.GID == "0xs" })
}

// 无法识别的通知不能让桥退出或卡住。aria2 版本升级、或有人把 RPC 地址
// 指到别的 JSON-RPC 服务时，会收到格式完全不同的报文；桥一死，下载页就
// 永远定格在最后一帧。
// 无法识别的通知不能让桥退出或卡住。aria2 版本升级、或有人把 RPC 地址
// 指到别的 JSON-RPC 服务时，会收到格式完全不同的报文；桥一死，下载页就
// 永远定格在最后一帧。
func TestNestedGidShapeAlsoAccepted(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })
	// 老版本/某些配置下的形状：params[0].params[0] = gid。
	f.notifyNested("onDownloadStart", "0xnested")
	r.waitFor(t, "嵌套形状的 start 事件", func(e Event) bool {
		return e.Kind == EventStart && e.GID == "0xnested"
	})
}

// 错误事件必须带原因。实测：onDownloadError 的载荷只有 gid，**没有**
// errorCode/errorMessage。不补一次 tellStatus，界面就只能说“失败了”
// 而说不出为什么。
func TestErrorEventCarriesReasonFromStatusLookup(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	b := NewEventBridge(f.wsURL(), r.notify, EventBridgeOptions{
		MinReconnect: 5 * time.Millisecond,
		FullSync:     func(context.Context) ([]Status, error) { return nil, nil },
		Lookup: func(_ context.Context, gid string) (*Status, error) {
			return &Status{GID: gid, Status: "error",
				ErrorCode: "22", ErrorMessage: "Resource not found."}, nil
		},
	})
	go b.Run(context.Background())
	defer b.Close()
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })
	f.notify("onDownloadError", "0xerr")
	e := r.waitFor(t, "带原因的 error 事件", func(e Event) bool {
		return e.Kind == EventError && e.GID == "0xerr"
	})
	if !strings.Contains(e.Error, "Resource not found") {
		t.Errorf("错误事件要带 aria2 的原文原因, got %q", e.Error)
	}
}

// 查原因失败不能让错误事件消失。tellStatus 可能因为 aria2 刚刚
// removeDownloadResult 而查不到 gid；那时用户仍然需要知道“这条失败了”，
// 带上一个兜底的文案也比没有强。
func TestErrorEventSurvivesFailedLookup(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	b := NewEventBridge(f.wsURL(), r.notify, EventBridgeOptions{
		MinReconnect: 5 * time.Millisecond,
		FullSync:     func(context.Context) ([]Status, error) { return nil, nil },
		Lookup: func(context.Context, string) (*Status, error) {
			return nil, &Error{Code: 1, Message: "GID not found"}
		},
	})
	go b.Run(context.Background())
	defer b.Close()
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })
	f.notify("onDownloadError", "0xgone")
	r.waitFor(t, "查不到原因也要报失败", func(e Event) bool {
		return e.Kind == EventError && e.GID == "0xgone"
	})
}

// 裸字符串形状也要能解（防御性形状，实测未见过，但解析器接不下就是地雷）。
func TestBareStringGidShape(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })
	f.notifyRaw(`{"jsonrpc":"2.0","method":"aria2.onDownloadStart","params":["0xbare"]}`)
	r.waitFor(t, "裸串形状的 start 事件", func(e Event) bool {
		return e.Kind == EventStart && e.GID == "0xbare"
	})
}

// 完全不是 JSON 的报文不能影响桥。
//
// 上一版这条测试发的是"合法 JSON 但形状不对"，于是 handle() 里那条
// 反序列化失败分支**根本没被执行到**（变异注入 panic 都测不出米）。
// 真正会撞上它的场景是 RPC 地址/端口写错、连到了另一个服务（它回 HTML），
// 那种报文就不是 JSON。
func TestGarbagePayloadDoesNotKillBridge(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })
	for _, junk := range []string{
		"<html><body>404 Not Found</body></html>",
		`{`,              // 截断
		`[]`,             // 顶层不是对象
		`{"method":123}`, // 字段类型不对
		"",               // 空帧
	} {
		f.notifyRaw(junk)
	}
	f.notify("onDownloadStart", "0xafterjunk")
	r.waitFor(t, "一堆垃圾报文后仍能收事件", func(e Event) bool {
		return e.Kind == EventStart && e.GID == "0xafterjunk"
	})
	if got := r.count(); got != 1 {
		t.Errorf("垃圾报文不该产出 Event, 现在共 %d 条", got)
	}
}

// 重连成功后退避必须归零。
//
// 不归零的后果不是立即报错，而是"aria2 恢复后界面迟迟不回来"：一次偶发
// 抖动把 backoff 顶到 60s，之后每次短暂掉线都要等满一分钟才能重新收到
// 事件 —— 而这正好发生在 aria2 刚重启完、最需要界面跟上来的时候。
// 这里直接测两个方法：takeBackoff 翻倍、resetBackoff 归零。
func TestBackoffResetsAfterSuccessfulConnect(t *testing.T) {
	b := NewEventBridge("ws://x", func(Event) {}, EventBridgeOptions{
		MinReconnect: 10 * time.Millisecond,
		MaxReconnect: time.Second,
	})
	defer b.Close()
	if w := b.takeBackoff(); w != 10*time.Millisecond {
		t.Fatalf("首次等待应为 MinReconnect, got %s", w)
	}
	b.takeBackoff()
	if b.CurrentBackoff() < 20*time.Millisecond {
		t.Fatalf("连续失败要退避增长, got %s", b.CurrentBackoff())
	}
	b.resetBackoff()
	if got := b.CurrentBackoff(); got != 10*time.Millisecond {
		t.Errorf("连上后应归零, got %s", got)
	}
	// 封顶也要真的生效：不封顶则长时间宕机后 backoff 会涨到几十分钟。
	for i := 0; i < 20; i++ {
		b.takeBackoff()
	}
	if got := b.CurrentBackoff(); got > time.Second {
		t.Errorf("退避必须封顶在 MaxReconnect, got %s", got)
	}
}

// waiting 不是状态变化，不能当成 started 上报。
func TestFullSyncDoesNotEmitWaiting(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	b := NewEventBridge(f.wsURL(), r.notify, EventBridgeOptions{
		MinReconnect: 5 * time.Millisecond,
		FullSync: func(context.Context) ([]Status, error) {
			return []Status{
				{GID: "0xw1", Status: "waiting"},
				{GID: "0xw2", Status: "waiting"},
			}, nil
		},
	})
	go b.Run(context.Background())
	defer b.Close()
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })
	// 确认桥确实跑过补拉（否则下面断言 0 条会因为"什么都没发生"而假绿）：
	// 补拉一条 complete，等到它出现，再回头看 waiting 有没有滲进来。
	f.notify("onDownloadComplete", "0xmarker")
	r.waitFor(t, "标记事件", func(e Event) bool { return e.GID == "0xmarker" })
	for _, e := range r.all() {
		if e.GID == "0xw1" || e.GID == "0xw2" {
			t.Errorf("waiting 不该产生事件（会把排队任务画成正在下载）: %+v", e)
		}
	}
}

func TestUnknownNotificationIgnored(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })

	f.notify("onSomethingNew", "0xzzz")
	f.notify("onDownloadStart", "0xok") // 之后仍要能正常收
	r.waitFor(t, "后续事件仍能处理", func(e Event) bool {
		return e.Kind == EventStart && e.GID == "0xok"
	})
	if got := r.count(); got != 1 {
		t.Errorf("未知事件不该产出 Event, 现在共 %d 条", got)
	}
}

// gid 不是字符串（aria2 异常/被中间层改写）时不能让解析 panic。
func TestMalformedNotificationIgnored(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥已连上", func() bool { return f.connCount() >= 1 })

	// 故意发畸形报文：params[0] 不是对象，而是裸数组。
	f.notifyRaw(`{"jsonrpc":"2.0","method":"aria2.onDownloadStart","params":["0xbare"]}`)
	f.notify("onDownloadStart", "0xafter")
	r.waitFor(t, "畸形报文后仍正常", func(e Event) bool {
		return e.Kind == EventStart && e.GID == "0xafter"
	})
	// 裸数组这种"其实能安全提取"的形状，接不接受都合理；关键是不能崩、
	// 不能因此丢掉后面的正常事件。
}

// 断线必须重连。aria2 被 systemctl restart 时 WS 必断；不重连的后果不是
// 报错而是"界面从此不再更新" —— 最坏的一类故障，因为它看起来一切正常。
func TestReconnectsAfterServerDropsConnection(t *testing.T) {
	f := newFakeAria2(t)
	f.dropOnConnect = true // 一连上就掐断，模拟 aria2 重启
	r := &recorder{}
	startBridge(t, f, r)
	waitCond(t, "桥在反复重连", func() bool { return f.connCount() >= 3 })
}

// 退避：连续失败时重连间隔必须增长，不能死循环猛撞。aria2 没起来（或正在
// 启动）时，没有退避的桥会变成每秒几十次的连接风暴，同时把 CPU 和日志打满。
func TestBackoffGrowsOnRepeatedFailures(t *testing.T) {
	// 指向一个没人听的端口：永远连不上。
	b := NewEventBridge("ws://127.0.0.1:1/jsonrpc", func(Event) {}, EventBridgeOptions{
		MinReconnect: 10 * time.Millisecond,
		MaxReconnect: 200 * time.Millisecond,
	})
	defer b.Close()
	go b.Run(context.Background())
	time.Sleep(600 * time.Millisecond)
	d := b.Attempts()
	// 600ms 内，若无退避会尝试几百次；有退避（10→20→40→80→160→200 封顶）
	// 只应个位数次。
	if d > 12 {
		t.Errorf("退避没生效：600ms 内尝试了 %d 次", d)
	}
	if d < 2 {
		t.Errorf("完全没尝试重连: %d 次", d)
	}
	if b.CurrentBackoff() < 50*time.Millisecond {
		t.Errorf("退避应已增长, 当前 %s", b.CurrentBackoff())
	}
}

// 重连成功后必须补拉一次全量任务状态。
//
// 这是 M7-T4 人工验收「中途 systemctl restart aria2，任务自动续传」的另一半：
// 断线期间 aria2 推的事件全丢了，其中最重要的是 complete —— 不补拉，那个
// 已经下载完的任务会在界面上永远显示"下载中"，而且再也等不到纠正它的事件。
func TestFullSyncAfterReconnect(t *testing.T) {
	f := newFakeAria2(t)
	var mu sync.Mutex
	var syncs int
	// 第一次连接立刻掐断，强制走一次重连路径。
	first := true
	f.onConnect = func(fa *fakeAria2, c *websocket.Conn) {
		mu.Lock()
		drop := first
		first = false
		mu.Unlock()
		if drop {
			_ = c.Close()
			return
		}
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}
	r := &recorder{}
	b := NewEventBridge(f.wsURL(), r.notify, EventBridgeOptions{
		MinReconnect: 5 * time.Millisecond,
		MaxReconnect: 50 * time.Millisecond,
		FullSync: func(context.Context) ([]Status, error) {
			mu.Lock()
			syncs++
			mu.Unlock()
			return []Status{{GID: "0xmissed", Status: "complete"}}, nil
		},
	})
	go b.Run(context.Background())
	defer b.Close()
	waitCond(t, "重连后补拉过全量", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return syncs >= 1
	})
	// 补拉的结果要变成事件冒上去，否则"补拉"只是白问一次。
	r.waitFor(t, "补拉出的 complete 事件", func(e Event) bool {
		return e.Kind == EventComplete && e.GID == "0xmissed"
	})
}

// 首次连接也要补拉一次：面板重启后 aria2 里的任务还在跑，界面得立刻反映。
func TestFullSyncOnFirstConnect(t *testing.T) {
	f := newFakeAria2(t)
	r := &recorder{}
	b := NewEventBridge(f.wsURL(), r.notify, EventBridgeOptions{
		MinReconnect: 5 * time.Millisecond,
		FullSync: func(context.Context) ([]Status, error) {
			return []Status{{GID: "0xboot", Status: "active"}}, nil
		},
	})
	go b.Run(context.Background())
	defer b.Close()
	r.waitFor(t, "首连补拉的 active 事件", func(e Event) bool {
		return e.GID == "0xboot"
	})
}

// Close 之后不能继续重连。否则"用户退出登录/面板优雅关闭"时桥还在后台
// 猛撞 aria2，进程退不出去。
func TestCloseStopsReconnect(t *testing.T) {
	b := NewEventBridge("ws://127.0.0.1:1/jsonrpc", func(Event) {}, EventBridgeOptions{
		MinReconnect: 5 * time.Millisecond,
		MaxReconnect: 50 * time.Millisecond,
	})
	go b.Run(context.Background())
	time.Sleep(50 * time.Millisecond)
	b.Close()
	n := b.Attempts()
	time.Sleep(100 * time.Millisecond)
	if m := b.Attempts(); m > n+1 {
		t.Errorf("Close 后仍在重连: %d → %d", n, m)
	}
}
