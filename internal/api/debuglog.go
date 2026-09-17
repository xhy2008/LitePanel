package api

import (
	"bufio"
	"fmt"
	"io"
	"net"
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

		// 连接被 Hijack（WebSocket）之后，底层 TCP 已归协议自己管：
		// 此处拿到的耗时与状态码都没意义，留一行“← /ws | 200”反而把人
		// 往错方向引（看着像请求正常返回，实际长连接还在跑）。
		// 注：这行在 ServeHTTP 返回时就打，而 WS handler 只是起了两个 goroutine
		// 就返回 —— 它代指“握手完成”，不是“连接结束”。
		if rec.hijacked {
			fmt.Fprintf(w, "↔ %s %s | 连接已被接管（WebSocket 等），不记完成行\n",
				r.Method, r.URL.Path)
			return
		}

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
	status   int
	hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Hijack 透传给底层。必须显式写：statusWriter 内嵌的是 http.ResponseWriter
// 接口，方法集里没有 Hijack，gorilla/websocket 的 w.(http.Hijacker) 断言
// 会直接失败并回 500 —— 于是“一开 -debug 所有 WebSocket 就挂”，
// 而需要 -debug 的时候恰恰是问题最刁钻的时候。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("api: 底层 ResponseWriter 不支持 Hijack: %T", w.ResponseWriter)
	}
	conn, brw, err := h.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, brw, err
}

// Flush / Unwrap 属于防御性透传，目前没有调用方：WS 升级后 gorilla 直接持有
// 裸 net.Conn，不再碰 ResponseWriter；项目里也没有 SSE，没人用 ResponseController。
// 留着是因为它们是 ResponseWriter 包装器的标配：将来一上流式接口或写超时，
// 缺了它们不会是报错，而是“缓冲住了看不见”这种难查的症状。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能拿到底层（TLS/超时等需要）。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
