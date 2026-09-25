package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/terminal"
	"litepanel/internal/ws"
)

func TestTerminalSessionsAreWired(t *testing.T) {
	db := openTestDB(t)
	requireTermSessionsTable(t, db)
	const pwd = "sess-wiring-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	// 会话服务由生产装配函数给出；测试里绝不自己 new terminal.Service
	//（那等于替生产补了缺失的一行，main 漏接也照样绿 —— M1/M4 踩过）。
	tw := wireTerminal(db, ws.NewHub())
	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, nil)
	deps.TermSessions = tw.Sessions

	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)
	call := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Requested-With", "litepanel")
		req.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: token})
		rec := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// 建一个真会话（真 tmux），并断言它在 tmux 里真的存在
	code, body := call("POST", "/api/term/sessions", `{"title":"装配验收"}`)
	if code != http.StatusCreated {
		t.Fatalf("真装配下创建失败: %d %s", code, body)
	}
	var created struct {
		Session terminal.SessionMeta `json:"session"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Session.ID
	idPath := "/api/term/sessions/" + strconv.FormatInt(id, 10)
	t.Cleanup(func() { call("DELETE", idPath+"?confirm=1", "") })

	if created.Session.TmuxName != "lp-"+strconv.FormatInt(id, 10) {
		t.Fatalf("会话名应由 id 决定, got %s", created.Session.TmuxName)
	}
	if !tmuxHas(t, created.Session.TmuxName) {
		t.Fatalf("API 报成功但 tmux 里没有该会话")
	}

	if code, body = call("GET", "/api/term/sessions", ""); code != http.StatusOK ||
		!strings.Contains(body, `"装配验收"`) {
		t.Fatalf("列表应含刚建的会话: %d %s", code, body)
	}

	if code, body = call("PATCH", idPath, `{"title":"改过了"}`); code != http.StatusOK {
		t.Fatalf("改名应 200: %d %s", code, body)
	}
	if code, body = call("GET", "/api/term/sessions", ""); !strings.Contains(body, "改过了") {
		t.Fatalf("改名没落到列表: %d %s", code, body)
	}

	// 缺 confirm 不许删（服务端强制二次确认）
	if code, _ = call("DELETE", idPath, ""); code != http.StatusBadRequest {
		t.Fatalf("缺 confirm 应 400, got %d", code)
	}
	if !tmuxHas(t, created.Session.TmuxName) {
		t.Fatal("未确认的请求把会话删掉了")
	}

	if code, body = call("DELETE", idPath+"?confirm=1", ""); code != http.StatusOK {
		t.Fatalf("删除应 200: %d %s", code, body)
	}
	if tmuxHas(t, created.Session.TmuxName) {
		t.Fatal("删除成功但 tmux 会话还在")
	}
	if code, body = call("GET", "/api/term/sessions", ""); strings.Contains(body, "改过了") {
		t.Fatalf("删除后列表里还有它: %d %s", code, body)
	}
}

// tmuxHas 用 tmux 自己判断会话在不在，不信面板的返回值。
func tmuxHas(t *testing.T, name string) bool {
	t.Helper()
	return exec.Command(terminal.DefaultBin, "has-session", "-t", "="+name).Run() == nil
}
