package terminal

import (
	"context"
	"testing"
)

// corr 的纯逻辑测试。集成测试覆盖了端到端，但只有这里能在没有 tmux 的
// 机器上钉住「按命令编号认领响应」这条规则 —— 它的失败模式是"命令失败
// 却返回成功"，最阴的一种。
func TestCorrClaimsFirstBeginAndMatchesNumber(t *testing.T) {
	c := &corr{done: make(chan struct{})}

	// 别的块（编号 7）的 %end 不能把我们的等待者叫醒
	if c.accept(Event{Kind: EvEnd, Arg: "111 7 0"}) {
		t.Fatal("无主 end 帧被认领了")
	}
	// 我们的块：第一个 %begin 就是自己的（FIFO）
	if !c.accept(Event{Kind: EvBegin, Arg: "111 9 1"}) {
		t.Fatal("自己的 begin 帧没被认领")
	}
	if !c.accept(Event{Kind: EvBlockData, Data: "boom\r\n"}) {
		t.Fatal("块内数据没被收取")
	}
	if c.accept(Event{Kind: EvEnd, Arg: "111 7 0"}) {
		t.Fatal("编号不符的 end 帧不该被认领")
	}
	if !c.accept(Event{Kind: EvError, Arg: "111 9 1"}) {
		t.Fatal("自己的 error 帧没被认领")
	}
	<-c.done
	if c.err == nil || c.err.Error() != "boom" {
		t.Fatalf("%%error 要变成错误（内容已 Trim）: %v", c.err)
	}
	if string(c.data) != "boom\r\n" {
		t.Fatalf("data = %q", c.data)
	}
	// 已定案后不再吃任何事件
	if c.accept(Event{Kind: EvOutput, Data: "x"}) {
		t.Fatal("已结束的 corr 还在认领事件")
	}
}

func TestCorrSuccessPathKeepsErrNil(t *testing.T) {
	c := &corr{done: make(chan struct{})}
	c.accept(Event{Kind: EvBegin, Arg: "1 5 1"})
	c.accept(Event{Kind: EvBlockData, Data: "L1\r\n"})
	if !c.accept(Event{Kind: EvEnd, Arg: "1 5 1"}) {
		t.Fatal("end 帧没被认领")
	}
	<-c.done
	if c.err != nil {
		t.Fatalf("%%end 路径不该有错误: %v", c.err)
	}
}

// TestCorrAbortIsIdempotent：调用方超时 abort 之后，迟到的 %end 不能再
// close 同一个 channel（那会 panic，而且 panic 在 readLoop 里 = 整条终端
// 连接崩掉）。
func TestCorrAbortIsIdempotent(t *testing.T) {
	c := &corr{done: make(chan struct{})}
	c.accept(Event{Kind: EvBegin, Arg: "1 5 1"})
	c.abort(context.DeadlineExceeded)
	<-c.done
	if c.accept(Event{Kind: EvEnd, Arg: "1 5 1"}) {
		t.Fatal("abort 后迟到的 end 帧被认领了")
	}
	c.abort(context.DeadlineExceeded) // 二次 abort 不能 panic
}
