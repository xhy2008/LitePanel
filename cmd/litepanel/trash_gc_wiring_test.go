package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"litepanel/internal/config"
	"litepanel/internal/filemgr"
	"litepanel/internal/ws"
)

// 回收站过期条目必须有人定时清掉（设计 8.6：每小时一次，保留 3 天）。
//
// 没接的症状和用户能察觉到的症状之间隔着整整 3 天，而且方向是"该删的
// 没删"：磁盘一点点被"我已经删掉的文件"吃掉，而回收站界面里那些条目
// 看起来完全正常（它们确实还在保留期内 —— 只是那个期限永远到不了）。
// 配置项 trash_retain_days 也会变成一个纯粹的摆设：改它没有任何可见
// 后果，没人会去怀疑一个数字没被用上。
func TestStartTrashJanitorCleansExpired(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	disk := filepath.Join(dir, "disk")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := filemgr.NewService(filemgr.Options{
		Clock: func() time.Time { return now },
		FilesystemRoot: func(string) (string, error) {
			return disk, nil
		},
		TrashRoots: func(context.Context) ([]string, error) {
			return []string{disk}, nil
		},
		TrashRetain: 72 * time.Hour,
	})
	deps := buildDeps(db, config.Config{DBPath: filepath.Join(dir, "litepanel.db")}, ws.NewHub(), false, nil)
	deps.Files = svc

	src := filepath.Join(disk, "过期.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Delete(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	if items, _ := svc.ListTrash(t.Context()); len(items) != 1 {
		t.Fatalf("前置：回收站该有 1 条, got %d", len(items))
	}
	// 时钟推过保留期
	now = now.Add(73 * time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if !startTrashJanitor(ctx, deps, time.Hour) {
		t.Fatal("回收站清理未接")
	}
	if items, _ := svc.ListTrash(t.Context()); len(items) != 0 {
		t.Errorf("过期条目应被清掉, 剩 %v", items)
	}
}

// 未过期的一条都不能动。
//
// 与上一条配对才成立：只测"过期的被删了"的话，把 CleanTrash 写成
// EmptyTrash 同样能过。
func TestStartTrashJanitorKeepsFresh(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := filemgr.NewService(filemgr.Options{
		FilesystemRoot: func(string) (string, error) { return disk, nil },
		TrashRoots: func(context.Context) ([]string, error) {
			return []string{disk}, nil
		},
	})
	deps := buildDeps(db, config.Config{DBPath: filepath.Join(dir, "litepanel.db")}, ws.NewHub(), false, nil)
	deps.Files = svc

	src := filepath.Join(disk, "刚删的.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Delete(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if !startTrashJanitor(ctx, deps, time.Hour) {
		t.Fatal("没接上")
	}
	if items, _ := svc.ListTrash(t.Context()); len(items) != 1 {
		t.Errorf("保留期内的条目被后台清掉了 —— 用户以为能还原，实际已经没了: %v", items)
	}
}

// ctx 已经结束时，过期条目一条都不该被动。
//
// 保证这件事的是 CleanTrash 内部的逐条 ctx 检查，不是 janitor 里的某个
// 前置 return —— 第一版在这里加过一道 ctx.Err() 早退，实测没有任何测试
// 能区分加与不加（CleanTrash 已经拦住了），于是把它删了：没有行为的代码
// 会在下次重构时被人当成"有测试保护"而不敢动。
// 本测试仍然值得留：它钉的是 janitor 与领域层**合起来**的那个对外承诺
// （关停中不动用户的文件），而不是某一层的某一行。
func TestStartTrashJanitorSkipsWhenCanceled(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	disk := filepath.Join(dir, "disk")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := filemgr.NewService(filemgr.Options{
		Clock:          func() time.Time { return now },
		FilesystemRoot: func(string) (string, error) { return disk, nil },
		TrashRoots: func(context.Context) ([]string, error) {
			return []string{disk}, nil
		},
		// 24h 是下限：再小也会被 NewService 夹上来（见下面的注释）
		TrashRetain: 24 * time.Hour,
	})
	deps := buildDeps(db, config.Config{DBPath: filepath.Join(dir, "litepanel.db")}, ws.NewHub(), false, nil)
	deps.Files = svc
	src := filepath.Join(disk, "x.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Delete(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	now = now.Add(25 * time.Hour) // 越过 24h 保留期

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !startTrashJanitor(ctx, deps, time.Hour) {
		t.Fatal("没接上")
	}
	if items, _ := svc.ListTrash(t.Context()); len(items) != 1 {
		t.Error("ctx 已结束时不该动手")
	}
}

// 没装配文件服务时必须明说"没在清"。
//
// 同 janitor 的理由：漏接回收站清理没有任何当下症状（3 天后才发现磁盘
// 被"已删除"的文件占着），所以它必须自己报告在不在跑。
func TestStartTrashJanitorReportsNotWired(t *testing.T) {
	db := openTestDB(t)
	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, nil)
	deps.Files = nil
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if startTrashJanitor(ctx, deps, time.Hour) {
		t.Error("Files 没接时不该谎称清理在跑")
	}
}

// 传 0 间隔不能把面板弄崩。
//
// time.NewTicker(0) 直接 panic —— 而调用方传 0 是完全可能的（将来把间隔
// 变成配置项、或者从别处传一个没初始化的值）。janitor 在 main  goroutine
// 里启动，panic 会让整个面板起不来：比"清理没跑"严重得多。
func TestStartTrashJanitorZeroIntervalDoesNotPanic(t *testing.T) {
	db := openTestDB(t)
	deps := buildDeps(db, config.Config{DBPath: filepath.Join(t.TempDir(), "x.db")}, ws.NewHub(), false, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// 断言的是"不 panic 且回 true"：真正的定时器行为由上一条测试覆盖。
	if !startTrashJanitor(ctx, deps, 0) {
		t.Error("接上了就该回 true")
	}
}

// 定时器那一轮必须真的会再清一次（不能只在启动时清一遍）。
//
// 里程碑 ⑥ 的验收标准是"回收站超期文件被自动永久删除"，而面板通常连续
// 跑很多天：只有启动那一轮的 janitor，等于"面板重启才清一次"——条目会
// 在回收站里住上几周，直到某次升级把它顺带治好，然后没人知道它坏过。
//
// 条目是在 janitor 启动**之后**才造出来的，所以"它被清掉了"只可能来自
// 定时器那一轮，不可能是启动清扫的余效。间隔取毫秒级，否则这条测试要
// 真跑一小时。
func TestStartTrashJanitorKeepsRunning(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	// 假时钟必须可并发读写：这条测试里推时钟的是测试 goroutine，读的
	// 是 janitor 的 goroutine，两者之间没有任何 happens-before 边 —— 裸
	// time.Time 变量在数据竞争下阅读器可能永远看不到新值（表现就是
	// "janitor 死活不清"，而实现是对的）。前几条测试不需要锁，是因为
	// 推完时钟才调 startTrashJanitor，清扫跑在同一个 goroutine 里。
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	clk := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
	disk := filepath.Join(dir, "disk")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := filemgr.NewService(filemgr.Options{
		Clock:          clk,
		FilesystemRoot: func(string) (string, error) { return disk, nil },
		TrashRoots: func(context.Context) ([]string, error) {
			return []string{disk}, nil
		},
		// 24h 是下限：再小也会被 NewService 夹上来（见下面的注释）
		TrashRetain: 24 * time.Hour,
	})
	deps := buildDeps(db, config.Config{DBPath: filepath.Join(dir, "litepanel.db")}, ws.NewHub(), false, nil)
	deps.Files = svc

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if !startTrashJanitor(ctx, deps, 5*time.Millisecond) {
		t.Fatal("没接上")
	}
	// 启动清扫已经跑过了（此刻回收站是空的），现在才造一条并让它过期
	src := filepath.Join(disk, "运行中过期.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Delete(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	advance(25 * time.Hour)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if items, _ := svc.ListTrash(t.Context()); len(items) == 0 {
			return // 定时器清掉了
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("定时器那一轮没在清：janitor 只会在启动时扫一次")
}
