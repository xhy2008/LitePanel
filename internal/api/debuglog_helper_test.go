package api_test

import (
	"bytes"
	"net/http"
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
