package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/store"
	"litepanel/internal/terminal"
	"litepanel/internal/termws"
	"litepanel/internal/ws"
)

// 终端健康探测的装配验收（M5-T1）。
//
// 与 wireMetrics/wireServices 同一理由：接线最容易"两边都写对、中间忘了接"。
// terminal 包有自己的探测测试，api 包有自己的处理器测试，两边各自全绿而 main
// 里漏接的话，实际表现是终端页永远拿到 501，用户以为面板没做终端功能。
//
// 所以这里的测试必须调用**生产装配函数**，绝不在测试里自己调 terminal.Probe
// 再塞进 deps —— 那样生产漏接也测不出来。

// 面板启动时探测 tmux 并缓存。tmux 不会在面板运行期间自己装上或换版本，
// 反复 fork 子进程没有意义。
func TestTerminalHealthIsWired(t *testing.T) {
	db := openTestDB(t)
	const pwd = "term-wiring-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}

	// 只调生产装配函数，**不在测试里赋 deps.Term**：手动赋值就等于把
	// "main 到底接没接"这件事排除在测试之外，生产漏接也照样绿。
	// 为此 buildDeps 必须自己负责 Term 的装配（它不需要外部入参，
	// 正好适合；Metrics/Services 需要 source/interval/db，才留在 main 里接）。
	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, nil)
	if deps.Term == nil {
		t.Fatal("buildDeps 没接 Term：终端页会永远拿到 501")
	}

	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	req := httptest.NewRequest(http.MethodGet, "/api/term/health", nil)
	req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应 200（tmux 缺失也回 200 让前端画引导）, got %d body=%s",
			rec.Code, rec.Body.String())
	}
	// 用真 tmux 探测：本机（Termux）与目标 Ubuntu 都装了 tmux，
	// 所以必须 available=true。写成"true 或 false 都接受"就是恒真断言。
	var h terminal.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("响应体应能反解为 terminal.Health: %v (%s)", err, rec.Body.String())
	}
	if !h.Available {
		t.Fatalf("本机装了 tmux 却报不可用: %q", h.Reason)
	}
	if h.Version == "" {
		t.Error("必须回报真实版本号，引导文案要用")
	}
}

// 缓存必须是**进程级一次**，不是每次请求一次。
//
// 第一版这里写成"比较 20 次 Health() 的结果都相等"，那是**假断言**：
// tmux 版本在测试期间本来就不会变，不缓存也照样相等（把 once 摘掉之后
// 测试仍然绿，实测踩过）。缓存的成效是"少 fork 子进程"，所以必须数
// 底层探测被调了几次 —— 这就是 terminalHealth.probe 这个缝存在的唯一理由。
func TestTerminalProbeIsCached(t *testing.T) {
	var calls int
	p := wireTerminalHealth()
	p.probe = func() terminal.Health {
		calls++
		return terminal.Health{Available: true, Version: "3.6"}
	}

	first := p.Health()
	for i := 0; i < 20; i++ {
		if got := p.Health(); got != first {
			t.Fatalf("第 %d 次结果变了（说明没缓存）: %+v vs %+v", i, got, first)
		}
	}
	if calls != 1 {
		t.Fatalf("21 次 Health() 只该探测 1 次，实际 %d 次", calls)
	}
}

// ---- 会话 CRUD 与桥接的装配（M5-T6）----

