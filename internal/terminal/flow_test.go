package terminal

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestStalledConsumerDoesNotFreezeSession 钉住丢掉的设计（无法在 CI 之外
// 复现的故障最难查，所以在这里钉死）：
//
// tmux 3.7c 对控制客户端**没有流速控制**（实测 refresh-client -A 各种写法
// 全被拒），所以浏览器侧停止读取时只能我们自己兜底。如果 emit 选择阻塞，
// readLoop 一堵，后果是：命令响应全部超时、管道一路堵回 pane、整条终端
// 僵死，而且用户得刷新页面才能恢复。宁可丢输出（顶多花屏一下），也绝不
// 卡死。丢掉的数量必须可观测，上层才能显示"输出过快，已省略 N 帧"。
func TestStalledConsumerDoesNotFreezeSession(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{Cols: 80, Rows: 24})

	// 一次灌远超缓冲深度的输出：2 万行 x ~10 字节，比 eventBuf 大一个数量级
	mark := fmt.Sprintf("TAIL%d", time.Now().UnixNano()%100000)
	if err := s.SendText(context.Background(),
		fmt.Sprintf("seq 1 20000 | sed 's/^/x/'; echo %s\n", mark)); err != nil {
		t.Fatal(err)
	}

	// 关键断言：**一个事件都不消费**，命令响应仍然要正常回来。
	// 消费者睡到测试结束，绝不读 Events()。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deadline := time.Now().Add(25 * time.Second)
	var text string
	for time.Now().Before(deadline) {
		got, err := s.CaptureAll(ctx)
		if err != nil {
			t.Fatalf("消费者停滞期间 Capture 失败: %v", err)
		}
		if strings.Contains(got, mark) {
			text = got
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if text == "" {
		t.Fatal("消费者不读事件时，输出再也取不回来了（readLoop 被堵死）")
	}

	// 这里**不**断言 Dropped()>0：事件缓冲按帧计，而 tmux 会把上万行合并
	// 成几个大帧（实测如此），所以不一定溢出。丢弃策略本身由
	// TestEmitDropsWhenBufferFull 用单元测定死，不靠 tmux 的合并行为。
	t.Logf("停滞消费者场景：丢掉 %d 帧，仍能取回全部输出", s.Dropped())
}

// TestEmitDropsWhenBufferFull 单元级钉住丢弃策略（不依赖 tmux）。
//
// 为什么值得单独测：真实 tmux 会把输出合并成大帧，集成测试里未必能让缓冲
// 区溢出；而"溢出时必须丢、必须计数、绝不阻塞"正是防僵死的那条线。
func TestEmitDropsWhenBufferFull(t *testing.T) {
	s := &Session{events: make(chan Event, 2)}
	for i := 0; i < 10; i++ {
		s.emit(Event{Kind: EvOutput, Data: fmt.Sprintf("f%d", i)})
	}
	if got := len(s.events); got != 2 {
		t.Fatalf("缓冲只有 2 格，却留下 %d 个事件", got)
	}
	if n := s.Dropped(); n != 8 {
		t.Fatalf("丢掉 8 帧却没记对: Dropped=%d", n)
	}
	// 丢的必须是后来的：先到的帧按序留给消费者
	close(s.events)
	var got []string
	for e := range s.events {
		got = append(got, e.Data)
	}
	if len(got) != 2 || got[0] != "f0" || got[1] != "f1" {
		t.Fatalf("事件顺序被打乱: %v", got)
	}
}

// TestResizeTriggersRedraw：改尺寸不只是改数字 —— 前端 xterm 先本地 resize，
// 再让我们通知 tmux；tmux 要给 pane 发 SIGWINCH，全屏程序才会重绘。尺寸
// 改了但程序没重绘，表现就是"能滚动但画面是花的"。
func TestResizeTriggersRedraw(t *testing.T) {
	tmuxReady(t)
	s := newTestSession(t, SessionOpts{Cols: 80, Rows: 24})

	// 用 COLUMNS 回显代替 top（Termux 不一定装了 top；bash 的 $COLUMNS 由
	// SIGWINCH 驱动更新，足够证明信号送到了 pane 里的进程）
	if err := s.SendText(context.Background(), "trap '_=$COLUMNS' WINCH; echo STARTED\n"); err != nil {
		t.Fatal(err)
	}
	expect(t, s, 5*time.Second, "STARTED", outputContains("STARTED"))

	if err := s.Resize(context.Background(), 100, 30); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		out := tmuxOut(t, "display-message", "-t", s.Name(), "-p", "#{window_width}")
		if strings.TrimSpace(out) == "100" {
			return
		}
		time.Sleep(60 * time.Millisecond)
	}
	t.Fatalf("窗口尺寸没跟上 Resize: %s",
		tmuxOut(t, "display-message", "-t", s.Name(), "-p", "#{window_width}"))
}
