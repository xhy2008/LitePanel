package termws

// 桥接层测试。用假 hub，这样断言的是"桥接做了什么"而不是 ws 的投递细节；
// 真 ws 的接通由 cmd/litepanel 的装配测试覆盖。
//
// hub 接口里的订阅者是不透明令牌（any）：真实现下它是 *ws.Client，测试里
// 用任意指针。桥接只把它当 map key 和"最后一次按键的设备"记号用。

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"litepanel/internal/terminal"
)

// ---------- 假 hub ----------

type fakeHub struct {
	mu        sync.Mutex
	bins      []binMsg // BroadcastBin 历史
	singles   []binMsg // SendBinTo 历史
	joinCB    map[string]func(any)
	binCB     map[string]func(any, string, []byte)
	joinToken any // triggerJoin 用的令牌
}

type binMsg struct {
	sub     any
	ch      string
	payload string
}

func newFakeHub() *fakeHub {
	return &fakeHub{joinCB: map[string]func(any){}, binCB: map[string]func(any, string, []byte){}}
}

func (f *fakeHub) BroadcastBin(ch string, payload []byte) {
	f.mu.Lock()
	f.bins = append(f.bins, binMsg{ch: ch, payload: string(payload)})
	f.mu.Unlock()
}

func (f *fakeHub) SendBinTo(sub any, ch string, payload []byte) {
	f.mu.Lock()
	f.singles = append(f.singles, binMsg{sub: sub, ch: ch, payload: string(payload)})
	f.mu.Unlock()
}

func (f *fakeHub) OnJoin(ch string, fn func(any)) {
	f.mu.Lock()
	f.joinCB[ch] = fn
	f.mu.Unlock()
}

func (f *fakeHub) OnBinary(prefix string, fn func(any, string, []byte)) {
	f.mu.Lock()
	f.binCB[prefix] = fn
	f.mu.Unlock()
}

// 测试驱动用的入口（真实现里由 hub 的读泵触发）
func (f *fakeHub) triggerJoin(ch string, sub any) {
	f.mu.Lock()
	fn := f.joinCB[ch]
	f.mu.Unlock()
	if fn != nil {
		fn(sub)
	}
}

func (f *fakeHub) triggerBinary(sub any, ch string, payload []byte) {
	f.mu.Lock()
	var fn func(any, string, []byte)
	best := -1
	for p, cb := range f.binCB {
		if strings.HasPrefix(ch, p) && len(p) > best {
			fn, best = cb, len(p)
		}
	}
	f.mu.Unlock()
	if fn != nil {
		fn(sub, ch, payload)
	}
}

func (f *fakeHub) allBroadcast() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, m := range f.bins {
		b.WriteString(m.ch)
		b.WriteString("|")
		b.WriteString(m.payload)
		b.WriteString("\n")
	}
	return b.String()
}

func (f *fakeHub) broadcastCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bins)
}

func (f *fakeHub) lastSingle() (binMsg, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.singles) == 0 {
		return binMsg{}, false
	}
	return f.singles[len(f.singles)-1], true
}

func (f *fakeHub) singlesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.singles)
}

// ---------- 工具 ----------

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// freshSession 造一个由 tmux 独立持有的会话名，并在测试结束时真的杀掉它。
func newManager(t *testing.T) (*Manager, *fakeHub) {
	t.Helper()
	fh := newFakeHub()
	m := NewManager(fh, terminal.DefaultBin)
	t.Cleanup(m.Close)
	return m, fh
}

func testID() string {
	return fmt.Sprintf("t5-%d", time.Now().UnixNano()%1e7)
}

func create(t *testing.T, m *Manager, id string, cols, rows int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Create(ctx, id, terminal.SessionOpts{Cols: cols, Rows: rows}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = terminal.KillSession(terminal.DefaultBin, "lp-"+id) })
}

// tmuxCapture 直接问 tmux 要屏幕：独立观测量，不经过被测代码。
func tmuxCapture(t *testing.T, id string) string {
	t.Helper()
	out, err := capturePane(id)
	if err != nil {
		return ""
	}
	return out
}

func paneSize(t *testing.T, id string) string {
	t.Helper()
	return paneWh(id)
}

// ---------- 测试 ----------

// 1. pane 输出要扇出到 term:{id} 频道
func TestOutputFansOutToChannel(t *testing.T) {
	m, fh := newManager(t)
	id := testID()
	create(t, m, id, 100, 24)

	// 用算术结果做断言目标：tty 会回显命令本身，只有 "r42" 是执行后才可能出现的
	sendKeys(t, fh, id, nil, "echo r$((6*7))\r")
	waitFor(t, func() bool { return strings.Contains(fh.allBroadcast(), "r42") },
		"输出扇出到频道")
}

