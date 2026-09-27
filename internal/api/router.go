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
	"litepanel/internal/logx"
	"litepanel/internal/store"
	"litepanel/internal/ws"
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
	Hub          *ws.Hub

	// Debug 打开逐请求访问日志（仅 dev 构建默认开启，见 cmd/litepanel）。
	Debug bool
	// LogWriter 是访问日志的去向；nil 则不记。测试注入 bytes.Buffer。
	LogWriter io.Writer
	// Services 为 nil 时服务相关接口返回 501（与 Metrics 同样的取舍：
	// 空实现会让前端以为"一个服务都没有"）。
	Services ServiceSupervisor

	// Metrics 为 nil 时指标接口返回 501（不返回空 200 ————
	// 空壳会被前端渲染成"各项 0%"，看起来像机器空闲）。
	Metrics MetricsSource

	// Term 为 nil 时终端相关接口返回 501。同上一条的理由：返回
	// available:false 会把装配层"没接终端模块"渲染成"你的服务器没装
	// tmux"，把用户支到完全错误的方向去。
	Term TermProber

	// TermSessions 为 nil 时终端会话接口返回 501（同上一条的理由：
	// 200 + 空列表会被渲染成"服务器上没有任何终端会话"）。
	TermSessions TermSessions

	// Commands 为 nil 时快捷命令接口返回 501（同上：200 + 空列表会被渲染
	// 成"没有任何快捷命令"，把"面板没接这个模块"说成"你还没添加过"）。
	Commands Commands
}

// NewRouter 返回面板根路由。static 为 nil 时不挂载前端（便于 API 测试）；
// deps.Sessions 为 nil 时 API 一律返回 501（仅用于早期骨架测试）。
func NewRouter(static fs.FS, deps AuthDeps) chi.Router {
	r := chi.NewRouter()

	// logx.Enabled 是构建期常量：发布构建里这个分支连同 accessLog、
	// statusWriter 一起被编译器消除（D9/§12.1：零开销，不是运行时判断）。
	// deps.Debug 保留作运行时段位：调试构建里不传 -debug 仍静默。
	if logx.Enabled && deps.Debug && deps.LogWriter != nil {
		r.Use(func(next http.Handler) http.Handler {
			return accessLog(deps.LogWriter, next)
		})
	}

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
		// /api/me 不套鉴权：未登录也要返 200 + {authenticated:false}。
		a.Get("/me", handleMe(deps))
		a.Post("/logout", authed(handleLogout(deps)))
		a.Post("/password", authed(handleChangePassword(deps)))
		ping := func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		}
		a.Get("/ping", authed(ping))
		a.Post("/ping", authed(ping))
		if deps.Services != nil {
			a.Get("/services", authed(handleServicesList(deps)))
			a.Post("/services", authed(handleServicesCreate(deps)))
			a.Patch("/services/{id}", authed(handleServicesUpdate(deps)))
			a.Delete("/services/{id}", authed(handleServicesDelete(deps)))
			a.Post("/services/{id}/toggle", authed(handleServiceToggle(deps)))
			a.Get("/services/{id}/log", authed(handleServiceLog(deps)))
			a.Delete("/services/{id}/log", authed(handleServiceLogClear(deps)))
		}
		if deps.Term != nil {
			a.Get("/term/health", authed(handleTermHealth(deps.Term)))
		}
		if deps.TermSessions != nil {
			a.Get("/term/sessions", authed(handleTermSessionsList(deps.TermSessions)))
			a.Post("/term/sessions", authed(handleTermSessionsCreate(deps.TermSessions)))
			a.Patch("/term/sessions/{id}", authed(handleTermSessionsRename(deps.TermSessions)))
			a.Delete("/term/sessions/{id}", authed(handleTermSessionsDelete(deps.TermSessions)))
			a.Get("/term/sessions/{id}/output", authed(handleTermSessionOutput(deps.TermSessions)))
		}
		if deps.Commands != nil {
			a.Get("/commands", authed(handleCommandsList(deps.Commands)))
			a.Post("/commands", authed(handleCommandsCreate(deps.Commands)))
			// /commands/busy 必须挂在 /commands/{id} 之前抢不到位置？chi 按
			// 静态段优先匹配，这里显式分开写是为了让"路由被 /{id} 吃掉"
			// 这种错在测试里立刻可见（busy 会被解成 id 非数字 → 400）。
			a.Get("/commands/busy", authed(handleCommandsBusy(deps.Commands)))
			a.Patch("/commands/{id}", authed(handleCommandsUpdate(deps.Commands)))
			a.Delete("/commands/{id}", authed(handleCommandsDelete(deps.Commands)))
			a.Post("/commands/{id}/move", authed(handleCommandsMove(deps.Commands)))
			a.Post("/commands/{id}/run", authed(handleCommandsRun(deps.Commands)))
		}
		if deps.Metrics != nil {
			// 性能监控快照公开（设计偏离，用户明确要求）：登录页也要画仪表。
			// 未登录时前端 attachMetrics 拉 snapshot 不再 401 → 不再刷屏。
			a.Get("/metrics/snapshot", handleMetricsSnapshot(deps.Metrics))
		}
		// 兜底：/api 下的其他路径先过鉴权，再回 501，
		// 避免未登录访问未实现接口被误判为 404/200。
		a.Handle("/*", authed(notImplemented))
	})

	// 事件接线（supervisor -> hub）只在 cmd/litepanel 的 wireServices 里做一次。
	// 这里再写一遍会让"接没接线"有两个主人：实测删掉 wireServices 里的
	// OnEvent，接线测试照过 —— 两个主人互相掩盖了漏接。与 wireMetrics 同规。
	if deps.Hub != nil {
		r.Handle("/ws", deps.Hub.Handler(wsAuth(deps)))
	}

	if static != nil {
		fileServer := http.FileServerFS(static)
		r.Handle("/*", spaHandler(static, fileServer))
	}
	return r
}

// wsAuth 把 WS 握手鉴权委托给会话存储（cookie 与 HTTP 一致）。
func wsAuth(deps AuthDeps) func(*http.Request) bool {
	return func(r *http.Request) bool {
		c, err := r.Cookie(SessionCookieName)
		if err != nil || c.Value == "" {
			return false
		}
		_, ok, err := deps.Sessions.Validate(c.Value)
		return err == nil && ok
	}
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "该接口尚未实现："+r.URL.Path)
}

// looksLikeAsset 以「最后一段是否含扩展名」判断。用启发式而非白名单目录，
// 是为了让同类问题不会换个目录名就复现。
func looksLikeAsset(p string) bool {
	i := strings.LastIndexByte(p, '/')
	return strings.IndexByte(p[i+1:], '.') > 0
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
			// 命中不到、且看起来是静态资源（带扩展名）→ 必须 404。
			// 绝不能回 index.html：那等于把资源缺失伪装成 200 成功，
			// 浏览器会因 MIME 不符拒绝把 HTML 当 ES module 执行，
			// 表现为白屏且不抛任何异常，前端错误捕获完全看不到。
			// 典型触发：重新构建后 hash 变化，而浏览器还在用缓存的旧 index.html。
			if looksLikeAsset(p) {
				http.NotFound(w, req)
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
