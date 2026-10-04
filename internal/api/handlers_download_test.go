package api_test

// M7-T3：下载端点的 HTTP 层（设计 751–760 行的端点表）。
//
// 这一层最要紧的性质是**三类"没有数据"必须给出三个不同的状态码**，因为
// 它们的用户动作完全不同：
//
//	501  面板没接下载模块    → 这是面板自己的 bug，用户什么都做不了
//	503  aria2 没装/没起     → 用户去装 aria2 / systemctl start aria2
//	200 + 空列表 aria2 正常但没任务 → 用户该去点"新建下载"
//
// 把它们混成一个 200 + 空列表，前端就只能显示"还没有下载任务"，而真相是
// "aria2 根本没起来" —— 用户会反复点新建，每次都失败，且没有任何地方告诉
// 他为什么。这是设计里反复出现的那类错误（见 Metrics/Files 的 501 注释）。
//
// 用替身而不是真 aria2：这一层测的是状态码映射与入参校验，把 aria2 拉进来
// 只会让测试依赖外部进程（并且"aria2 没起"这一类恰恰没法用起着的 aria2 测）。
// 真 aria2 的协议正确性在 internal/download 那一层已经用 httptest + 实测覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/download"
)

// fakeDL 是 Downloads 接口的可编程替身。
type fakeDL struct {
	health  download.Health
	summary download.Summary
	tasks   []download.TaskView
	added   []download.AddInput
	addErr  error
	calls   []string

	pauseErr, resumeErr, removeErr, summaryErr, tasksErr error
	removed                                              []string
	historyCleared                                       int
}

func (f *fakeDL) record(s string) { f.calls = append(f.calls, s) }

func (f *fakeDL) Health(context.Context) download.Health {
	f.record("health")
	return f.health
}

func (f *fakeDL) Summary(context.Context) (download.Summary, error) {
	f.record("summary")
	return f.summary, f.summaryErr
}

func (f *fakeDL) Tasks(context.Context) ([]download.TaskView, error) {
	f.record("tasks")
	return f.tasks, f.tasksErr
}

func (f *fakeDL) Add(_ context.Context, in download.AddInput) (download.TaskView, error) {
	f.record("add")
	f.added = append(f.added, in)
	if f.addErr != nil {
		return download.TaskView{}, f.addErr
	}
	return download.TaskView{GID: "0xnew", Name: in.Out, Dir: in.Dir,
		URIs: in.URIs, State: "active"}, nil
}

func (f *fakeDL) Pause(_ context.Context, gid string) error {
	f.record("pause:" + gid)
	return f.pauseErr
}

func (f *fakeDL) Resume(_ context.Context, gid string) error {
	f.record("resume:" + gid)
	return f.resumeErr
}

func (f *fakeDL) Remove(_ context.Context, gid string, force bool) error {
	f.record(fmt.Sprintf("remove:%s:%v", gid, force))
	f.removed = append(f.removed, gid)
	return f.removeErr
}

func (f *fakeDL) ClearHistory(context.Context) (int, error) {
	f.record("clear")
	return f.historyCleared, nil
}

var _ api.Downloads = (*fakeDL)(nil)

func setUpDL(t *testing.T, f *fakeDL) *harness {
	t.Helper()
	return newHarnessWith(t, func(d *api.AuthDeps) { d.Downloads = f })
}

func (h *harness) dlGet(path string) *http.Response {
	h.t.Helper()
	return h.get(path, &http.Cookie{Name: cookieName, Value: h.token})
}

func (h *harness) dlDo(method, path, body string) *http.Response {
	h.t.Helper()
	return h.do(method, path, true, "litepanel", body, "127.0.0.1")
}

func dlBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	return out
}

func dlErrCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := json.Marshal(dlBody(t, resp))
	var eb struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(b, &eb)
	return eb.Code
}

