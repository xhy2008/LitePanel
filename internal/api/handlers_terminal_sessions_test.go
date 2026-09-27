package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/auth"
	"litepanel/internal/terminal"
)

var errBoom = errors.New("存储层炸了")

// M5-T6：终端会话的 HTTP 接口。
//
// 与 M5-T1 同构：HTTP 语义（状态码、校验、二次确认、鉴权）用替身在本包测，
// "接的是不是真家伙"由 cmd/litepanel 的装配测试钉住。这里绝不用真 tmux 起
// 会话来测 HTTP —— 那会把 HTTP 层的失败与 tmux 的时序混在一起。

// ---------- 替身 ----------

type stubSess struct {
	calls []string

	list   []terminal.SessionMeta
	err    error
	output string // CorpseOutput 的返回值（id=3 有词，其余空）
}

func (s *stubSess) record(f string) { s.calls = append(s.calls, f) }

func (s *stubSess) List(ctx context.Context) ([]terminal.SessionMeta, error) {
	s.record("list")
	return s.list, s.err
}

func (s *stubSess) Create(ctx context.Context, in terminal.SessionInput) (terminal.SessionMeta, error) {
	s.record("create")
	if s.err != nil {
		return terminal.SessionMeta{}, s.err
	}
	return terminal.SessionMeta{
		ID: 7, TmuxName: "lp-7", Title: in.Title, HistoryLimit: in.HistoryLimit,
		CreatedAt: 111, Alive: true,
	}, nil
}

func (s *stubSess) Rename(ctx context.Context, id int64, title string) error {
	s.record("rename")
	if s.err != nil {
		return s.err
	}
	for i := range s.list {
		if s.list[i].ID == id {
			s.list[i].Title = title
		}
	}
	return nil
}

func (s *stubSess) CorpseOutput(ctx context.Context, id int64) (string, error) {
	s.record("output:" + strconv.FormatInt(id, 10))
	if id == 999 {
		return "", terminal.ErrSessionNotFound
	}
	if s.err != nil {
		return "", s.err
	}
	if id == 3 {
		return s.output, nil
	}
	return "", nil
}

func (s *stubSess) Delete(ctx context.Context, id int64) error {
	s.record("delete")
	if s.err != nil {
		return s.err
	}
	out := s.list[:0]
	for _, v := range s.list {
		if v.ID != id {
			out = append(out, v)
		}
	}
	s.list = out
	return nil
}

func (s *stubSess) Reconcile(ctx context.Context) error {
	s.record("reconcile")
	return s.err
}

// ---------- 环境 ----------

// newTermAPIEnv 复用改密链路的脚手架：新库的 must_change_password 会把一切
// 挡成 403，必须真的走一遍登录/改密/重登（测试桩与真实装配漂移是 M1 冻结
// 事故的成因）。
func newTermAPIEnv(t *testing.T, sm api.TermSessions) *pwEnv {
	t.Helper()
	const initial = "tsess-initial-123"
	const newer = "tsess-changed-456"
	e := newPWEnvWith(t, initial, func(d *api.AuthDeps) {
		d.TermSessions = sm
	})
	e.login(initial)
	if code, body := e.do("POST", "/api/password",
		`{"old":"`+initial+`","new":"`+newer+`"}`); code != http.StatusOK {
		t.Fatalf("改密失败: %d %+v", code, body)
	}
	e.login(newer)
	return e
}

func sessList(id int64, title string, alive bool) []terminal.SessionMeta {
	return []terminal.SessionMeta{{
		ID: id, TmuxName: "lp-" + strconv.FormatInt(id, 10), Title: title, HistoryLimit: 20000,
		CreatedAt: 100, Alive: alive,
	}}
}

func sessionsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(body["sessions"])
	if err != nil {
		t.Fatalf("响应里没有 sessions 字段: %+v", body)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("sessions 形状不对: %v", err)
	}
	return out
}

// ---------- 测试 ----------

