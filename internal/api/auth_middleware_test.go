package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/auth"
	"litepanel/internal/store"
)

const cookieName = api.SessionCookieName

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type harness struct {
	t        *testing.T
	handler  http.Handler
	sessions *auth.SessionStore
	token    string
	clock    *clock
	// deps 留着：有的测试要在装配后改它再重建路由（少见），
	// 更重要的是让 newHarnessWith 的回调产物有处可查。
	deps *api.AuthDeps
}

// newHarness 装配一套共享的鉴权底座（db/sessions/limiter/clock）+ 路由。
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, nil)
}

// testPassword 是共享底座的口令。它必须是真值而不是占位串：
// TestLoginLockout 那几个要真的走一次 POST /api/login 并成功。
const testPassword = "test-password"

// testPasswordHash 只算一次。
//
// 这不是"为了快而把真实现换成假的"：cost=12 的 bcrypt 在这台 ARM 机器上
// 实测 266ms（见 dev/bench 的探针），而本包 193 个测试里有 190 个建底座 ——
// 光重复哈希**同一个**口令就占了整包 162s 里的 ~51s。
//
// 为什么可以共用一个 hash：鉴权路径上没有任何地方重新校验它。requireAuth 走
// Sessions.Validate（查 token），强制改密闸门（MustChangePassword）只看
// password_hash 非空与 must_change_password 标志；真正会校验明文的只有
// /api/login，而那些测试用的就是上面这个 test-password，所以"同一个口令的
// 同一个 hash"与"每次新算一个"对它们完全等价（bcrypt 的 salt 只影响摘要本身，
// CheckPassword 从摘要里取 salt）。
//
// 写成包级变量而不是 sync.Once：Go 的包初始化天然只跑一次，而 Once 会让
// "这里省了 266ms"这件事在代码里看起来像个优化技巧，实际它就是个常量。
var testPasswordHash = func() string {
	h, err := auth.HashPassword(testPassword)
	if err != nil {
		panic(err)
	}
	return h
}()

// newHarnessWith 是可参数化装配：模块 Deps（Services/Files/...）由各测试
// 自己塞进来。之所以是"改 deps 的回调"而不是一堆 newHarnessXxx 变体：
// 每加一个模块就多一个变体，而底座那 20 行装配（含密码 hash 入库）会被
// 复制粘贴 N 份 —— 改一处忘另一处的代价是"某个模块的测试在假底座上跑"，
// 那种测试照样绿。
func newHarnessWith(t *testing.T, mutate func(*api.AuthDeps)) *harness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	clk := &clock{now: time.Unix(1_800_000_000, 0)}
	if _, err := db.SqlDB().Exec(
		`INSERT INTO settings(key,value,updated_at) VALUES('password_hash',?,1)`, testPasswordHash,
	); err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessionStore(db, clk.Now, 7*24*time.Hour)

	deps := api.AuthDeps{
		DB:       db,
		Sessions: sessions,
		Limiter:  auth.NewLoginLimiter(clk.Now, 5, 10*time.Minute),
		Clock:    clk.Now,
	}
	if mutate != nil {
		mutate(&deps)
	}
	h := &harness{t: t, sessions: sessions, clock: clk, deps: &deps}
	h.handler = api.NewRouter(nil, deps)

	tok, err := sessions.Issue("ua", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	h.token = tok
	return h
}

func (h *harness) do(method, path string, withCookie bool, xrw, body, remoteIP string) *http.Response {
	h.t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if withCookie {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: h.token})
	}
	if xrw != "" {
		req.Header.Set("X-Requested-With", xrw)
	}
	if remoteIP != "" {
		req.RemoteAddr = remoteIP + ":1234"
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w.Result()
}

func (h *harness) get(path string, cookies ...*http.Cookie) *http.Response {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w.Result()
}

func (h *harness) login(password, remoteIP string) *http.Response {
	h.t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	return h.do(http.MethodPost, "/api/login", false, "litepanel", string(body), remoteIP)
}

func TestAPIRoutesRequireAuth(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/api/settings", "/api/services", "/api/commands", "/api/metrics/snapshot", "/api/ping"} {
		if resp := h.get(p); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("未登录访问 %s 应 401, got %d", p, resp.StatusCode)
		}
	}
}

