package main

// 下载模块的装配测试（M7-T3 的最后一根线）。
//
// 与 jobs_wiring_test.go 同一条纪律：**只调用生产侧的装配函数**，测试里绝不
// 出现 download 包的内部注入口、也不手工调 hub.Broadcast。判据是端到端的现象
// —— 假 aria2 在跑，从 buildDeps 交出来的 deps 提交一个下载，然后：
//
//   - GET /api/dl/tasks 能看到它（证明 RPC 地址从配置传进去了；接错地址的
//     后果是这一句 503，而 503 在用户眼里是"aria2 没装"，方向完全错）；
//   - aria2 推一个 complete 事件后，历史里那条**落库**成了 complete，且
//     订阅了 downloads 的 WS 连接收到了它（证明 notifier 两头都接上了：
//     只接 WS 的话，重启后面板会"忘掉"自己下过什么；只接落库的话，界面
//     永远不刷新）。
//
// 假 aria2 而不是真 aria2c：真进程能让"起不来"这一类根本没法构造，而这里要
// 验的是装配而不是协议（协议在 internal/download 用 httptest + 实测覆盖）。
// 假服务器实现的是**线协议**（JSON-RPC 方法名与返回形状），不是 Go 接口，
// 所以装配里的地址/路径/密钥拼错都照样测得出来。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/store"
	"litepanel/internal/ws"

	"github.com/gorilla/websocket"
)

// fakeAria2 是一个最小可用的 aria2 JSON-RPC 服务（HTTP + WS 同端点）。
type fakeAria2 struct {
	mu      sync.Mutex
	tasks   map[string]map[string]any // gid -> tellStatus 的返回
	wsConns []*websocket.Conn
	added   []string // 收到的 addUri 的 uris（拼起来比对）
	server  *httptest.Server
	nextGID int
	onAddWS func() // 每次 addUri 后调用（用来推事件）
}

func newFakeAria2(t *testing.T) *fakeAria2 {
	t.Helper()
	f := &fakeAria2{tasks: map[string]map[string]any{}, nextGID: 1}
	mux := http.NewServeMux()
	mux.HandleFunc("/jsonrpc", f.handle)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAria2) url() string { return f.server.URL + "/jsonrpc" }
func (f *fakeAria2) wsURL() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http") + "/jsonrpc"
}

type rpcReq struct {
	Method string `json:"method"`
	Params []any  `json:"params"`
	ID     any    `json:"id"`
}

func (f *fakeAria2) handle(w http.ResponseWriter, r *http.Request) {
	var req rpcReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	reply := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	switch req.Method {
	case "aria2.getVersion":
		reply(map[string]any{"version": "1.37.0", "enabledFeatures": []string{"IPv6", "HTTP/HTTPS"}})
	case "aria2.addUri":
		gid := f.addTaskLocked(req.Params)
		reply(gid)
		if f.onAddWS != nil {
			f.onAddWS()
		}
	case "aria2.tellStatus":
		gid, _ := req.Params[0].(string)
		f.mu.Lock()
		st, ok := f.tasks[gid]
		f.mu.Unlock()
		if !ok {
			errReply(w, req.ID, 1, "Download not present.")
			return
		}
		reply(st)
	case "aria2.tellActive", "aria2.tellWaiting":
		want := "active"
		if req.Method == "aria2.tellWaiting" {
			want = "waiting"
		}
		f.mu.Lock()
		var out []any
		for _, st := range f.tasks {
			if st["status"] == want {
				out = append(out, st)
			}
		}
		f.mu.Unlock()
		reply(out)
	case "aria2.getGlobalStat":
		f.mu.Lock()
		n := len(f.tasks)
		f.mu.Unlock()
		reply(map[string]any{"numActive": fmt.Sprint(n), "numWaiting": "0",
			"numStopped": "0", "downloadSpeed": "0", "uploadSpeed": "0"})
	case "aria2.pause", "aria2.pauseDownload", "aria2.unpause", "aria2.unpauseDownload",
		"aria2.remove", "aria2.forceRemove", "aria2.removeDownloadResult":
		gid, _ := req.Params[0].(string)
		f.mu.Lock()
		st, ok := f.tasks[gid]
		if ok && (req.Method == "aria2.remove" || req.Method == "aria2.forceRemove") {
			st["status"] = "removed"
		}
		if req.Method == "aria2.removeDownloadResult" {
			delete(f.tasks, gid)
		}
		if req.Method == "aria2.pause" || req.Method == "aria2.pauseDownload" {
			if ok {
				st["status"] = "paused"
			}
		}
		if req.Method == "aria2.unpause" || req.Method == "aria2.unpauseDownload" {
			if ok {
				st["status"] = "active"
			}
		}
		f.mu.Unlock()
		if !ok {
			errReply(w, req.ID, 1, "Download not present.")
			return
		}
		reply("OK")
	case "aria2.tellStopped":
		reply([]any{})
	default:
		errReply(w, req.ID, -32601, "Method not found: "+req.Method)
	}
}