// 未登录一律 401（终端页在登录后才被访问，没有公开理由）。
// 注意环境要"已接线"，否则 401 可能只是 501 兜底的结果。
func TestTermSessionRoutesRequireAuth(t *testing.T) {
	db := openDB(t)
	if err := auth.SetInitialPassword(db, "authz-initial-123"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewRouter(nil, api.AuthDeps{
		DB:           db,
		Sessions:     auth.NewSessionStore(db, time.Now, 24*time.Hour),
		Limiter:      auth.NewLoginLimiter(time.Now, 5, 10*time.Minute),
		Clock:        time.Now,
		TermSessions: &stubSess{},
	}))
	defer srv.Close()

	// 不带 cookie 的裸 client
	var bare http.Client
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/term/sessions"},
		{"POST", "/api/term/sessions"},
		{"PATCH", "/api/term/sessions/1"},
		{"DELETE", "/api/term/sessions/1?confirm=1"},
	} {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Requested-With", "litepanel")
		resp, err := bare.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s 应 401, got %d", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestTermSessionsList(t *testing.T) {
	sm := &stubSess{list: sessList(3, "跑备份", true)}
	e := newTermAPIEnv(t, sm)

	code, body := e.do("GET", "/api/term/sessions", "")
	if code != http.StatusOK {
		t.Fatalf("应 200, got %d %+v", code, body)
	}
	got := sessionsOf(t, body)
	if len(got) != 1 || got[0]["title"] != "跑备份" {
		t.Fatalf("列表不符: %+v", got)
	}
	// 前端坚持 snake_case（与 Go tag 一一对应，HTTP/WS 共用一套 key）
	if got[0]["tmux_name"] != "lp-3" || got[0]["history_limit"] != float64(20000) {
		t.Fatalf("字段名/值不符: %+v", got[0])
	}
	if got[0]["alive"] != true {
		t.Fatalf("alive 丢了: %+v", got[0])
	}
}

