package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/auth"
	"litepanel/internal/store"
)

// ---------- 脚手架（密码流程专用，cookie 由 jar 自动管理）----------

type pwEnv struct {
	t      *testing.T
	db     *store.DB
	srv    *httptest.Server
	client *http.Client
}

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newPWEnv(t *testing.T, initial string) *pwEnv {
	t.Helper()
	return newPWEnvWith(t, initial, nil)
}

// newPWEnvWith 允许调用方往 AuthDeps 里追加依赖（如 Metrics）。
// 只开这一个口子是有意的：所有测试都走同一份真实 AuthDeps 装配，
// 免得测试桩与真实装配漂移（M1 的冻结 bug 就是这么来的）。
func newPWEnvWith(t *testing.T, initial string, mutate func(*api.AuthDeps)) *pwEnv {
	t.Helper()
	db := openDB(t)
	if err := auth.SetInitialPassword(db, initial); err != nil {
		t.Fatal(err)
	}
	clk := time.Now
	deps := api.AuthDeps{
		DB:       db,
		Sessions: auth.NewSessionStore(db, clk, 24*time.Hour),
		Limiter:  auth.NewLoginLimiter(clk, 5, 10*time.Minute),
		Clock:    clk,
	}
	if mutate != nil {
		mutate(&deps)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewRouter(nil, deps))
	t.Cleanup(srv.Close)
	return &pwEnv{t: t, db: db, srv: srv, client: &http.Client{Jar: jar}}
}

func (e *pwEnv) do(method, path, body string) (int, map[string]any) {
	e.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	if method != "GET" {
		// 与后端 CSRF 约定一致：非 GET 必须带该头。
		req.Header.Set("X-Requested-With", "litepanel")
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *pwEnv) login(password string) {
	e.t.Helper()
	code, body := e.do("POST", "/api/login", `{"password":"`+password+`"}`)
	if code != http.StatusOK {
		e.t.Fatalf("登录失败: %d %+v", code, body)
	}
}

// ---------- GET /api/me ----------

// /api/me 是少数「未登录也 200」的接口：前端靠它决定是否跳登录，
// 若这里返回 401，http.ts 的 401 拦截会造成跳转风暴。
func TestMeUnauthenticated(t *testing.T) {
	e := newPWEnv(t, "initial-pass-123")
	code, body := e.do("GET", "/api/me", "")
	if code != http.StatusOK {
		t.Fatalf("未登录也应 200, got %d", code)
	}
	if body["authenticated"] != false {
		t.Fatalf("authenticated 应为 false, got %+v", body)
	}
}

func TestMeReportsMustChangePassword(t *testing.T) {
	e := newPWEnv(t, "initial-pass-123")
	e.login("initial-pass-123")

	code, body := e.do("GET", "/api/me", "")
	if code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	if body["authenticated"] != true {
		t.Fatalf("authenticated 应为 true, got %+v", body)
	}
	if body["must_change_password"] != true {
		t.Fatalf("首启后应要求改密, got %+v", body)
	}
}

// ---------- 强制改密闸门 ----------

// 未改密前，除 me/password/logout 外的 API 一律 403。
func TestMustChangePasswordBlocksOtherAPI(t *testing.T) {
	e := newPWEnv(t, "initial-pass-123")
	e.login("initial-pass-123")

	if code, body := e.do("GET", "/api/ping", ""); code != http.StatusForbidden {
		t.Fatalf("未改密应 403, got %d %+v", code, body)
	}
	// 改密相关接口必须可达，否则用户被永久锁死。
	if code, _ := e.do("GET", "/api/me", ""); code != http.StatusOK {
		t.Fatalf("/api/me 应可达, got %d", code)
	}
	if code, _ := e.do("POST", "/api/password", `{"old":"initial-pass-123","new":"brand-new-pass"}`); code != http.StatusOK {
		t.Fatalf("/api/password 应可达, got %d", code)
	}
}

// ---------- POST /api/password ----------

func TestChangePasswordFlow(t *testing.T) {
	const old = "initial-pass-123"
	e := newPWEnv(t, old)
	e.login(old)

	// 旧密码不对 → 拒绝。
	if code, body := e.do("POST", "/api/password", `{"old":"wrong-one","new":"brand-new-pass"}`); code == http.StatusOK {
		t.Fatalf("旧密码错误应被拒, got %d %+v", code, body)
	}
	// 过短的新密码也拒。
	if code, _ := e.do("POST", "/api/password", `{"old":"`+old+`","new":"short"}`); code != http.StatusBadRequest {
		t.Fatalf("过短新密码应 400, got %d", code)
	}

	code, body := e.do("POST", "/api/password", `{"old":"`+old+`","new":"brand-new-pass"}`)
	if code != http.StatusOK {
		t.Fatalf("改密应 200, got %d %+v", code, body)
	}

	// 改密后当前会话也全部失效（设计 M7-T5）：需重新登录。
	if code, _ := e.do("GET", "/api/ping", ""); code != http.StatusUnauthorized {
		t.Fatalf("改密后会话应失效, got %d", code)
	}

	// 用新密码重新登录后：闸门解除、标记清除。
	e.login("brand-new-pass")
	if code, _ := e.do("GET", "/api/ping", ""); code != http.StatusOK {
		t.Fatalf("改密后应放行, got %d", code)
	}
	if _, body := e.do("GET", "/api/me", ""); body["must_change_password"] != false {
		t.Fatalf("改密后标记应清除, got %+v", body)
	}
}

func TestOldPasswordNoLongerWorks(t *testing.T) {
	const old = "initial-pass-123"
	e := newPWEnv(t, old)
	e.login(old)
	if code, _ := e.do("POST", "/api/password", `{"old":"`+old+`","new":"brand-new-pass"}`); code != http.StatusOK {
		t.Fatalf("改密应 200, got %d", code)
	}

	// 换一个新客户端（无 cookie）验证旧密码不能再登录。
	fresh := &http.Client{}
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/login",
		bytes.NewBufferString(`{"password":"`+old+`"}`))
	req.Header.Set("X-Requested-With", "litepanel")
	req.Header.Set("Content-Type", "application/json")
	resp, err := fresh.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("旧密码不应能登录, got %d", resp.StatusCode)
	}
}