// 生产装配必须真的把 term_sessions 表接成 HTTP 接口。
//
// 与上面 Term 装配同一理由：terminal 包有 Service 的集成测试、api 包有
// 处理器的替身测试，两边各自全绿而 main 忘了接的话，实际表现是终端页
// 只会让输出翻倍、两个标签互相覆盖光标，那种 bug 只在真 tmux + 真 WS 上暴露。
func TestTerminalBridgeIsWiredToHub(t *testing.T) {
	db := openTestDB(t)
	requireTermSessionsTable(t, db)
	const pwd = "bridge-wiring-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	hub := ws.NewHub()
	tw := wireTerminal(db, hub) // 生产装配函数，测试里绝不自己 new Manager
	if tw == nil || tw.Sessions == nil || tw.Bridge == nil {
		t.Fatal("wireTerminal 没装配完整（会话服务 / 桥接缺一不可）")
	}
	t.Cleanup(tw.Bridge.Close)

	deps := buildDeps(db, config.Config{}, hub, false, nil)
	deps.TermSessions = tw.Sessions
	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	// 会话由 REST 建（前端就是这么做的），桥接那边事先毫不知情
	req := httptest.NewRequest(http.MethodPost, "/api/term/sessions",
		strings.NewReader(`{"title":"两人一屏"}`))
	req.Header.Set("X-Requested-With", "litepanel")
	req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("建会话失败: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Session terminal.SessionMeta `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	name := created.Session.TmuxName
	t.Cleanup(func() { _ = terminal.KillSession(terminal.DefaultBin, name) })

	// 用**数字 id** 拼频道：前端拿到的 id 来自 REST，桥接的频道口径必须一致
	ch := "term:" + strconv.FormatInt(created.Session.ID, 10)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	a := dialWS(t, wsURL, token)
	defer a.Close()
	b := dialWS(t, wsURL, token)
	defer b.Close()
	for _, c := range []*websocket.Conn{a, b} {
		if err := c.WriteMessage(websocket.TextMessage,
			// 字段名是 t（见 ws.Frame）：写成 "op" 会被 readPump 当成
			// "不支持的帧类型"，订阅静默失败，然后这里一头雾水地等超时。
			[]byte(`{"t":"sub","ch":"`+ch+`"}`)); err != nil {
			t.Fatal(err)
		}
	}

	// 从 a 发命令（二进制上行帧：[1B 频道名长度][频道名]['k'+按键]）。
	// 走 a 而不是外部 tmux send-keys：与真实用户输入同一条路径，
	// 桥接会等控制模式握手就绪（外部注入会撞握手空窗，输出永久丢失）。
	marker := fmt.Sprintf("BRIDGE-%d", time.Now().UnixNano())
	frame := make([]byte, 0, 3+len(ch)+8)
	frame = append(frame, byte(len(ch)))
	frame = append(frame, ch...)
	frame = append(frame, []byte("kprintf 'X"+marker+"Y\n'\r")...)
	if err := a.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}
	want := "X" + marker + "Y"

	// 两边都必须看到同一份输出。只数二进制帧：文本帧是 sub_ok 回执与心跳。
	seen := func(c *websocket.Conn) string {
		deadline := time.Now().Add(12 * time.Second)
		var all strings.Builder
		for time.Now().Before(deadline) {
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			mt, data, err := c.ReadMessage()
			if err != nil {
				// 必须退出：连接已死时继续读，gorilla 会在累计 1000 次失败后
				// 主动 panic（"repeated read on failed websocket connection"），
				// 把一次本可报清楚的超时炸成整个测试进程崩溃。
				t.Logf("读取结束: %v（已收 %d 字节）", err, all.Len())
				return all.String()
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			all.Write(data)
			if strings.Contains(all.String(), want) {
				return all.String()
			}
		}
		return all.String()
	}
	if !strings.Contains(seen(a), want) {
		t.Fatalf("发起命令的标签没看到输出（装配/上行路由没接）")
	}
	// b 晚一步读：给它独立的读取窗口
	if !strings.Contains(seen(b), want) {
		t.Fatalf("另一个标签没看到同一份输出（扇出没接上）")
	}

	// control 连接必须恰好一条（两个浏览器共用一条 tmux 控制连接）
	if n := controlConns(t, name); n != 1 {
		t.Fatalf("会话 %s 上有 %d 条 control 连接，应为 1", name, n)
	}
}

// 桥接在启动时必须对账：D5 的核心承诺是"面板重启，终端里的任务不死"。
// 面板重启后如果 tmux 里活着的会话没被接管，侧栏会列出它们而点进去没反应。
func TestTerminalReconcileOnBoot(t *testing.T) {
	db := openTestDB(t)
	requireTermSessionsTable(t, db)
	// 先造一个"上次运行遗留"的会话（tmux + 库都有它）
	meta, err := terminal.CreateSessionMeta(db, terminal.SessionInput{Title: "上次遗留"})
	if err != nil {
		t.Fatal(err)
	}
	// 必须 Close：CreateSession 顺带 attach 了一条控制连接，攥着不关会一直
	// 挂在 tmux 上，下面的"对账后应有 1 条"就会数到 2（这个坑踩过一次，
	// 生产侧 Service.Create 里同样的漏关由 terminal 包的测试钉住）。
	sess, err := terminal.CreateSession(terminal.DefaultBin, meta.TmuxName,
		terminal.SessionOpts{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()
	t.Cleanup(func() { _ = terminal.KillSession(terminal.DefaultBin, meta.TmuxName) })

	// 模拟面板重启：新 hub、新桥接，然后走生产的启动对账
	hub := ws.NewHub()
	tw := wireTerminal(db, hub)
	t.Cleanup(tw.Bridge.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tw.Reconcile(ctx)

	id := strconv.FormatInt(meta.ID, 10)
	if got := tw.Bridge.Sessions(); !contains(got, id) {
		t.Fatalf("启动对账没接管遗留会话 %s: %v", id, got)
	}
	// 接管不是壳：桥接里的会话必须真的能收键。用 tmux 侧独立观测确认
	// 面板确实挂着一条控制连接（而不是只在 map 里记了一笔）。
	if n := controlConns(t, meta.TmuxName); n != 1 {
		t.Fatalf("对账后 tmux 侧应有 1 条控制连接, got %d", n)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// term_sessions 表必须由迁移建出来，且列齐全。装配层写的 SQL 一旦引用了
// 不存在的列，只会在用户第一次点"新建会话"时才炸。
func requireTermSessionsTable(t *testing.T, db *store.DB) {
	t.Helper()
	var got string
	err := db.SqlDB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='term_sessions'`).Scan(&got)
	if err != nil {
		t.Fatalf("迁移没建出 term_sessions: %v", err)
	}
	cols, err := db.SqlDB().Query(`SELECT name FROM pragma_table_info('term_sessions')`)
	if err != nil {
		t.Fatal(err)
	}
	defer cols.Close()
	have := map[string]bool{}
	for cols.Next() {
		var c string
		if err := cols.Scan(&c); err != nil {
			t.Fatal(err)
		}
		have[c] = true
	}
	for _, want := range []string{"id", "tmux_name", "title", "history_limit", "alive"} {
		if !have[want] {
			t.Fatalf("term_sessions 缺列 %s（实际列: %v）", want, have)
		}
	}
}

func TestTermSessionsMigrationIsApplied(t *testing.T) {
	db := openTestDB(t)
	requireTermSessionsTable(t, db)
}

// controlConns 数 tmux 自己看到的连接数，不靠面板自报：
// "面板说我只开了一条"与"真只开了一条"是两件事。
// 实测确认控制模式客户端会出现在 list-clients（带 control-mode 标记），
// 且 -t 精确过滤有效。
func controlConns(t *testing.T, name string) int {
	t.Helper()
	out, err := exec.Command(terminal.DefaultBin, "list-clients", "-t", "="+name).Output()
	if err != nil {
		return -1
	}
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

// store 生成的会话名必须能被桥接接管 —— 两侧前缀的端到端一致性检查。
//
// store 用 terminal.TmuxPrefix 造名字，termws.Manager 用自己私有的 "lp-"
// 解析名字。两边各自硬编码字面量时，改一边不会有编译错误、也不会让任何
// 单测失败，表现却是"侧栏列出了会话，点进去没反应"。所以这里用 store
// 真建一个会话，再让桥接真去接管它：前缀或 id 口径漂移会直接让 Ensure 报错。
func TestStoreSessionIsAttachableByBridge(t *testing.T) {
	db := openTestDB(t)
	requireTermSessionsTable(t, db)
	meta, err := terminal.CreateSessionMeta(db, terminal.SessionInput{Title: "前缀一致性"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.CreateSession(terminal.DefaultBin, meta.TmuxName,
		terminal.SessionOpts{Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("造真会话失败: %v", err)
	}
	t.Cleanup(func() { _ = terminal.KillSession(terminal.DefaultBin, meta.TmuxName) })

	hub := ws.NewHub()
	m := termws.NewManager(termHub{h: hub}, terminal.DefaultBin)
	t.Cleanup(m.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 桥接的 id 口径是"数字 id"（频道叫 term:<id>），必须与 store 的 int64
	// 主键互换得回来，否则 REST 给的 id 拼出的频道桥接根本不认
	if err := m.Ensure(ctx, strconv.FormatInt(meta.ID, 10)); err != nil {
		t.Fatalf("桥接接管 store 建的会话失败（前缀/口径漂移？）: %v", err)
	}
}