func errReply(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg}})
}

// addTaskLocked 收下 addUri 的参数并登记一个新任务（状态 active）。
func (f *fakeAria2) addTaskLocked(params []any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var uris []string
	if len(params) >= 1 {
		if arr, ok := params[0].([]any); ok {
			for _, u := range arr {
				if s, ok := u.(string); ok {
					uris = append(uris, s)
				}
			}
		}
	}
	f.nextGID++
	gid := fmt.Sprintf("%016x", f.nextGID)
	f.added = append(f.added, strings.Join(uris, ","))
	f.tasks[gid] = map[string]any{
		"gid": gid, "status": "active", "totalLength": "0",
		"completedLength": "0", "downloadSpeed": "0", "connections": "1",
		"errorCode": "0", "errorMessage": "",
	}
	return gid
}

func (f *fakeAria2) setStatus(gid, status string, extra map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.tasks[gid]
	if !ok {
		return
	}
	st["status"] = status
	for k, v := range extra {
		st[k] = v
	}
}

func (f *fakeAria2) firstGID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for g := range f.tasks {
		return g
	}
	return ""
}

func (f *fakeAria2) pushNotification(method, gid string) {
	f.mu.Lock()
	conns := append([]*websocket.Conn(nil), f.wsConns...)
	f.mu.Unlock()
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method,
		"params": []any{map[string]any{"gid": gid}}})
	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		_ = c.WriteMessage(websocket.TextMessage, msg)
	}
}

// WS 端点：aria2 在同一个 /jsonrpc 路径上同时接受 HTTP 与 WS。
func (f *fakeAria2) serveWS(t *testing.T) {
	t.Helper()
	up := websocket.Upgrader{}
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Upgrade"), "websocket") {
			f.handle(w, r)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.wsConns = append(f.wsConns, c)
		f.mu.Unlock()
		// 读到断开为止（事件桥不发请求体，这里也不解析内容）。
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})
}

// 装配的主线：配置里的地址要用起来，端点要能从假 aria2 拿到数据。
func TestWiringDownloadsUsesConfiguredURL(t *testing.T) {
	fake := newFakeAria2(t)
	fake.serveWS(t)
	db := openTestDB(t)
	hub := ws.NewHub()
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(t.TempDir(), "p.db")
	cfg.Aria2RPCURL = fake.url()

	deps := buildDeps(db, cfg, hub, false, nil)
	if deps.Downloads == nil {
		t.Fatal("buildDeps 没接 Downloads —— 下载页会整片 501")
	}
	// 起后台循环（等于 run() 里那一步）。
	w := startDownloads(deps.Downloads)
	defer w.Stop()

	const pwd = "dlwire-pass-1234"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	resp := getJSON(t, srv, "/api/dl/health", token)
	if resp["ok"] != true {
		t.Fatalf("健康检查没连上配置里的地址（接错 URL 的表现）: %v", resp)
	}

	// 从 HTTP 端点提交一个下载（走完整的 handler→service→RPC 链）。
	body := map[string]any{"uris": []string{"https://example.com/a.bin"}, "split": 4}
	created := postJSON(t, srv, "/api/dl/tasks", token, body)
	if created["gid"] == nil || created["gid"] == "" {
		t.Fatalf("提交没回 gid: %v", created)
	}
	if len(fake.added) != 1 || !strings.Contains(fake.added[0], "example.com/a.bin") {
		t.Errorf("假 aria2 没收到 addUri: %v", fake.added)
	}

	// 列表端点要能看到它 —— 这一句证明读路径用的是同一个地址。
	list := getJSON(t, srv, "/api/dl/tasks", token)
	items, _ := list["tasks"].([]any)
	if len(items) != 1 {
		t.Fatalf("列表应 1 条, got %v", list["tasks"])
	}
}

