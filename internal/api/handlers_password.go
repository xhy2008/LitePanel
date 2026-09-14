package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"litepanel/internal/auth"
)

// handleMe 报告当前认证状态。它刻意不套 requireAuth：
// 前端靠它判断要不要跳登录，返回 401 会和 http.ts 的 401 拦截打转。
func handleMe(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authenticated := false
		if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
			if _, ok, err := deps.Sessions.Validate(c.Value); err == nil {
				authenticated = ok
			}
		}
		must, err := auth.MustChangePassword(deps.DB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "读取密码状态失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated":        authenticated,
			"must_change_password": must,
		})
	}
}

type passwordRequest struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// handleChangePassword 校验旧密码后更换，并吊销全部会话（设计 M7-T5：
// 改密后旧会话全部失效——否则密码换了、别人的旧 token 照样能用）。
func handleChangePassword(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req passwordRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
			return
		}
		hash, err := auth.CurrentPassword(deps.DB)
		if errors.Is(err, auth.ErrNoPassword) {
			writeError(w, http.StatusServiceUnavailable, "no_password", "面板尚未完成初始化")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "读取密码失败")
			return
		}
		if err := auth.CheckPassword(hash, req.Old); err != nil {
			writeError(w, http.StatusUnauthorized, "bad_credentials", "当前密码错误")
			return
		}
		if len(req.New) < 8 {
			writeError(w, http.StatusBadRequest, "weak_password", "新密码至少 8 位")
			return
		}
		if err := auth.ChangePassword(deps.DB, req.New); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "写入新密码失败")
			return
		}
		if err := deps.Sessions.RevokeAll(); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "吊销旧会话失败")
			return
		}
		// 当前会话也在 RevokeAll 里，顺手清 cookie，前端据此跳回登录页。
		http.SetCookie(w, sessionCookie("", -1, deps.SecureCookie))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reauth_required": true})
	}
}
