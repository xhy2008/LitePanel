package api_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/auth"
	"litepanel/internal/ws"
)

// 排查“某个浏览器白屏、另一个正常”这类问题只有服务端日志能定性：
// 是请求没到、到了没回、还是回了但浏览器不认。
// 因此 Debug 模式必须留下 方法/路径/状态/耗时/UA 的访问日志。
func newDebugRouter(t *testing.T, w *bytes.Buffer, debug bool) http.Handler {
	t.Helper()
	db := openDB(t)
	clk := time.Now
	return api.NewRouter(nil, api.AuthDeps{
		DB:        db,
		Sessions:  auth.NewSessionStore(db, clk, 24*time.Hour),
		Limiter:   auth.NewLoginLimiter(clk, 5, 10*time.Minute),
		Clock:     clk,
		Hub:       ws.NewHub(),
		Debug:     debug,
		LogWriter: w,
	})
}

func TestDebugRequestLog(t *testing.T) {
	var log bytes.Buffer
	srv := httptest.NewServer(newDebugRouter(t, &log, true))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux) Chrome/140.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	out := log.String()
	for _, want := range []string{"GET", "/api/me", "200", "Chrome/140.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("日志应含 %q, 实际:\n%s", want, out)
		}
	}
}

// 默认（生产）不得留任何请求日志（设计 R4/D9：发布构建不写日志）。
func TestNoRequestLogByDefault(t *testing.T) {
	var log bytes.Buffer
	srv := httptest.NewServer(newDebugRouter(t, &log, false))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if log.Len() != 0 {
		t.Fatalf("默认不该有请求日志, got %q", log.String())
	}
}

// 慢请求必须显式标出来：白屏通常是某个请求挂住，而不是代码报错。
func TestFastRequestNotFlaggedSlow(t *testing.T) {
	var log bytes.Buffer
	srv := httptest.NewServer(newDebugRouter(t, &log, true))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if strings.Contains(log.String(), "SLOW") {
		t.Fatalf("快请求不该被标 SLOW, got %q", log.String())
	}
}

// 白屏场景里最要命的是“请求进来但永不返回”。因此必须在进 handler 之前
// 先落一行开始日志：handler 挂住时日志里会留下“只开始没结束”的条目。
func TestRequestStartIsLoggedBeforeHandlerRuns(t *testing.T) {
	var log bytes.Buffer
	srv := httptest.NewServer(newDebugRouter(t, &log, true))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("开始与结束应各一行, got %d 行:\n%s", len(lines), log.String())
	}
	if !strings.HasPrefix(lines[0], "→") {
		t.Fatalf("首行应是请求开始标记, got %q", lines[0])
	}
	if !strings.Contains(lines[len(lines)-1], "200") {
		t.Fatalf("末行应含状态码, got %q", lines[len(lines)-1])
	}
}