// 2. 实时流的频道名必须正确（前端按频道名订阅，错了就什么都收不到）
func TestFanOutUsesSessionChannel(t *testing.T) {
	m, fh := newManager(t)
	id := testID()
	create(t, m, id, 100, 24)

	sendKeys(t, fh, id, nil, "echo chan$((3+4))\r")
	waitFor(t, func() bool { return strings.Contains(fh.allBroadcast(), "chan7") }, "输出到达")

	fh.mu.Lock()
	defer fh.mu.Unlock()
	seen := false
	for _, b := range fh.bins {
		if b.ch != "term:"+id && strings.Contains(b.payload, "chan7") {
			t.Fatalf("输出发到了错误频道 %q", b.ch)
		}
		if b.ch == "term:"+id {
			seen = true
		}
	}
	if !seen {
		t.Fatal("没有任何帧发到 term:" + id)
	}
}

// 3. 回放只给刚接入的设备，不能广播（否则 PC 正在看的屏被手机一连就重播）。
//
// 判据：把已收到的广播记录清空，之后任何再次出现的 old101 都不可能是实时流
// （control mode 只发差分，屏幕上已有的内容不会重发），只能来自回放。
// 于是"回放有没有偷偷走广播"变成一个可直接观测的差值。
func TestReplayGoesToNewSubscriberOnly(t *testing.T) {
	m, fh := newManager(t)
	id := testID()
	create(t, m, id, 100, 24)

	sendKeys(t, fh, id, nil, "echo old$((100+1))\r")
	waitFor(t, func() bool { return strings.Contains(fh.allBroadcast(), "old101") }, "历史输出")

	fh.mu.Lock()
	fh.bins = nil // 只留记录层的空白，会话与缓冲都不动
	fh.mu.Unlock()

	phone := new(any)
	fh.triggerJoin("term:"+id, phone)
	waitFor(t, func() bool { return fh.singlesCount() > 0 }, "回放单点投递")

	msg, _ := fh.lastSingle()
	if msg.sub != phone {
		t.Fatal("回放没发给刚接入的那个订阅者")
	}
	if msg.ch != "term:"+id {
		t.Fatalf("回放频道错: %q", msg.ch)
	}
	if !strings.Contains(msg.payload, "old101") {
		t.Fatalf("回放里没有历史输出: %q", msg.payload)
	}
	if strings.Contains(fh.allBroadcast(), "old101") {
		t.Fatal("回放内容出现在广播里 —— 会把正在看的设备整屏重播")
	}
}

// 4. 面板重启后内存缓冲是空的（tmux 会话还活着）→ 回放必须退回 capture-pane
func TestReplayFallsBackToCaptureAfterRestart(t *testing.T) {
	m, fh := newManager(t)
	id := testID()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Create(ctx, id, terminal.SessionOpts{Cols: 100, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminal.KillSession(terminal.DefaultBin, "lp-"+id) })

	sendKeys(t, fh, id, nil, "echo kept$((50+1))\r")
	waitFor(t, func() bool { return strings.Contains(fh.allBroadcast(), "kept51") }, "输出")

	// 模拟面板重启：新 Manager（空缓冲），对账接入既有 tmux 会话
	m2, fh2 := newManager(t)
	attached, err := m2.Reconcile(ctx)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	found := false
	for _, a := range attached {
		if a == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("对账没找回会话 %q, got %v", id, attached)
	}

	fh2.triggerJoin("term:"+id, new(any))
	waitFor(t, func() bool { return fh2.singlesCount() > 0 }, "重启后的回放")
	msg, _ := fh2.lastSingle()
	if !strings.Contains(msg.payload, "kept51") {
		t.Fatalf("重启后回放缺历史（capture 兜底没生效）: %q", msg.payload)
	}
}

// 5. 浏览器按键要真的进到 pane 并被 shell 执行
func TestInboundKeysReachPane(t *testing.T) {
	m, fh := newManager(t)
	id := testID()
	create(t, m, id, 100, 24)

	sendKeys(t, fh, id, nil, "printf 'XYZ%s\\n' $((11*11))\r")
	waitFor(t, func() bool { return strings.Contains(tmuxCapture(t, id), "XYZ121") },
		"命令在 pane 里执行（tmux 侧观测）")
}

