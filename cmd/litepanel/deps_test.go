package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litepanel/internal/auth"
	"litepanel/internal/config"
	"litepanel/internal/filemgr"
	"litepanel/internal/logx"
	"litepanel/internal/store"
	"litepanel/internal/ws"
)

// -debug 必须把访问日志的接线字段接上：排查“某浏览器白屏”时，
// 唯一能定性“请求有没有到、哪个请求挂住”的证据就是服务端逐请求日志。
//
// 注意两层门（§12.1/D9）：字段接线是运行时的，两种构建都要验；
// 而“日志真的落笔”只在调试构建存在 —— 发布构建里挂载点被
// logx.Enabled 编译期剥离，release 下的零日志由 api 包的
// accesslog_release_test.go 把关。
func TestBuildDepsWiresAccessLogInDebug(t *testing.T) {
	db := openTestDB(t)
	var log bytes.Buffer

	deps := buildDeps(db, config.Config{}, ws.NewHub(), true, &log)

	if !deps.Debug {
		t.Error("debug=true 时 deps.Debug 必须为 true")
	}
	if deps.LogWriter == nil {
		t.Fatal("debug=true 时必须接上 LogWriter")
	}

	srv := newTestServer(t, deps)
	get(t, srv, "/api/me")
	if logx.Enabled {
		if !strings.Contains(log.String(), "/api/me") {
			t.Errorf("调试构建访问日志应含请求路径, got %q", log.String())
		}
	} else if log.Len() != 0 {
		t.Errorf("发布构建不得落任何日志, got %q", log.String())
	}
}

// 默认（不带 -debug）不得留请求日志：设计 D9 要求发布构建不写日志。
func TestBuildDepsSilentByDefault(t *testing.T) {
	db := openTestDB(t)
	var log bytes.Buffer

	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, &log)

	if deps.Debug {
		t.Error("默认 Debug 必须为 false")
	}

	srv := newTestServer(t, deps)
	get(t, srv, "/api/me")
	if log.Len() != 0 {
		t.Errorf("默认不该有日志, got %q", log.String())
	}
}

// 会话有效期与限流阈值是安全参数，装配错位会静默削弱安全性。
func TestBuildDepsSecurityParams(t *testing.T) {
	db := openTestDB(t)
	deps := buildDeps(db, config.Config{TLS: config.TLSConfig{Enabled: true}}, ws.NewHub(), false, nil)

	if !deps.SecureCookie {
		t.Error("启用 TLS 时 cookie 必须带 Secure")
	}
	if deps.Sessions == nil || deps.Limiter == nil {
		t.Fatal("Sessions / Limiter 必须装配")
	}

	// 未启用 TLS → Secure 必须关，否则明文 HTTP 下 cookie 根本存不住。
	db2 := openTestDB(t)
	if buildDeps(db2, config.Config{}, ws.NewHub(), false, nil).SecureCookie {
		t.Error("明文 HTTP 下 cookie 不该带 Secure")
	}

	// 锁定阈值：第 6 次错误必须被拒（429），阈值若被写松等于没有锁定。
	lim := auth.NewLoginLimiter(time.Now, 5, 10*time.Minute)
	for i := 0; i < 5; i++ {
		if !lim.Allow("1.2.3.4") {
			t.Fatalf("第 %d 次本应允许", i+1)
		}
		lim.Fail("1.2.3.4")
	}
	if lim.Allow("1.2.3.4") {
		t.Error("5 次失败后应锁定")
	}
}

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Files 必须在 buildDeps 里接上。
//
// 接在 buildDeps 而不是 main：它只需要 /proc 的位置，与其他需要外部入参
// 的模块不同。放在 main 里的话，"文件页是不是 501"就取决于 main 有没有
// 被跑到，装配测试覆盖不到 —— 而 501 是用户看到的"面板坏了"。
func TestBuildDepsWiresFiles(t *testing.T) {
	db := openTestDB(t)
	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, nil)
	if deps.Files == nil {
		t.Fatal("deps.Files 没接：文件页会整片 501")
	}
	// 真家伙能干活：列 /proc（任何 Linux/Android 上都在）。
	// 断言"能列出东西"而不是"非 nil"：一个返回空实现的假接线也能过
	// 非 nil 检查。
	page, err := deps.Files.List(t.Context(), "/proc", filemgr.ListOptions{Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) == 0 || page.Total == 0 {
		t.Fatalf("/proc 不可能列出空: %+v", page)
	}
}

// 磁盘枚举接的是真实 /proc/mounts：roots 空 = 地址栏没有一个盘。
func TestBuildDepsFilesRootsWork(t *testing.T) {
	db := openTestDB(t)
	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, nil)
	roots, err := deps.Files.Roots(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) == 0 {
		t.Fatal("roots 为空：地址栏会显示“这台机器没有磁盘”")
	}
}