func TestTermSessionsCreate(t *testing.T) {
	sm := &stubSess{}
	e := newTermAPIEnv(t, sm)

	code, body := e.do("POST", "/api/term/sessions",
		`{"title":"编译","cwd":"/root","history_limit":5000}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应 201, got %d %+v", code, body)
	}
	if body["session"] == nil {
		t.Fatalf("响应应带 session: %+v", body)
	}
	s := body["session"].(map[string]any)
	if s["tmux_name"] != "lp-7" || s["title"] != "编译" {
		t.Fatalf("回显不对: %+v", s)
	}
}

// 校验的主人是谁，测试就测谁。标题必填 / history_limit 白名单由存储层的
// normalized() 独占（见 store_test.go 里那两条），HTTP 层的职责是**把领域
// 错误翻成正确的状态码** —— 所以这里让替身返回那个错误，断言 400 而不是 500。
//
// 写成"发个空 body 期望 400"会是假测试：替身不校验，它只会返回 201；
// 而真存储换掉校验规则时，那种测试还会绿着替错误分层背书。
func TestTermSessionsCreateMapsDomainErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"标题必填", terminal.ErrTitleRequired, http.StatusBadRequest},
		{"历史上限越界", terminal.ErrBadHistoryLimit, http.StatusBadRequest},
		{"其它错误不该被当成用户错", errBoom, http.StatusInternalServerError},
	} {
		sm := &stubSess{err: tc.err}
		e := newTermAPIEnv(t, sm)
		code, body := e.do("POST", "/api/term/sessions", `{"title":"x"}`)
		if code != tc.want {
			t.Fatalf("%s: 应 %d, got %d %+v", tc.name, tc.want, code, body)
		}
	}
}

// 前端打错字段名必须立刻 400（静默忽略会让人以为"改了没生效"）。
func TestTermSessionsCreateRejectsUnknownField(t *testing.T) {
	e := newTermAPIEnv(t, &stubSess{})
	code, body := e.do("POST", "/api/term/sessions", `{"title":"x","titel":"y"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未知字段应 400, got %d %+v", code, body)
	}
}

func TestTermSessionsRename(t *testing.T) {
	sm := &stubSess{list: sessList(3, "旧名", true)}
	e := newTermAPIEnv(t, sm)

	code, body := e.do("PATCH", "/api/term/sessions/3", `{"title":"新名"}`)
	if code != http.StatusOK {
		t.Fatalf("重命名应 200, got %d %+v", code, body)
	}
	if got := sessionsOf(t, mustJSONGet(t, e)); len(got) == 0 || got[0]["title"] != "新名" {
		t.Fatalf("重命名没落到存储: %+v", got)
	}

	// 空标题 → 存储层报错 → 400（不是 500）
	sm.err = terminal.ErrTitleRequired
	if code, body := e.do("PATCH", "/api/term/sessions/3", `{"title":"  "}`); code != http.StatusBadRequest {
		t.Fatalf("空标题应 400, got %d %+v", code, body)
	}
	// 不存在的 id 404（不是 500）
	sm.err = terminal.ErrSessionNotFound
	if code, body := e.do("PATCH", "/api/term/sessions/999", `{"title":"x"}`); code != http.StatusNotFound {
		t.Fatalf("不存在的会话应 404, got %d %+v", code, body)
	}
}

// PATCH 只接受 title：其余字段（cwd/shell/history_limit）建会话时就固定了，
// 改它们要么需要重建会话、要么需要重启 tmux 会话，都不是 PATCH 该偷偷做的事。
func TestTermSessionsRenameRejectsOtherFields(t *testing.T) {
	e := newTermAPIEnv(t, &stubSess{list: sessList(3, "旧名", true)})
	if code, body := e.do("PATCH", "/api/term/sessions/3", `{"cwd":"/tmp"}`); code != http.StatusBadRequest {
		t.Fatalf("PATCH cwd 应 400, got %d %+v", code, body)
	}
}

// 删除必须带二次确认标记。这是服务端强制：少一个 query 参数就 400，
// 这样任何"忘了带确认"的调用（包括手工 curl、脚本、前端漏改）都不会
// 把一个正在跑任务的会话连 shell 一起杀掉。
func TestTermSessionsDeleteRequiresConfirm(t *testing.T) {
	sm := &stubSess{list: sessList(3, "在跑备份", true)}
	e := newTermAPIEnv(t, sm)

	code, body := e.do("DELETE", "/api/term/sessions/3", "")
	if code != http.StatusBadRequest {
		t.Fatalf("缺 confirm 应 400, got %d %+v", code, body)
	}
	if code, _ := e.do("DELETE", "/api/term/sessions/3?confirm=false", ""); code != http.StatusBadRequest {
		t.Fatalf("confirm=false 也应 400, got %d", code)
	}
	if len(sm.calls) > 0 {
		for _, c := range sm.calls {
			if c == "delete" {
				t.Fatal("未确认却执行了删除")
			}
		}
	}

	if code, body := e.do("DELETE", "/api/term/sessions/3?confirm=1", ""); code != http.StatusOK {
		t.Fatalf("确认后应 200, got %d %+v", code, body)
	}
	if len(sm.list) != 0 {
		t.Fatalf("删除没生效: %+v", sm.list)
	}
}

func TestTermSessionsDeleteUnknownIs404(t *testing.T) {
	sm := &stubSess{}
	sm.err = terminal.ErrSessionNotFound
	e := newTermAPIEnv(t, sm)
	if code, body := e.do("DELETE", "/api/term/sessions/999?confirm=1", ""); code != http.StatusNotFound {
		t.Fatalf("应 404, got %d %+v", code, body)
	}
}

// 存储层报错必须变 500 + 错误体，不能是空 200（前端会把空列表渲染成
// "一个会话都没有"，与真实情况相反）。
func TestTermSessionsListErrorIs5xx(t *testing.T) {
	sm := &stubSess{err: errBoom}
	e := newTermAPIEnv(t, sm)
	code, body := e.do("GET", "/api/term/sessions", "")
	if code < 500 {
		t.Fatalf("存储报错应 5xx, got %d %+v", code, body)
	}
	if body["code"] == nil {
		t.Fatalf("应回统一错误体: %+v", body)
	}
}

// 装配层没接终端会话模块时必须 501，而不是 200 + 空列表 ——
// 后者会被前端渲染成"你的服务器上没有任何终端会话"。
//
// 必须登录后再取：未登录拿到的 401 来自鉴权中间件，与装没接线无关，
// 用它断言 501 等于什么都没断言。
func TestTermSessionsNotWiredIs501(t *testing.T) {
	const initial, newer = "nowire-initial-123", "nowire-changed-456"
	e := newPWEnvWith(t, initial, func(d *api.AuthDeps) { /* TermSessions 故意留空 */ })
	e.login(initial)
	if code, body := e.do("POST", "/api/password",
		`{"old":"`+initial+`","new":"`+newer+`"}`); code != http.StatusOK {
		t.Fatalf("改密失败: %d %+v", code, body)
	}
	e.login(newer)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/term/sessions"},
		{"POST", "/api/term/sessions"},
		{"DELETE", "/api/term/sessions/1?confirm=1"},
	} {
		code, body := e.do(tc.method, tc.path, "{}")
		if code != http.StatusNotImplemented {
			t.Fatalf("%s %s 应 501, got %d %+v", tc.method, tc.path, code, body)
		}
	}
}