// 6. 客户端断开不能杀掉 tmux 会话（"关浏览器再进来还在"的地基）
func TestAllClientsGoneKeepsTmuxSession(t *testing.T) {
	m, fh := newManager(t)
	id := testID()
	create(t, m, id, 100, 24)

	sendKeys(t, fh, id, nil, "echo stay$((9+1))\r")
	waitFor(t, func() bool { return strings.Contains(fh.allBroadcast(), "stay10") }, "输出")

	// 桥接不持有浏览器连接：设备全掉线时什么都不做，tmux 会话必须还在
	m.ClientGone(new(any))
	waitFor(t, func() bool { return strings.Contains(tmuxCapture(t, id), "stay10") }, "会话仍在")

	// 新设备接入仍然能拿到历史
	fh.triggerJoin("term:"+id, new(any))
	waitFor(t, func() bool {
		msg, ok := fh.lastSingle()
		return ok && strings.Contains(msg.payload, "stay10")
	}, "重连后仍有历史")
}

//  7. 尺寸策略（一个 pane 只有一套网格，实测做不到每设备各画各的）：
//     a) 只有"最后一次按键的设备"决定尺寸 —— 手机 merely 打开页面（xterm
//     fit 会自动上报尺寸）不该把桌面上正在跑的程序压成 12 行；
//     b) 谁开始打字，pane 就跟谁的尺寸走。
func TestResizeFollowsLastInteractiveClient(t *testing.T) {
	m, fh := newManager(t)
	id := testID()
	create(t, m, id, 100, 24)

	pc, phone := new(any), new(any)

	// PC 先报尺寸再打字 → 成为尺寸主人
	fh.triggerBinary(pc, "term:"+id, ResizeFrame(120, 40))
	sendKeys(t, fh, id, pc, "echo pc\r")
	waitFor(t, func() bool { return paneSize(t, id) == "120x40" }, "尺寸跟随 PC")

	// 手机只上报尺寸、不打字：必须被忽略（否则手机上打开终端就把桌面压小）
	fh.triggerBinary(phone, "term:"+id, ResizeFrame(60, 12))
	time.Sleep(300 * time.Millisecond)
	if got := paneSize(t, id); got != "120x40" {
		t.Fatalf("手机没打字却改了 pane 尺寸: %s", got)
	}

	// 手机开始打字 → 尺寸跟手机走
	sendKeys(t, fh, id, phone, "echo phone\r")
	waitFor(t, func() bool { return paneSize(t, id) == "60x12" }, "尺寸跟随手机")

	// 回到 PC 打字 → 又跟 PC 走（PC 尺寸已知，不需要重新上报）
	sendKeys(t, fh, id, pc, "echo back\r")
	waitFor(t, func() bool { return paneSize(t, id) == "120x40" }, "尺寸回到 PC")
}

// 8. 回放缓冲必须有上限：12GB 的机器上，一个长输出会话不能把内存吃光
func TestReplayBufferIsCapped(t *testing.T) {
	m, fh := newManager(t)
	id := testID()
	create(t, m, id, 100, 24)

	// 一次产出约 600KB
	sendKeys(t, fh, id, nil, "for i in $(seq 1 8000); do echo line-$i-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; done\r")
	waitFor(t, func() bool { return strings.Contains(fh.allBroadcast(), "line-8000-") }, "长输出完成")

	fh.triggerJoin("term:"+id, new(any))
	waitFor(t, func() bool { return fh.singlesCount() > 0 }, "回放")
	msg, _ := fh.lastSingle()
	if len(msg.payload) > MaxReplayBytes+4096 {
		t.Fatalf("回放 %d 字节，超过上限 %d", len(msg.payload), MaxReplayBytes)
	}
	// 截断必须保留尾部：用户重连最关心"刚才跑到哪了"
	if !strings.Contains(msg.payload, "line-8000-") {
		t.Fatal("截断丢了最新输出（应保尾去头）")
	}
}

// 9. 未知会话的输入必须被丢掉而不是 panic / 投递到别的会话
func TestInputToUnknownSessionIsIgnored(t *testing.T) {
	m, fh := newManager(t)
	id := testID()
	other := testID()
	create(t, m, other, 100, 24)

	fh.triggerBinary(new(any), "term:"+id, KeysFrame([]byte("echo hack\r")))
	// 没有任何会话被这条输入影响
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(tmuxCapture(t, other), "hack") {
		t.Fatal("未知频道的输入串到了别的会话")
	}
	_ = m
}

// ---------- tmux 侧独立观测 ----------

func capturePane(id string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-p", "-t", "lp-"+id).Output()
	return string(out), err
}

func paneWh(id string) string {
	out, err := exec.Command("tmux", "display-message", "-t", "lp-"+id, "-p",
		"#{window_width}x#{window_height}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---------- 输入帧辅助 ----------

func sendKeys(t *testing.T, fh *fakeHub, id string, sub any, text string) {
	t.Helper()
	if sub == nil {
		sub = new(any)
	}
	fh.triggerBinary(sub, "term:"+id, KeysFrame([]byte(text)))
}
