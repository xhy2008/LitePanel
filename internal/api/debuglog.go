package api

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// accessLog 把请求记到 deps.LogWriter。
// 关键设计：进入 handler 之前先打一行 "→"，返回后再打一行 "←"。
// 这样“请求进来但永不返回”（挂住、死锁、等外部进程）在日志里会留下
// 只有 → 没有 ← 的条目 —— 排查浏览器白屏时这是决定性的证据。
func accessLog(w io.Writer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ua := r.Header.Get("User-Agent")
		if len(ua) > 160 {
			ua = ua[:160]
		}
		fmt.Fprintf(w, "→ %s %s%s | %s | enc=%q\n",
			r.Method, r.Host, r.URL.Path, ua, r.Header.Get("Accept-Encoding"))

		rec := &statusWriter{ResponseWriter: rw, status: 0}
		next.ServeHTTP(rec, r)

		cost := time.Since(start)
		tag := ""
		if cost > 2*time.Second {
			tag = " SLOW"
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK // 从未 WriteHeader 过（例如静态文件走了 200）
		}
		fmt.Fprintf(w, "← %s %s | %d | %s%s\n", r.Method, r.URL.Path, status, cost.Round(time.Microsecond), tag)
	})
}

// statusWriter 记录是否写过状态码，用于区分“回了 200”和“一个字都没回”。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Flush 透传给底层，保证 WS 升级与流式响应不被打断。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能拿到底层（TLS/超时等需要）。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
