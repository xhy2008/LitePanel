package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/metrics"
	"litepanel/internal/ws"
)

// D6 的装配验收：hub 的订阅计数必须真的接到采集器上。
//
// 这一层单独测是因为它最容易"两边都写对、中间忘了接"：
// Collector 的启停有 metrics 包的测试，hub 的计数回调有 ws 包的测试，
// 但若 main 里漏了 OnCount 接线，两边各自全绿，实际行为却是采集器永远在跑
// —— 面板在无人观看的夜里持续吃 CPU，正是 D6 要消灭的东西。
func TestHubSubscriptionDrivesCollector(t *testing.T) {
	db := openTestDB(t)
	const pwd = "wiring-test-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	hub := ws.NewHub()
	src := &probeSource{}
	// 用生产的装配函数：测试里绝不出 OnCount，否则生产漏接也测不出来。
	col := wireMetrics(hub, src, 5*time.Millisecond)
	defer col.Stop()

	deps := buildDeps(db, config.Config{}, hub, false, nil)
	deps.Metrics = col

	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	// 未订阅：一次采样都不该发生。
	time.Sleep(50 * time.Millisecond)
	if n := src.calls.Load(); n != 0 {
		t.Fatalf("无 WS 订阅时采集了 %d 次（hub 与采集器没接上，或 D6 失效）", n)
	}

	c := dialWS(t, wsURL, token)
	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"ch":"`+metrics.ChannelMetrics+`","t":"sub"}`)); err != nil {
		t.Fatal(err)
	}

	// 只测"开始采样"不够，必须确认帧真到了前端能收到的地方。
	gotFrame := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !gotFrame {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		var frame struct {
			T  string `json:"t"`
			Ch string `json:"ch"`
		}
		if err := c.ReadJSON(&frame); err != nil {
			break
		}
		if frame.T == "data" && frame.Ch == metrics.ChannelMetrics {
			gotFrame = true
		}
	}
	if !gotFrame {
		t.Fatal("订阅后 3 秒内没收到 metrics 帧")
	}
	if src.calls.Load() == 0 {
		t.Fatal("收到帧却没采样，探针没接对")
	}

	// 断开后必须停表（D6 的另一半）。用"异常断开"（直接 Close，不发 unsub），
	// 这是最容易泄漏计数的一条路径。
	c.Close()
	time.Sleep(100 * time.Millisecond)
	settled := src.calls.Load()
	time.Sleep(150 * time.Millisecond)
	if n := src.calls.Load(); n != settled {
		t.Errorf("断开 WS 后仍在采样: %d → %d", settled, n)
	}
}

// HTTP 首屏与 WS 必须共用同一个采集器实例。
// 各建一个的话，"首屏数据"与 WS 推送会不一致；而且 HTTP 那个还在偷偷采样，
// D6 就形同虚设。接口上没有任何东西能阻止这种装配，所以显式测它。
func TestSingleCollectorSharedByHTTPAndWS(t *testing.T) {
	db := openTestDB(t)
	const pwd = "shared-test-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	hub := ws.NewHub()
	src := &probeSource{}
	col := wireMetrics(hub, src, 10*time.Second)
	defer col.Stop()

	deps := buildDeps(db, config.Config{}, hub, false, nil)
	deps.Metrics = col
	srv := newTestServer(t, deps)

	// HTTP 路径走一次按需采样。
	token := loginForToken(t, srv, pwd)
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/snapshot", nil)
	req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d %s", rec.Code, rec.Body.String())
	}
	httpCalls := src.calls.Load()
	if httpCalls != 1 {
		t.Errorf("HTTP 应触发 1 次采样, got %d", httpCalls)
	}

	// WS 路径用的必须是同一个实例：订阅后计数从同一处增长。
	c := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", token)
	defer c.Close()
	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"ch":"`+metrics.ChannelMetrics+`","t":"sub"}`)); err != nil {
		t.Fatal(err)
	}
	// interval=10s，所以订阅本身几乎不会额外触发采样；
	// 这里断言的是"没有第二个采集器在跑另一份采样"这个上界。
	time.Sleep(80 * time.Millisecond)
	if n := src.calls.Load(); n > httpCalls+1 {
		t.Errorf("疑似存在第二个采集器: 采样从 %d 涨到 %d", httpCalls, n)
	}
}

