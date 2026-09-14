package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"litepanel/internal/auth"
)

// defaultSessionTTL 是会话默认有效期，M7 接入设置页后可覆盖。
const defaultSessionTTL = 7 * 24 * time.Hour

type loginRequest struct {
	Password string `json:"password"`
}

// handleLogin 校验密码并下发会话 cookie；失败按 IP 计数锁定。
func handleLogin(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if left, locked := deps.Limiter.RetryAfter(ip); locked {
			minutes := int(left.Minutes())
			if left.Minutes()-float64(minutes) > 0 {
				minutes++
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(left.Seconds())))
			writeError(w, http.StatusTooManyRequests, "locked",
				"登录失败次数过多，请 "+strconv.Itoa(minutes)+" 分钟后再试")
			return
		}

		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
			return
		}
		hash, err := auth.CurrentPassword(deps.DB)
		if errors.Is(err, auth.ErrNoPassword) {
			writeError(w, http.StatusServiceUnavailable, "no_password", "面板尚未完成初始化（未设置密码）")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "读取密码失败")
			return
		}
		if err := auth.CheckPassword(hash, req.Password); err != nil {
			deps.Limiter.Fail(ip)
			writeError(w, http.StatusUnauthorized, "bad_credentials", "密码错误")
			return
		}
		deps.Limiter.Reset(ip)

		ua := r.UserAgent()
		if len(ua) > 256 {
			ua = ua[:256]
		}
		token, err := deps.Sessions.Issue(ua, ip)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "创建会话失败")
			return
		}
		http.SetCookie(w, sessionCookie(token, sessionTTLOf(deps), deps.SecureCookie))
		must, _ := auth.MustChangePassword(deps.DB)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "must_change_password": must})
	}
}

// handleLogout 吊销当前会话并清 cookie。
func handleLogout(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(SessionCookieName); err == nil {
			_ = deps.Sessions.Revoke(c.Value)
		}
		http.SetCookie(w, sessionCookie("", -1, deps.SecureCookie))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func sessionTTLOf(deps AuthDeps) time.Duration {
	if deps.SessionTTL > 0 {
		return deps.SessionTTL
	}
	return defaultSessionTTL
}

func sessionCookie(token string, ttl time.Duration, secure bool) *http.Cookie {
	c := &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
	if ttl < 0 {
		c.MaxAge = -1 // Go 会渲染成 Expires=epoch + Max-Age=0，即清除 cookie
	} else {
		c.MaxAge = int(ttl.Seconds())
	}
	return c
}