// 没接下载模块 → 501，且**不能**是 200 + 空数据。
//
// 与 Metrics/Files/Term 的取舍同源：空壳会被前端渲染成"这台机器没有任何
// 下载任务"，把"面板装配漏了"翻译成"用户没用过这个功能"，两者要的动作完全
// 不同。这里逐个端点都测，因为漏接的表现是"只有一个端点 501"。
func TestDownloadEndpointsReturn501WhenNotWired(t *testing.T) {
	h := newHarness(t) // 不接 Downloads
	cases := []struct {
		method, path, body string
	}{
		{"GET", "/api/dl/health", ""},
		{"GET", "/api/dl/summary", ""},
		{"GET", "/api/dl/tasks", ""},
		{"POST", "/api/dl/tasks", `{"uris":["https://x/a"]}`},
		{"POST", "/api/dl/tasks/0x1/pause", ""},
		{"POST", "/api/dl/tasks/0x1/resume", ""},
		{"DELETE", "/api/dl/tasks/0x1", ""},
		{"DELETE", "/api/dl/history", ""},
	}
	for _, c := range cases {
		resp := h.dlDoOrGet(c.method, c.path, c.body)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s 未接线应 501, got %d", c.method, c.path, resp.StatusCode)
		}
	}
}

func (h *harness) dlDoOrGet(method, path, body string) *http.Response {
	h.t.Helper()
	if method == http.MethodGet {
		return h.get(path, &http.Cookie{Name: cookieName, Value: h.token})
	}
	return h.do(method, path, true, "litepanel", body, "127.0.0.1")
}

// 未登录时下载端点也要 401（而不是先撞上 501 泄露"这个面板没接 aria2"）。
// 顺序由中间件保证，但值得钉：路由改动很容易把 authed 漏在某个子树上。
func TestDownloadEndpointsRequireAuth(t *testing.T) {
	f := &fakeDL{}
	h := setUpDL(t, f)
	for _, p := range []string{"/api/dl/health", "/api/dl/summary", "/api/dl/tasks"} {
		if resp := h.get(p); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 未登录应 401, got %d", p, resp.StatusCode)
		}
	}
}

func TestHealthReportsAria2State(t *testing.T) {
	f := &fakeDL{health: download.Health{OK: true, Version: "1.37.0"}}
	h := setUpDL(t, f)
	resp := h.dlGet("/api/dl/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康查询本身不该因 aria2 状态而失败, got %d", resp.StatusCode)
	}
	got := dlBody(t, resp)
	if got["ok"] != true || got["version"] != "1.37.0" {
		t.Errorf("健康载荷不对: %v", got)
	}
}

// aria2 不可达时 health 仍回 **200**（ok=false），不是 5xx。
//
// 这是刻意的：health 的职责就是"报告 aria2 状态"，报告失败不是错误。前端拿
// 到 ok=false + message 才能渲染安装引导；如果这里回 503，前端一般会把它
// 归进"请求失败"分支，引导永远出不来。
func TestHealthIs200EvenWhenAria2Down(t *testing.T) {
	f := &fakeDL{health: download.Health{OK: false, Message: "aria2 不可达：可能未安装 aria2"}}
	h := setUpDL(t, f)
	resp := h.dlGet("/api/dl/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康端点应恒 200（aria2 状态是数据不是错误）, got %d", resp.StatusCode)
	}
	got := dlBody(t, resp)
	if got["ok"] != false {
		t.Error("ok 必须是 false")
	}
	if m, _ := got["message"].(string); !strings.Contains(m, "aria2") {
		t.Errorf("message 要能直接显示给用户, got %v", got["message"])
	}
}

