package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/terminal"
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
	p := wireTerminal()
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
