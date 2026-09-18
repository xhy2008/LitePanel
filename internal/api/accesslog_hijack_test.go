package api

// statusWriter 的 Hijack 透传测试 —— 两种构建标签下都必须通过。
//
// 与 cmd/litepanel 里那个依赖 -debug 装配的 WS 端到端测试不同，这里
// 直接测 accessLog 单元：release 构建虽然不挂载中间件，但类型还在，
// 谁将来把它接到别的挂载点上，Hijack 断链会立刻咬人
// （M1 的 WS 500 事故正是这个形状：编译过、测试绿、一开日志全挂）。

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccessLogPassesHijackThrough(t *testing.T) {
	var log bytes.Buffer
	h := accessLog(&log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			// 这就是 M1 事故现场：gorilla 在这里断言失败后回了 500。
			http.Error(w, "response does not implement http.Hijacker",
				http.StatusInternalServerError)
			return
		}
		conn, bw, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(bw, "HTTP/1.1 418 I'm a teapot\r\n\r\n")
		_ = bw.Flush()
	}))
	srv := httptest.NewServer(h)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("Hijack 后写的响应应原样到达客户端, got %d", resp.StatusCode)
	}
	// 必须先关掉服务再读日志：↔ 行是中间件在 handler 返回之后才写的，
	// 客户端拿到响应 ≠ 那一行已落笔。早先的写法在 release 下靠时序侥幸绿，
	// -tags debug 下就输了 —— 典型的假绿/假红对。srv.Close 会等整条
	// ServeHTTP 链（含中间件收尾）返回。
	srv.Close()

	out := log.String()
	// 被接管的连接不得记"← … 200"完成行（看着像正常结束，实际连接还活着）。
	if strings.Contains(out, "←") {
		t.Errorf("劫持后不该资源篮完成行, got %q", out)
	}
	if !strings.Contains(out, "↔") {
		t.Errorf("应记下接管行 ↔, got %q", out)
	}
}

// 普通（不劫持）响应必须照常记完成行，防止为了过上一个用例把日志整个砍掉。
func TestAccessLogStillLogsNormalResponse(t *testing.T) {
	var log bytes.Buffer
	h := accessLog(&log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !strings.Contains(log.String(), "←") || !strings.Contains(log.String(), "201") {
		t.Errorf("普通响应应记完成行含状态码, got %q", log.String())
	}
}

// 底层不支持 Hijack 时必须明确报错，而不是 panic 或假成功。
func TestHijackWithoutSupportErrors(t *testing.T) {
	rec := &statusWriter{ResponseWriter: plainResponseWriter{}}
	if _, _, err := rec.Hijack(); err == nil {
		t.Fatal("底层无 Hijack 时应返回错误")
	}
}

// plainResponseWriter 故意只实现 http.ResponseWriter 的三个方法。
type plainResponseWriter struct{ header http.Header }

func (p plainResponseWriter) Header() http.Header {
	if p.header == nil {
		p.header = http.Header{}
	}
	return p.header
}
func (p plainResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (p plainResponseWriter) WriteHeader(int)             {}

// 编译期钉死：statusWriter 必须始终满足 Hijacker / Flusher / 接口本身。
var (
	_ http.ResponseWriter = &statusWriter{}
	_ http.Hijacker       = &statusWriter{}
	_ http.Flusher        = &statusWriter{}
)

// 劫持成功后底层连接归调用方管；这里再确认一次我们没顺手把
// bufio 缓冲里残留的响应写成两份（ hijack 语义里 brw 是给调用方的）。
func TestHijackedConnUsableAfterRelease(t *testing.T) {
	done := make(chan error, 1)
	var log bytes.Buffer
	h := accessLog(&log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			done <- err
			return
		}
		rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		fmt.Fprint(rw, "HTTP/1.1 204 No Content\r\n\r\n")
		done <- rw.Flush()
	}))
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "204") {
		t.Errorf("劫持连接应可读, got %q", line)
	}
	if err := <-done; err != nil {
		t.Errorf("服务端写劫持连接失败: %v", err)
	}
}