// 事件要从 aria2 的 WS 一路走到前端频道 **并且**落库。
//
// 两个出口分别验证：WS 那一侧用真订阅连接；落库那一侧用"重启后再看一次"
// —— 新起一个 Service 读同一个 DB，历史里必须有它且是 complete。那正是
// 本地表存在的全部理由（aria2 跨重启不留终态记录）。
func TestWiringDownloadEventReachesWSAndDB(t *testing.T) {
	fake := newFakeAria2(t)
	fake.serveWS(t)
	hub := ws.NewHub()
	cfg := config.Defaults()
	// 两个 Service 必须读写**同一个库文件**，否则"重启后还在"测的是两个
	// 不相干的库（曾经这里误用 openTestDB，它自带一个临时路径，于是
	// "重启"读到一个空库，测试永远红）。
	dbPath := filepath.Join(t.TempDir(), "p.db")
	cfg.DBPath = dbPath
	db := openTestDBAt(t, dbPath)
	cfg.Aria2RPCURL = fake.url()

	deps := buildDeps(db, cfg, hub, false, nil)
	w := startDownloads(deps.Downloads)

	const pwd = "dlwire2-pass-1234"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	// 订阅 downloads 频道（必须在提交之前，否则收不到事件）。
	c := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", token)
	defer c.Close()
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"ch":"downloads","t":"sub"}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return hub.SubscriberCount("downloads") == 1 }, "订阅 downloads 未生效")

	body := map[string]any{"uris": []string{"https://example.com/b.bin"}}
	created := postJSON(t, srv, "/api/dl/tasks", token, body)
	gid, _ := created["gid"].(string)
	if gid == "" {
		t.Fatal("提交没回 gid")
	}

	// 事件桥连上假 aria2 需要一点时间（拨号 + 全量补齐）。等它连上再推事件，
	// 否则事件会丢在连接建立之前 —— 那是测试的时序问题而不是产品的。
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.wsConns) >= 1
	}, "事件桥没连上 aria2 的 WS")
	time.Sleep(200 * time.Millisecond) // 让全量补齐先跑完

	fake.setStatus(gid, "complete", map[string]any{
		"totalLength": "4096", "completedLength": "4096", "downloadSpeed": "0"})
	fake.pushNotification("aria2.onDownloadComplete", gid)

	// 1) WS 侧：收到一条 gid 与 kind 都对的事件。
	deadline := time.Now().Add(8 * time.Second)
	var sawEvent bool
	for time.Now().Before(deadline) && !sawEvent {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := c.ReadMessage()
		if err != nil {
			continue
		}
		var frame struct {
			Ch string `json:"ch"`
			T  string `json:"t"`
			D  struct {
				Kind string `json:"kind"`
				GID  string `json:"gid"`
			} `json:"d"`
		}
		if json.Unmarshal(msg, &frame) != nil {
			continue
		}
		if frame.Ch == "downloads" && frame.D.GID == gid && frame.D.Kind == "completed" {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Fatal("8s 内没从 downloads 频道收到 complete 事件（事件桥或 WS 广播漏接）")
	}

	// 2) 落库侧：换一个 Service 读同一个 DB（模拟面板重启）。
	w.Stop()
	hub2 := ws.NewHub()
	deps2 := buildDeps(openTestDBAt(t, dbPath), cfg, hub2, false, nil)
	tasks, err := deps2.Downloads.Tasks(context.Background())
	if err != nil {
		t.Fatalf("重启后读历史: %v", err)
	}
	var found bool
	for _, tk := range tasks {
		if tk.GID == gid {
			found = true
			if tk.State != "complete" {
				t.Errorf("终态没落库（重启后变回 %q）", tk.State)
			}
			if tk.TotalBytes != 4096 {
				t.Errorf("终态要带大小，否则历史里是 0 B: %d", tk.TotalBytes)
			}
		}
	}
	if !found {
		t.Error("重启后历史里没有这条任务（事件没落库，或库根本没接）")
	}
}

// aria2 没起时装配必须安全（面板不能因此起不来，也不能崩在后台循环里）。
func TestWiringDownloadsSafeWhenAria2Down(t *testing.T) {
	db := openTestDB(t)
	hub := ws.NewHub()
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(t.TempDir(), "p.db")
	// 一个必然没人监听端口：与"aria2 没装"等价，且不需要真的去停谁。
	cfg.Aria2RPCURL = "http://127.0.0.1:1/jsonrpc"

	deps := buildDeps(db, cfg, hub, false, nil)
	w := startDownloads(deps.Downloads)
	defer w.Stop()

	const pwd = "dlwire3-pass-1234"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	// 数据类端点要 503（不是 200 + 空列表，也不是 500）。
	res := rawGet(t, srv, "/api/dl/tasks", token)
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("aria2 不可达应 503, got %d", res.StatusCode)
	}
	// health 仍 200（aria2 的状态是数据不是错误）。
	hres := rawGet(t, srv, "/api/dl/health", token)
	defer hres.Body.Close()
	if hres.StatusCode != http.StatusOK {
		t.Errorf("health 应恒 200, got %d", hres.StatusCode)
	}
}

