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

// 上传暂存目录必须接成一个**绝对路径**，而且在 db 的同级目录里。
//
// 空值的后果不是"报错"，而是更糟的静默：filemgr 会把暂存建在进程的
// 相对路径下，也就是 systemd 的 WorkingDirectory（多半是 /）或用户
// 随手启动时所在的目录 —— 前者直接权限失败（用户看到的是"上传坏了"），
// 后者在一个谁都没预期的地方攒出几百个文件，而且面板换了启动目录后
// 旧暂存再也不会被 GC 认出来，永远留在盘上。
//
// 选 db 同级是因为它必然可写（面板正在往那里写 SQLite）且必然属于
// 面板自己，而不是某个用户的数据目录。
func TestBuildDepsWiresUploadRoot(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "litepanel.db")}
	deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
	svc, ok := deps.Files.(*filemgr.Service)
	if !ok {
		t.Fatalf("Files 不是 *filemgr.Service: %T", deps.Files)
	}
	root := svc.UploadRoot()
	if root == "" {
		t.Fatal("UploadRoot 没接：暂存会落在进程 cwd 的相对路径下")
	}
	if !filepath.IsAbs(root) {
		t.Errorf("UploadRoot 必须是绝对路径, got %q", root)
	}
	if filepath.Dir(root) != filepath.Dir(cfg.DBPath) {
		t.Errorf("暂存该与 db 同级: %q vs %q", root, cfg.DBPath)
	}
	// 真能建会话：这一步同时证明那条路径真的可写（配置指向只读目录时，
	// "非空字符串"这种断言照样绿，而用户第一次上传就失败）。
	st, err := deps.Files.BeginUpload(t.Context(), filemgr.UploadInit{
		ID: "wire", Dir: t.TempDir(), Name: "a.bin", Size: 4, ChunkSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != "wire" {
		t.Errorf("会话没建出来: %+v", st)
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

// 回收站的两项配置必须真的接进文件服务。
//
// 光加配置键不接线的话一切照跑、什么都不会坏 —— 除了用户改的值不起
// 作用（改目录名/保留天数没有任何可见症状，不像漏接上传根那样有 501
// 或空页面）。所以直接把值灌进去再读回来：断言的是"配置到此为止都是
// 活的"。
func TestBuildDepsWiresTrashConfig(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Config{
		DBPath:          filepath.Join(t.TempDir(), "litepanel.db"),
		TrashDirName:    ".trash测试",
		TrashRetainDays: 9,
	}
	deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
	svc, ok := deps.Files.(*filemgr.Service)
	if !ok {
		t.Fatalf("Files 不是 *filemgr.Service: %T", deps.Files)
	}
	if got := svc.TrashDirName(); got != ".trash测试" {
		t.Errorf("回收站目录名没接上: %q", got)
	}
	if got := svc.TrashRetain(); got != 9*24*time.Hour {
		t.Errorf("保留期没接上: %v", got)
	}
}
