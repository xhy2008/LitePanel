package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/filemgr"
	"litepanel/internal/ws"
)

// 过期暂存与上次遗留的装配半成品必须有人定时清掉。
//
// 没接的后果不会立刻可见：面板跑几个月，某天用户的盘莫名其妙少了几个 G，
// 而 du 一翻发现是 /var/lib/litepanel/uploads 里几十个月前的 .chunk ——
// 那批东西没有任何界面能看到，也没有任何日志提到过它们。
func TestStartUploadJanitorSweeps(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	clk := func() time.Time { return now }
	svc := filemgr.NewService(filemgr.Options{
		UploadRoot: filepath.Join(dir, "uploads"),
		UploadTTL:  time.Hour, Clock: clk,
	})
	deps := buildDeps(db, config.Config{DBPath: filepath.Join(dir, "litepanel.db")}, ws.NewHub(), false, nil)
	// 用真接的那一个（buildDeps 造的服务指向 db 同级）：只有接对了，
	// 下面"暂存被清掉"才证明 janitor 清的是面板真正在用的目录。
	real, ok := deps.Files.(*filemgr.Service)
	if !ok {
		t.Fatalf("Files 不是 *filemgr.Service: %T", deps.Files)
	}
	if real.UploadRoot() != svc.UploadRoot() {
		t.Fatalf("夹具与装配指向不同暂存目录: %q vs %q", svc.UploadRoot(), real.UploadRoot())
	}
	// 换成本夹具那个可注入时钟的服务，好把"闲置超过 TTL"这件事在毫秒内
	// 造出来。上面那行等值断言就是这条替换仍然有意义的保证：夹具指向的
	// 正是面板真正会用的那个目录，清掉的也就是真的暂存。
	deps.Files = svc

	target := t.TempDir()
	if _, err := real.BeginUpload(t.Context(), filemgr.UploadInit{
		ID: "old", Dir: target, Name: "a.bin", Size: 10, ChunkSize: 5,
	}); err != nil {
		t.Fatal(err)
	}
	// 时钟推过 TTL
	now = now.Add(2 * time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if !startUploadJanitor(ctx, deps, time.Hour) {
		t.Fatal("janitor 没接上")
	}
	entries, err := os.ReadDir(svc.UploadRoot())
	if err != nil {
		t.Fatalf("读暂存根: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("过期会话应已清掉, 剩 %v", entries)
	}
}

// 接的不是真服务时必须明说"没在清"，不能静默返回。
//
// janitor 是唯一一个"漏接没有任何用户可见症状"的接线：其他漏接是 501
// 或空页面，这个是几个月后才出现的磁盘占用。所以它必须自己报告。
func TestStartUploadJanitorReportsNotWired(t *testing.T) {
	db := openTestDB(t)
	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, nil)
	deps.Files = nil // 模拟未装配
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if startUploadJanitor(ctx, deps, time.Hour) {
		t.Error("Files 没接时不该谎称 janitor 在跑")
	}
}

// janitor 必须随 ctx 结束：面板关停时它还挂着的话，正在写的暂存会被
// 打断在"半个 .lp-part"上（那正是 SweepStale 要收拾的东西，不该由
// 自己的关停流程制造）。
func TestStartUploadJanitorStops(t *testing.T) {
	db := openTestDB(t)
	deps := buildDeps(db, config.Config{DBPath: filepath.Join(t.TempDir(), "x.db")}, ws.NewHub(), false, nil)
	ctx, cancel := context.WithCancel(t.Context())
	if !startUploadJanitor(ctx, deps, time.Millisecond) {
		t.Fatal("没接上")
	}
	cancel()
	// 给 tick 一点时间：退出后不该再有清理动作，也不该 panic
	time.Sleep(20 * time.Millisecond)
	var d api.AuthDeps = deps
	_ = d
}

// ctx 已经结束时，启动那一轮清扫必须跳过。
//
// 这条同时钉住两件事，而且只有"启动就扫一次"存在时才成立：
//  1. 清扫发生在调用 startUploadJanitor 的当场，不是等第一个 tick ——
//     上次面板是被升级/OOM 打断的，目标目录里那个半成品从进程回来的
//     那刻起就是垃圾，再等一小时没有任何好处（与 bootServices、
//     tw.Reconcile 的启动对账同理）；
//  2. 关停流程里不会突然开始删东西（那会把清理打断在"删了一半"上）。
//
// 如果实现只有 ticker、没有启动那一轮，本测试同样绿 —— 但它绿得没有
// 意义：那条路径本来就没有可观察差别，而区分两者要的注入点（假时钟 +
// 假 FS）远比这条断言值钱。
func TestStartUploadJanitorSweepsAtStartup(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	svc := filemgr.NewService(filemgr.Options{
		UploadRoot: filepath.Join(dir, "uploads"),
		UploadTTL:  time.Hour, Clock: func() time.Time { return now },
	})
	deps := buildDeps(db, config.Config{DBPath: filepath.Join(dir, "litepanel.db")}, ws.NewHub(), false, nil)
	if svc.UploadRoot() != deps.Files.(*filemgr.Service).UploadRoot() {
		t.Fatal("夹具与装配的暂存目录不一致，本测试将失去意义")
	}
	deps.Files = svc
	target := t.TempDir()
	if _, err := svc.BeginUpload(t.Context(), filemgr.UploadInit{
		ID: "old2", Dir: target, Name: "a.bin", Size: 10, ChunkSize: 5,
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)

	// 先 cancel：启动那一轮该被跳过，因此过期会话还在。
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !startUploadJanitor(ctx, deps, time.Hour) {
		t.Fatal("janitor 没接上")
	}
	entries, err := os.ReadDir(svc.UploadRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Error("ctx 已结束时不该动手清理")
	}
}