// 关停不能挂住。
//
// 事件桥的读循环会卡在 ReadMessage 上，若 ctx 取消时不主动断开连接，
// "优雅关停"就变成"等 systemd 的 SIGKILL"，整台面板（含托管服务）跟着多活
// 一分半。这里给一个硬超时。
func TestWiringDownloadStopIsPrompt(t *testing.T) {
	fake := newFakeAria2(t)
	fake.serveWS(t)
	db := openTestDB(t)
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(t.TempDir(), "p.db")
	cfg.Aria2RPCURL = fake.url()
	deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
	w := startDownloads(deps.Downloads)
	time.Sleep(300 * time.Millisecond) // 让事件桥连上

	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 超过 5s 没返回（事件桥的读循环没随 ctx 取消而退出）")
	}
}

// WSURLForRPC 的协议换算：https 必须变 wss。
//
// 不换算的失败方式很难查 —— 表现是 WS 握手一直超时，而 HTTP 那边一切正常，
// 于是"能提交任务、看不到事件推送"，运维会去查 aria2 的日志而那里什么都没有。
func TestWSURLForRPC(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:6800/jsonrpc", "ws://127.0.0.1:6800/jsonrpc"},
		{"https://aria2.internal:6800/jsonrpc", "wss://aria2.internal:6800/jsonrpc"},
		{"  http://localhost:6800/jsonrpc  ", "ws://localhost:6800/jsonrpc"},
		{"ftp://127.0.0.1:6800/jsonrpc", ""},
		{"", ""},
		{"不是 URL", ""},
	} {
		if got := wsURLForRPC(tc.in); got != tc.want {
			t.Errorf("wsURLForRPC(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

// openTestDBAt 在**已有**的库文件上再开一个连接（模拟面板重启：新的进程、
// 同一个库）。与 openTestDB 的区别就是它不新建临时路径。
func openTestDBAt(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// rawGet 带会话 cookie 发一个 GET，返回原始响应（要断状态码的场合）。
func rawGet(t *testing.T, srv *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: token})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getJSON(t *testing.T, srv *httptest.Server, path, token string) map[string]any {
	t.Helper()
	resp := rawGet(t, srv, path, token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 应 200, got %d", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// postJSON 带会话 cookie 与 CSRF 头发 POST（缺后者会被 403，装配测试就会
// 全体红在一个与装配无关的地方）。
func postJSON(t *testing.T, srv *httptest.Server, path, token string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "litepanel")
	req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: token})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s 失败 %d: %s", path, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// 装配遇到非法地址时不能 panic，也不能把 503 的原因写成别的地址。
//
// 这条测的是"下载配置写错"这一整类：NewClient 失败后装配层塞的是
// UnavailableClient 而不是 nil。用 nil 的后果是每个下载请求都在对 nil 调
// 方法时 panic —— HTTP 层会把它记成 500 并写一条访问日志，而面板管着用户的
// 服务（托管的 nginx 也在同一进程里被监管），一个可选模块不该有那种杀伤力。
func TestWiringDownloadsBadURLNoPanic(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(t.TempDir(), "p.db")
	// 绕过 config.Validate 直接喂给 buildDeps：模拟的是"校验之外还有别的路径
	// 把配置塞进来"（热重载、测试、未来的设置页写入），装配层自己得站住。
	cfg.Aria2RPCURL = "\x7f 不是个 URL"
	deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
	if deps.Downloads == nil {
		t.Fatal("即使地址非法也要接上（503 + 原因），不能留 nil 让下游 panic")
	}
	w := startDownloads(deps.Downloads)
	defer w.Stop()

	const pwd = "dlbad-pass-1234"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	res := rawGet(t, srv, "/api/dl/tasks", token)
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("地址非法应 503, got %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	// detail 里必须带**真实原因**与用户填的地址，而不是某个替代品地址。
	if !strings.Contains(string(body), "不是个 URL") {
		t.Errorf("503 的 detail 要带真实原因, got %s", body)
	}
}