// aria2 不可达时数据类端点回 **503 + 明确 code**，不是 200 + 空列表。
//
// 200 + 空会被渲染成"还没有下载任务"，用户于是反复点新建、每次都失败、
// 且没有任何地方说为什么。带 code 而不是靠文案：前端要据此切到"引导页"，
// 而对中文文案做子串匹配在本项目里是禁止的（改一个标点就失效）。
func TestTasksReturn503WhenAria2Unavailable(t *testing.T) {
	f := &fakeDL{tasksErr: errors.New("wrap: boom")}
	// 用真实的哨包装，避免手搓一个"看起来像"的错误。
	f.tasksErr = unavailableForTest()
	h := setUpDL(t, f)
	resp := h.dlGet("/api/dl/tasks")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("aria2 不可达应 503, got %d", resp.StatusCode)
	}
	if c := dlErrCode(t, resp); c != "aria2_unavailable" {
		t.Errorf("要带机器可判的 code, got %q", c)
	}
}

func TestSummaryReturn503WhenAria2Unavailable(t *testing.T) {
	f := &fakeDL{summaryErr: unavailableForTest()}
	h := setUpDL(t, f)
	if resp := h.dlGet("/api/dl/summary"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

// 添加任务失败的原因若是"aria2 没起"，也要 503 而不是 400。
// 400 会被前端当成"我填的 URL 有问题"，用户于是反复改 URL。
func TestAdd503WhenAria2Unavailable(t *testing.T) {
	f := &fakeDL{addErr: unavailableForTest()}
	h := setUpDL(t, f)
	resp := h.dlDo(http.MethodPost, "/api/dl/tasks", `{"uris":["https://x/a.iso"]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if c := dlErrCode(t, resp); c != "aria2_unavailable" {
		t.Errorf("got %q", c)
	}
}

func TestSummaryReturnsCountsAndSpeed(t *testing.T) {
	f := &fakeDL{summary: download.Summary{
		Speed: 524288, Active: 2, Waiting: 3, Paused: 1, Done: 40, Failed: 2,
	}}
	h := setUpDL(t, f)
	resp := h.dlGet("/api/dl/summary")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	got := dlBody(t, resp)
	for k, want := range map[string]float64{"speed": 524288, "active": 2, "waiting": 3, "paused": 1, "done": 40, "failed": 2} {
		if v, ok := got[k].(float64); !ok || v != want {
			t.Errorf("summary.%s 应为 %v, got %v", k, want, got[k])
		}
	}
}

func TestTasksReturnMergedView(t *testing.T) {
	f := &fakeDL{tasks: []download.TaskView{{
		GID: "0xa", URIs: []string{"https://x/a.iso"}, Name: "a.iso", Dir: "/DISK/downloads",
		State: "active", TotalBytes: 1000, DoneBytes: 250, Speed: 500, Connections: 8,
	}}}
	h := setUpDL(t, f)
	resp := h.dlGet("/api/dl/tasks")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	body := dlBody(t, resp)
	items, _ := body["tasks"].([]any)
	if len(items) != 1 {
		t.Fatalf("应返回 1 条, got %v", body["tasks"])
	}
	it := items[0].(map[string]any)
	// 数字必须是 JSON number 而不是字符串。aria2 侧全是字符串，转换在领域层
	// 做过一次；这里再退回去，前端就得每个消费点自己 Number()，漏一处就是
	// "250/1000" 这种字符串拼接出来的百分比。
	if v, ok := it["done_bytes"].(float64); !ok || v != 250 {
		t.Errorf("done_bytes 必须是数字, got %#v", it["done_bytes"])
	}
	if v, ok := it["speed"].(float64); !ok || v != 500 {
		t.Errorf("speed 必须是数字, got %#v", it["speed"])
	}
	if it["gid"] != "0xa" {
		t.Errorf("gid 不对: %v", it["gid"])
	}
}

// 空列表返回 `tasks: []` 而不是 `tasks: null`。
//
// Go 的 nil slice marshal 成 null，而前端 `data.tasks.length` 在 null 上直接
// 抛 TypeError —— 整个下载页白屏。空数组是这类"列表端点"的固定形状。
func TestEmptyTaskListIsArrayNotNull(t *testing.T) {
	f := &fakeDL{}
	h := setUpDL(t, f)
	resp := h.dlGet("/api/dl/tasks")
	raw := dlBody(t, resp)
	if raw["tasks"] == nil {
		t.Fatal(`tasks 不能是 null（前端 .length 会抛，整页白屏），必须是 []`)
	}
	items, ok := raw["tasks"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("应为空数组, got %#v", raw["tasks"])
	}
}

func TestAddPassesInputThrough(t *testing.T) {
	f := &fakeDL{}
	h := setUpDL(t, f)
	// dir 用真实存在的临时目录：面板会校验保存目录存在（见
	// TestAddRejectsMissingDir 的理由），本测试的重点是"入参逐项传到领域层"，
	// 用一个编出来的路径会让它顺带依赖"这台机器上没有 /DISK/downloads"。
	dir := t.TempDir()
	body := fmt.Sprintf(`{"uris":["https://x/a.iso","https://y/a.iso"],"dir":%q,"out":"a.iso","split":16}`, dir)
	resp := h.dlDo(http.MethodPost, "/api/dl/tasks", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("新建应 201, got %d", resp.StatusCode)
	}
	if len(f.added) != 1 {
		t.Fatalf("应调用一次 Add, got %d", len(f.added))
	}
	in := f.added[0]
	if len(in.URIs) != 2 || in.Dir != dir || in.Out != "a.iso" || in.Split != 16 {
		t.Errorf("入参没传对: %+v", in)
	}
	got := dlBody(t, resp)
	if got["gid"] != "0xnew" {
		t.Errorf("要回新任务, got %v", got)
	}
}

// 空 uris 必须在**面板侧**就被拒（400），不能转发给 aria2。
//
// aria2 收到空数组回的是 "URI is not provided."（实测），一条既不知道是什么
// 又没法在自己界面上定位的错误。而且这个请求根本不该出发。
func TestAddRejectsEmptyURIs(t *testing.T) {
	for _, body := range []string{`{}`, `{"uris":[]}`, `{"uris":[""]}`, `{"uris":["   "]}`} {
		f := &fakeDL{}
		h := setUpDL(t, f)
		resp := h.dlDo(http.MethodPost, "/api/dl/tasks", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400, got %d", body, resp.StatusCode)
		}
		if len(f.added) != 0 {
			t.Errorf("%s 不该转发给 aria2", body)
		}
	}
}

// URL 必须是 aria2 认的协议。
//
// 面板不校验的话，用户把 "www.example.com/a.zip"（漏了 https://）粘进去，
// aria2 会回一句 "URI is not supported"，或者更糟：把它当成一个本地文件路径
// 去尝试。校验完能给出"缺少 http:// 或 https:// 前缀"这种可直接照做的提示。
func TestAddRejectsUnsupportedSchemes(t *testing.T) {
	cases := []string{
		"www.example.com/a.zip",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"/tmp/local.iso",
	}
	for _, u := range cases {
		f := &fakeDL{}
		h := setUpDL(t, f)
		body := fmt.Sprintf(`{"uris":[%q]}`, u)
		resp := h.dlDo(http.MethodPost, "/api/dl/tasks", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q 应 400, got %d", u, resp.StatusCode)
		}
		if len(f.added) != 0 {
			t.Errorf("%q 不该转发给 aria2", u)
		}
	}
	// 反过来：合法协议必须放行，别把校验做成白名单太窄。
	for _, u := range []string{"https://x/a", "http://x/a", "ftp://x/a",
		"magnet:?xt=urn:btih:abc"} {
		f := &fakeDL{}
		h := setUpDL(t, f)
		resp := h.dlDo(http.MethodPost, "/api/dl/tasks", fmt.Sprintf(`{"uris":[%q]}`, u))
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("%q 合法却被拒, got %d", u, resp.StatusCode)
		}
	}
}

// 保存目录不存在时明确报错，而不是让 aria2 顺手 mkdir。
//
// aria2 会自动创建目标目录，因此"不校验"也能跑通 —— 但打错一个字的结果是
// 任务安安静静地往一个新建出来的错误目录里下几十 GB，用户在预期位置找不到
// 文件，还要面对一个凭空多出来的目录。宁可拒绝并说明。
func TestAddRejectsMissingDir(t *testing.T) {
	dir := t.TempDir()
	f := &fakeDL{}
	h := setUpDL(t, f)
	body := fmt.Sprintf(`{"uris":["https://x/a"],"dir":%q}`, filepath.Join(dir, "没有这个目录"))
	resp := h.dlDo(http.MethodPost, "/api/dl/tasks", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("目录不存在应 400, got %d", resp.StatusCode)
	}
	if len(f.added) != 0 {
		t.Error("目录不存在时不该提交给 aria2")
	}
}

func TestAddAcceptsExistingDir(t *testing.T) {
	dir := t.TempDir()
	f := &fakeDL{}
	h := setUpDL(t, f)
	body := fmt.Sprintf(`{"uris":["https://x/a"],"dir":%q}`, dir)
	if resp := h.dlDo(http.MethodPost, "/api/dl/tasks", body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("存在的目录应放行, got %d", resp.StatusCode)
	}
	// 不填 dir 也要放行（用 aria2 的 --dir 默认值）。
	f2 := &fakeDL{}
	h2 := setUpDL(t, f2)
	if resp := h2.dlDo(http.MethodPost, "/api/dl/tasks", `{"uris":["https://x/a"]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("不填 dir 应放行, got %d", resp.StatusCode)
	}
}

// split 越界要夹住而不是原样转发。
//
// aria2 对 split 的合法上界来自它自己的 max-concurrent-connections；给它一个
// 荒谬的值（比如 1e9）不会报错，而是把内存和连接数按用户输入放大 —— 这是
// 一个可以从 web 界面发起的资源放大面。夹住之后仍然是"成功但比预期少"，
// 但至少不伤机器，并且响应里回显实际生效值。
func TestAddClampsSplit(t *testing.T) {
	f := &fakeDL{}
	h := setUpDL(t, f)
	resp := h.dlDo(http.MethodPost, "/api/dl/tasks", `{"uris":["https://x/a"],"split":999999}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if f.added[0].Split > download.MaxSplit {
		t.Errorf("split 必须夹到上限, got %d (上限 %d)", f.added[0].Split, download.MaxSplit)
	}
	// 负数同理：aria2 会把负 split 当非法值静默忽略（用户以为多线程是假的）。
	f2 := &fakeDL{}
	h2 := setUpDL(t, f2)
	h2.dlDo(http.MethodPost, "/api/dl/tasks", `{"uris":["https://x/a"],"split":-5}`)
	if f2.added[0].Split < 0 {
		t.Errorf("负 split 不该原样转发, got %d", f2.added[0].Split)
	}
}

// 请求体不是合法 JSON → 400，不能 500。
func TestAddRejectsMalformedBody(t *testing.T) {
	f := &fakeDL{}
	h := setUpDL(t, f)
	for _, body := range []string{`{`, `[]`, `"x"`, ``} {
		if resp := h.dlDo(http.MethodPost, "/api/dl/tasks", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("畸形请求体 %q 应 400, got %d", body, resp.StatusCode)
		}
	}
}

func TestPauseAndResumeHitService(t *testing.T) {
	f := &fakeDL{}
	h := setUpDL(t, f)
	if resp := h.dlDo(http.MethodPost, "/api/dl/tasks/0xabc/pause", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("pause got %d", resp.StatusCode)
	}
	if resp := h.dlDo(http.MethodPost, "/api/dl/tasks/0xabc/resume", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("resume got %d", resp.StatusCode)
	}
	want := []string{"pause:0xabc", "resume:0xabc"}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Errorf("调用不对: %v", f.calls)
	}
}

// gid 不存在 → 404。
//
// 与"aria2 拒绝了这个 gid"区分开：前者是面板/前端的引用失效（列表陈旧，
// 记录已被清除），后者要展示 aria2 的原文。两者的修法不同。
func TestPauseUnknownGidIs404(t *testing.T) {
	f := &fakeDL{pauseErr: download.ErrNoTask}
	h := setUpDL(t, f)
	resp := h.dlDo(http.MethodPost, "/api/dl/tasks/0xghost/pause", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

// aria2 的业务错误要透传原文 + 4xx。
//
// 典型场景：对一个已完成的任务点"继续"，aria2 回 "The gid of the download
// result is not found" 之类。吞掉原文只剩"操作失败"，用户无从判断是按钮点错
// 了还是 aria2 坏了 —— 这是用户明确反对过的那种错误显示。
func TestAria2BusinessErrorKeepsOriginalMessage(t *testing.T) {
	f := &fakeDL{resumeErr: &download.Error{
		Code: 1, Message: "Download not present.", Method: "aria2.unpause"}}
	h := setUpDL(t, f)
	resp := h.dlDo(http.MethodPost, "/api/dl/tasks/0x1/resume", "")
	if resp.StatusCode >= 500 {
		t.Fatalf("aria2 的业务拒绝不该回 5xx, got %d", resp.StatusCode)
	}
	body := dlBody(t, resp)
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "Download not present") {
		t.Errorf("要保留 aria2 原文, got %q", msg)
	}
}

func TestRemovePassesForceFlag(t *testing.T) {
	f := &fakeDL{}
	h := setUpDL(t, f)
	if resp := h.dlDo(http.MethodDelete, "/api/dl/tasks/0xg", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if len(f.removed) != 1 || f.removed[0] != "0xg" {
		t.Errorf("没删对: %v", f.removed)
	}
	// force=1 要传到领域层（forceRemove 会留下 .aria2 控制文件，是需要用户
	// 明确选择的动作，不该默认发生）。
	f2 := &fakeDL{}
	h2 := setUpDL(t, f2)
	h2.dlDo(http.MethodDelete, "/api/dl/tasks/0xg?force=1", "")
	var sawForce bool
	for _, c := range f2.calls {
		if c == "remove:0xg:true" {
			sawForce = true
		}
	}
	if !sawForce {
		t.Errorf("force 没传到领域层: %v", f2.calls)
	}
}

func TestClearHistoryReturnsCount(t *testing.T) {
	f := &fakeDL{historyCleared: 7}
	h := setUpDL(t, f)
	resp := h.dlDo(http.MethodDelete, "/api/dl/history", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	got := dlBody(t, resp)
	if n, _ := got["cleared"].(float64); n != 7 {
		t.Errorf("要回清除条数, got %v", got)
	}
}

// 写端点的 CSRF 保护覆盖下载路由（自定义头缺失 → 403）。
// 逐端点各测一次：CSRF 守卫挂在子路由上，漏挂某一个很容易。
func TestDownloadWritesNeedCSRFHeader(t *testing.T) {
	f := &fakeDL{}
	h := setUpDL(t, f)
	cases := []struct{ method, path, body string }{
		{"POST", "/api/dl/tasks", `{"uris":["https://x/a"]}`},
		{"POST", "/api/dl/tasks/0x1/pause", ""},
		{"POST", "/api/dl/tasks/0x1/resume", ""},
		{"DELETE", "/api/dl/tasks/0x1", ""},
		{"DELETE", "/api/dl/history", ""},
	}
	for _, c := range cases {
		resp := h.do(c.method, c.path, true, "", c.body, "127.0.0.1")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s 缺 CSRF 头应 403, got %d", c.method, c.path, resp.StatusCode)
		}
	}
}

// unavailableForTest 造一个"aria2 连不上"的错误。
//
// 用 download 包的公开构造函数而不是手搓一个 error：handler 的分类判据是
// download.IsUnavailable，手搓的错误不会满足它，测试就会测到一个假分支。
func unavailableForTest() error { return download.NewUnavailable(errors.New("connection refused")) }
