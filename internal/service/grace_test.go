package service

import (
	"context"
	"testing"
	"time"
)

// 停止宽限期必须是可读写的默认值（设置页热改的就是它）。
//
// 这条测试钉的是"接线"而不是字段：handler 现在传 grace=0 表示"用面板
// 当前设置"，如果 Supervisor 里没有这个默认值，0 会被 terminate 当成
// "你自己决定"回落到常量，设置页上的数字就成了只存不读的摆设。
func TestSupervisorGraceDefault(t *testing.T) {
	s := NewSupervisor(nil)
	if got := s.Grace(); got != DefaultGrace {
		t.Errorf("默认应是 %v，得 %v", DefaultGrace, got)
	}
	s.SetGrace(30 * time.Second)
	if got := s.Grace(); got != 30*time.Second {
		t.Errorf("改后应是 30s，得 %v", got)
	}
	// 非正值按"没设置"处理：宽限 0 秒会让 SIGTERM 立刻升级成 SIGKILL，
	// 把还在写盘的服务打断 —— 一个坏数字不该有这么大的破坏力。
	s.SetGrace(0)
	if got := s.Grace(); got != DefaultGrace {
		t.Errorf("0 应回落到默认，得 %v", got)
	}
	s.SetGrace(-time.Second)
	if got := s.Grace(); got != DefaultGrace {
		t.Errorf("负值应回落到默认，得 %v", got)
	}
}

// SetLogLimit 必须既改"以后新建的缓冲"也**当场 resize 所有现存缓冲**。
//
// 只存新默认的话，用户把 500 调成 5000 之后，正在跑的服务还是 500 行
// —— 而那恰恰是他想多看日志的场景（服务正在出问题）。反过来调小不
// resize 则是内存不回收，设置变成单向的。
func TestSetLogLimitResizesExisting(t *testing.T) {
	b := NewLogBuf(10)
	for i := 0; i < 10; i++ {
		b.Write([]byte("line\n"))
	}
	if b.Len() != 10 {
		t.Fatalf("前置条件：应满 10 行，得 %d", b.Len())
	}
	b.Resize(3)
	if b.Len() != 3 {
		t.Errorf("调小应立刻裁掉最老的行，剩 %d", b.Len())
	}
	if got := b.Tail(10); len(got) != 3 || got[0] != "line" || got[2] != "line" {
		t.Errorf("裁完应留最新的 3 行，得 %v", got)
	}
	b.Resize(20)
	if b.Limit() != 20 {
		t.Errorf("调大应更新上限，得 %d", b.Limit())
	}
	// 调大之后能继续攒到新上限。
	for i := 0; i < 15; i++ {
		b.Write([]byte("x\n"))
	}
	if b.Len() != 18 {
		t.Errorf("3+15=18 行都应留住，得 %d", b.Len())
	}
	// 非 0 兜底：Resize(0) 不该把缓冲变成"永远空"。
	b.Resize(0)
	if b.Limit() != DefaultLogLines {
		t.Errorf("0 应回落到默认，得 %d", b.Limit())
	}
}

// Supervisor.SetLogLimit 的三个去处逐个验证：正在跑的进程、已退出服务的
// last 缓冲、之后新建的缓冲。漏掉任何一个都会出现"改了设置但某些服务的
// 日志行数纹丝不动"，而用户无从分辨是哪一类 —— 尤其是"正在跑的服务"，
// 那恰恰是出问题、最需要日志的时候。
func TestSupervisorSetLogLimitEverywhere(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "for i in 1 2 3 4 5; do echo l$i; done; sleep 3000")
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	waitForLines(t, s.Log(svc.ID), 5, 5*time.Second)

	// 1) 正在跑的进程：改上限要**当场**裁它的缓冲。
	s.SetLogLimit(2)
	if got := s.Log(svc.ID).Limit(); got != 2 {
		t.Errorf("运行中服务的上限应立刻变 2，得 %d", got)
	}
	if got := s.Log(svc.ID).Len(); got != 2 {
		t.Errorf("运行中服务的缓冲应立刻裁到 2 行，得 %d", got)
	}

	// 2) 已退出服务的 last 缓冲。先停（缓冲进 last），再改上限。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Stop(ctx, svc, time.Second); err != nil {
		t.Fatal(err)
	}
	s.SetLogLimit(9)
	if got := s.Log(svc.ID).Limit(); got != 9 {
		t.Errorf("last 缓冲的上限也应被改到，得 %d", got)
	}

	// 3) 之后新建的缓冲用新上限（Log 给没跑过的服务也建新缓冲）。
	if got := s.Log(9999).Limit(); got != 9 {
		t.Errorf("新缓冲应用新上限，得 %d", got)
	}

	// 非法值（0/负）不改坏现值 —— 日志是排查服务问题的唯一入口，一个坏
	// 数字把它变成 1 行或 0 行，等于在最需要它的时候把工具抽掉。
	s.SetLogLimit(0)
	if got := s.Log(9999).Limit(); got != 9 {
		t.Errorf("非法值不该改动现值，得 %d", got)
	}
}