func TestAuthenticatedRequestPasses(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/api/ping", &http.Cookie{Name: cookieName, Value: h.token})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("已登录应 200, got %d", resp.StatusCode)
	}
}

func TestGarbageCookieIsUnauthorized(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/api/ping", &http.Cookie{Name: cookieName, Value: strings.Repeat("f", 64)})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("伪造 cookie 应 401, got %d", resp.StatusCode)
	}
}

// CSRF：所有非 GET 请求必须带 X-Requested-With: litepanel。
func TestNonGETNeedsXHRHeader(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		xrw  string
		want int
	}{
		{"缺失该头", "", http.StatusForbidden},
		{"值是别的框架惯例", "XMLHttpRequest", http.StatusForbidden},
		{"值正确", "litepanel", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, "/api/ping", true, tc.xrw, "{}", "127.0.0.1")
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// 登录接口自身不需要 cookie，但失败 5 次后按 IP 锁定，
// 即使密码正确也返回 429；锁定窗口过后恢复。
func TestLoginLockout(t *testing.T) {
	h := newHarness(t)
	const ip = "100.64.0.9"
	for i := 1; i <= 5; i++ {
		if resp := h.login("wrong-password", ip); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误密码应 401, got %d", i, resp.StatusCode)
		}
	}
	if resp := h.login("test-password", ip); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("锁定后即使密码正确也应 429, got %d", resp.StatusCode)
	}
	// 别的 IP 不受影响。
	if resp := h.login("test-password", "100.64.0.10"); resp.StatusCode != http.StatusOK {
		t.Fatalf("其他 IP 不应被锁定, got %d", resp.StatusCode)
	}
	h.clock.Advance(10*time.Minute + time.Second)
	if resp := h.login("test-password", ip); resp.StatusCode != http.StatusOK {
		t.Fatalf("锁定窗口过后应可登录, got %d", resp.StatusCode)
	}
}

// Cookie 必须 HttpOnly + SameSite=Lax + Path=/。
func TestLoginSetsHardenedCookie(t *testing.T) {
	h := newHarness(t)
	resp := h.login("test-password", "100.64.0.20")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录应成功, got %d", resp.StatusCode)
	}
	sc := resp.Header.Get("Set-Cookie")
	for _, want := range []string{cookieName + "=", "HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie 应含 %q, got %q", want, sc)
		}
	}
}

func TestLoginFailureBodyIsUniform(t *testing.T) {
	h := newHarness(t)
	resp := h.login("wrong-password", "100.64.0.30")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应 401, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("错误响应应为 JSON: %v", err)
	}
	if body["code"] == nil || body["message"] == nil {
		t.Fatalf("错误体应含 code 与 message, got %v", body)
	}
}

// 改密后旧会话必须全部失效（RevokeAll 的对外承诺）。
func TestRevokeAllInvalidatesCookie(t *testing.T) {
	h := newHarness(t)
	if resp := h.get("/api/ping", &http.Cookie{Name: cookieName, Value: h.token}); resp.StatusCode != 200 {
		t.Fatal("前置条件：token 应有效")
	}
	if err := h.sessions.RevokeAll(); err != nil {
		t.Fatal(err)
	}
	if resp := h.get("/api/ping", &http.Cookie{Name: cookieName, Value: h.token}); resp.StatusCode != 401 {
		t.Fatalf("RevokeAll 后应 401, got %d", resp.StatusCode)
	}
}

// POST /api/logout 清 cookie 并使会话失效。
func TestLogoutClearsSession(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodPost, "/api/logout", true, "litepanel", "", "100.64.0.40")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登出应 200, got %d", resp.StatusCode)
	}
	if sc := resp.Header.Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") {
		t.Errorf("登出应下发过期 cookie, got %q", sc)
	}
	if resp := h.get("/api/ping", &http.Cookie{Name: cookieName, Value: h.token}); resp.StatusCode != 401 {
		t.Fatalf("登出后旧 token 应失效, got %d", resp.StatusCode)
	}
}
