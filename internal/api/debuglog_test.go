//go:build debug

// 本文件跑的是"经 NewRouter 接线"的全栈链路：release 构建下
// accessLog 被 logx.Enabled 编译期剥离，这些断言就没有对象了。
// statusWriter 本身的行为（含 Hijack 透传）由 accesslog_hijack_test.go
// 在两种构建下都把关；release 下"整个二进制确实无日志"由
// cmd/litepanel 的零输出测试把关。

package api_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