// 改密后其他会话必须全部失效（M7-T5 明确列出），
// 否则密码换了、别人的旧 token 照样能用。
func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	const pw = "initial-pass-123"
	db := openDB(t)
	if err := auth.SetInitialPassword(db, pw); err != nil {
		t.Fatal(err)
	}
	clk := time.Now
	srv := httptest.NewServer(api.NewRouter(nil, api.AuthDeps{
		DB:       db,
		Sessions: auth.NewSessionStore(db, clk, 24*time.Hour),
		Limiter:  auth.NewLoginLimiter(clk, 5, 10*time.Minute),
		Clock:    clk,
	}))
	t.Cleanup(srv.Close)

	newClient := func() *http.Client {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Client{Jar: jar}
	}
	doWith := func(c *http.Client, method, path, body string) (int, map[string]any) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rdr)
		if method != "GET" {
			req.Header.Set("X-Requested-With", "litepanel")
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	A, B := newClient(), newClient()
	for _, c := range []*http.Client{A, B} {
		if code, _ := doWith(c, "POST", "/api/login", `{"password":"`+pw+`"}`); code != http.StatusOK {
			t.Fatalf("登录应成功, got %d", code)
		}
	}
	// 两个会话都能过闸门检查（/api/me  authenticated=true）。
	if _, body := doWith(B, "GET", "/api/me", ""); body["authenticated"] != true {
		t.Fatalf("B 会话应有效, got %+v", body)
	}

	if code, _ := doWith(A, "POST", "/api/password", `{"old":"`+pw+`","new":"brand-new-pass"}`); code != http.StatusOK {
		t.Fatalf("改密应 200, got %d", code)
	}

	// B 的旧会话立刻失效。
	if _, body := doWith(B, "GET", "/api/me", ""); body["authenticated"] != false {
		t.Fatalf("改密后旧会话应失效, got %+v", body)
	}
}