func mustJSONGet(t *testing.T, e *pwEnv) map[string]any {
	t.Helper()
	code, body := e.do("GET", "/api/term/sessions", "")
	if code != http.StatusOK {
		t.Fatalf("列表应 200, got %d %+v", code, body)
	}
	return body
}

// exit_status 是死因的对外契约（nil/null=活着或没观测过，-1=凭空消失，
// 0=正常退出[会被后端自动删]，>0=异常退出码）。前端全靠它把异常退出的
// 标签标成「命令异常退出 N」——字段名或类型漂了，异常会话就会显示成
// 普通死会话，正是用户要求手动清理时需要知道的那个信息。
func TestTermSessionsListCarriesExitStatus(t *testing.T) {
	seven := 7
	sm := &stubSess{list: []terminal.SessionMeta{
		{ID: 1, TmuxName: "lp-1", Title: "异常", Alive: false, ExitStatus: &seven},
		{ID: 2, TmuxName: "lp-2", Title: "活着", Alive: true},
	}}
	e := newTermAPIEnv(t, sm)
	code, body := e.do("GET", "/api/term/sessions", "")
	if code != http.StatusOK {
		t.Fatalf("应 200, got %d %s", code, body)
	}
	got := sessionsOf(t, body)
	if len(got) != 2 {
		t.Fatalf("列表不符: %+v", got)
	}
	if got[0]["exit_status"] != float64(7) {
		t.Fatalf("exit_status 必须是数字 7（前端据此标'异常退出'）: %+v", got[0])
	}
	v, ok := got[1]["exit_status"]
	if !ok || v != nil {
		t.Fatalf("活会话的 exit_status 必须是显式 null（不是缺字段）: %+v", got[1])
	}
}

// 遗言端点：GET /api/term/sessions/{id}/output。
// 异常退出的 tab 点进去，前端靠它显示死前最后几屏。
// 三种返回各有语义：有历史→原文；会话消失→空串+200（前端显示
// "无输出记录"）；库里没这个 id→404。
func TestTermSessionOutput(t *testing.T) {
	sm := &stubSess{list: sessList(3, "跑备份", false)}
	sm.output = "遗言正文"
	e := newTermAPIEnv(t, sm)

	code, body := e.do("GET", "/api/term/sessions/3/output", "")
	if code != http.StatusOK {
		t.Fatalf("应 200, got %d %s", code, body)
	}
	if body["output"] != "遗言正文" {
		t.Fatalf("output 字段不符: %+v", body)
	}
	if got := sm.calls[len(sm.calls)-1]; got != "output:3" {
		t.Fatalf("必须带 id 调 CorpseOutput, got %s", got)
	}

	// 未知 id → 404（service 报 ErrSessionNotFound）
	code, _ = e.do("GET", "/api/term/sessions/999/output", "")
	if code != http.StatusNotFound {
		t.Fatalf("未知 id 应 404, got %d", code)
	}
}