// 未注入采集器时接口必须 501，而不是空 200。
// 空 200 会让前端画出一片 0% 的仪表，看起来像"机器很闲"。
func TestSnapshotIs501WithoutCollector(t *testing.T) {
	db := openTestDB(t)
	const pwd = "no501-test-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, nil)
	srv := newTestServer(t, deps)

	token := loginForToken(t, srv, pwd)
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/snapshot", nil)
	req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("应 501, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// ---------- 本文件专用脚手架 ----------

type probeSource struct{ calls atomic.Int64 }

func (p *probeSource) Sample() (metrics.Snapshot, bool, error) {
	p.calls.Add(1)
	return metrics.Snapshot{Mem: &metrics.MemStat{Total: 1}}, false, nil
}

// probeSource 不实现 Reset：Collector 用可选接口，缺它也要能工作。

// loginForToken 走真实登录拿会话 cookie 值。
// 用真实登录而不是直接 SessionStore.Issue：登录会写 cookie，
// 那条链路（密码 → 会话 → cookie 名）一旦变化，这里立刻红。
func loginForToken(t *testing.T, srv *httptest.Server, pwd string) string {
	t.Helper()
	body := `{"password":"` + pwd + `"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/login", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Requested-With", "litepanel")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		t.Fatalf("登录失败: %d %+v", resp.StatusCode, m)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == api.SessionCookieName && ck.Value != "" {
			return ck.Value
		}
	}
	t.Fatal("登录响应里没有会话 cookie")
	return ""
}

func dialWS(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	c, resp, err := websocket.DefaultDialer.Dial(url, http.Header{
		"Cookie": {api.SessionCookieName + "=" + token},
	})
	if err != nil {
		t.Fatalf("WS 握手失败: %v (%v)", err, resp)
	}
	return c
}

// 这一条才是在验 main 的接线。
//
// 上面几个测试自己调了 hub.OnCount(...)，所以哪怕 main 里完全忘了接，
// 它们也照样全绿 —— 那种"测试替生产补了缺失的一行"是假验收。
// 这里改成只调用生产侧的装配函数，测试里绝不出 OnCount。
func TestWireMetricsConnectsHubToCollector(t *testing.T) {
	hub := ws.NewHub()
	src := &probeSource{}
	// interval 设长：让"是否采样"只由订阅数决定，而不是被定时器推着走。
	col := wireMetrics(hub, src, time.Hour)
	if col == nil {
		t.Fatal("必须返回采集器以便优雅退出时 Stop")
	}
	defer col.Stop()

	time.Sleep(40 * time.Millisecond)
	if n := src.calls.Load(); n != 0 {
		t.Fatalf("未订阅就采了 %d 次", n)
	}

	// 只通过 hub 的订阅数变化来驱动，不直接碰采集器。
	if got := hub.SubscriberCount(metrics.ChannelMetrics); got != 0 {
		t.Fatalf("前置：应有 0 个订阅者, got %d", got)
	}
	// 用真实 WS 连接产生订阅计数，而不是手写回调。
	db := openTestDB(t)
	const pwd = "wire-test-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	deps := buildDeps(db, config.Config{}, hub, false, nil)
	deps.Metrics = col
	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	c := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", token)
	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"ch":"`+metrics.ChannelMetrics+`","t":"sub"}`)); err != nil {
		t.Fatal(err)
	}
	// 订阅计数跨越 0 时，采集器必须立刻采一轮（首帧不等一个 interval）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && src.calls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if src.calls.Load() == 0 {
		t.Error("订阅后采集器没启动：hub 与采集器之间的接线缺失或写错频道名")
	}

	// 反向也必须成立，且要能认出"频道名写错"这类错法。
	c.Close()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.SubscriberCount(metrics.ChannelMetrics) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	settled := src.calls.Load()
	time.Sleep(120 * time.Millisecond)
	if n := src.calls.Load(); n != settled {
		t.Errorf("取消订阅后仍在采样: %d → %d", settled, n)
	}
}

// -debug 打开时 WebSocket 必须仍然能升级。
//
// M1 遗留缺陷：访问日志中间件用 statusWriter 包住 ResponseWriter，
// 而它内嵌的是 http.ResponseWriter 接口，方法集里没有 Hijack。
// gorilla 靠 w.(http.Hijacker) 抢裸 TCP，断言失败就回
// 500 "websocket: response does not implement http.Hijacker"。
// 后果是：一旦开 -debug 排查问题，WS 就全挂 —— 而需要 -debug 的
// 时候恰恰都是问题最刁钻的时候。
func TestWebSocketWorksWithDebugAccessLog(t *testing.T) {
	db := openTestDB(t)
	const pwd = "debug-ws-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	hub := ws.NewHub()
	var log bytes.Buffer
	// 接上采集器：没有订阅者驱动的广播时，“读不到帧”无法区分
	// “中间件拆断了连接”与“本来就没东西可推”，测不出东西。
	col := wireMetrics(hub, &probeSource{}, 10*time.Millisecond)
	defer col.Stop()
	// debug=true 是本用例的全部重点。
	deps := buildDeps(db, config.Config{}, hub, true, &log)
	deps.Metrics = col
	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	// 握手必须成功（dialWS 内部会 fatal）。
	c := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", token)
	defer c.Close()

	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"ch":"`+metrics.ChannelMetrics+`","t":"sub"}`)); err != nil {
		t.Fatal(err)
	}
	// 必须真的收到 metrics 数据帧：这才证明中间件没拆断双向流量。
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var frame struct {
			T  string `json:"t"`
			Ch string `json:"ch"`
		}
		if err := c.ReadJSON(&frame); err != nil {
			t.Fatalf("-debug 下读不到帧: %v", err)
		}
		if frame.T == "data" && frame.Ch == metrics.ChannelMetrics {
			break
		}
	}
	// -debug 下访问日志仍要记到东西（否则修 Hijack 时把日志顺带改没了也不会发现）。
	// 断言的是具体行为，不是"日志里有 /ws" —— 后者连"→"那半行都能满足，
	// 把 hijacked 分支整段废掉也照样绿，等于没测。
	out := log.String()
	if !strings.Contains(out, "↔ GET /ws") {
		t.Errorf("应记下接管行 ↔ /ws, got %q", out)
	}
	// 绝不能出现"← /ws | 200"这种完成行：它看着像请求已正常结束，
	// 而实际长连接还在跑 —— 这正是本 bug 最需要避免的误导。
	if strings.Contains(out, "← GET /ws") {
		t.Errorf("被接管的连接不该记完成行, got %q", out)
	}
}
