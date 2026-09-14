package api

import (
	"context"
	"net"
	"net/http"
	"strings"

	"litepanel/internal/auth"
)

// csrfHeader 是 CSRF 防护要求的 X-Requested-With 取值（设计 5.7）。
const csrfHeader = "litepanel"

// csrfGuard 要求所有改状态请求带上自定义头，浏览器跨站表单无法伪造。
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Header.Get("X-Requested-With") != csrfHeader {
			writeError(w, http.StatusForbidden, "csrf", "缺少或错误的 X-Requested-With 头")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const sessionKey ctxKey = 1

// requireAuth 保护 API：校验会话 cookie，并对非 GET 请求先校验 CSRF 头。
// 401=未认证，403=CSRF，两者不混用（设计 14 节的错误语义）。
func requireAuth(deps AuthDeps) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead &&
				r.Header.Get("X-Requested-With") != csrfHeader {
				writeError(w, http.StatusForbidden, "csrf", "缺少或错误的 X-Requested-With 头")
				return
			}
			c, err := r.Cookie(SessionCookieName)
			if err != nil || c.Value == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized", "未登录")
				return
			}
			sess, ok, err := deps.Sessions.Validate(c.Value)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "会话校验失败")
				return
			}
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized", "会话无效或已过期")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
		}
	}
}

// SessionFrom 取出中间件注入的会话，不存在返回 nil。
func SessionFrom(ctx context.Context) *auth.Session {
	s, _ := ctx.Value(sessionKey).(*auth.Session)
	return s
}

// clientIP 从 RemoteAddr 提取纯 IP（认证与限流按 IP 计数）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
