// Package api 组装 HTTP 路由。
package api

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"litepanel/internal/auth"
	"litepanel/internal/store"
)

// SessionCookieName 是承载会话 token 的 cookie 名。
const SessionCookieName = "lp_session"

// AuthDeps 是认证相关依赖。
type AuthDeps struct {
	DB           *store.DB
	Sessions     *auth.SessionStore
	Limiter      *auth.LoginLimiter
	Clock        func() time.Time
	SessionTTL   time.Duration
	SecureCookie bool
}

// NewRouter 返回面板根路由。static 为 nil 时不挂载前端（便于 API 测试）；
// deps.Sessions 为 nil 时 API 一律返回 501（仅用于早期骨架测试）。
func NewRouter(static fs.FS, deps AuthDeps) chi.Router {
	r := chi.NewRouter()

	authed := func(h http.HandlerFunc) http.HandlerFunc { return requireAuth(deps)(h) }

	r.Route("/api", func(a chi.Router) {
		// 面板所有非 GET 请求都必须带 X-Requested-With（含登录本身）。
		a.Use(csrfGuard)
		if deps.Sessions == nil || deps.Limiter == nil {
			a.HandleFunc("/*", notImplemented)
			a.HandleFunc("/", notImplemented)
			return
		}
		a.Post("/login", handleLogin(deps))
		a.Post("/logout", handleLogout(deps))
		ping := func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		}
		a.Get("/ping", authed(ping))
		a.Post("/ping", authed(ping))
		// 兜底：/api 下的其他路径先过鉴权，再回 501，
		// 避免未登录访问未实现接口被误判为 404/200。
		a.Handle("/*", authed(notImplemented))
	})

	if static != nil {
		fileServer := http.FileServerFS(static)
		r.Handle("/*", spaHandler(static, fileServer))
	}
	return r
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "该接口尚未实现："+r.URL.Path)
}

// spaHandler 命中不到真实文件时返回 index.html，让前端路由接管。
func spaHandler(static fs.FS, fileServer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := strings.TrimPrefix(path.Clean("/"+req.URL.Path), "/")
		if p != "" && p != "index.html" {
			if f, err := static.Open(p); err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, req)
				return
			}
		}
		index, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, string(index))
	})
}
