package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/quickcmd"
)

// M5-T9：快捷命令的 HTTP 层（设计 688-690 行，接口表：/api/commands*）。

// fakeCommands 是一个内存实现，只记录"被调了没"。
//
// 故意不做得像真存储（不做校验、不做排序）：校验和排序是 quickcmd 包的
// 职责并且在那边测过；这里只验 HTTP 层的映射 —— 状态码、字段名、以及
// "危险命令没带确认时绝不能调到执行层"。
type fakeCommands struct {
	items  map[int64]quickcmd.Command
	next   int64
	runRes quickcmd.Result
	runErr error

	runCalls    []int64
	moveCalls   []string
	deleteCalls []int64
	busyIDs     [][]int64
	states      map[int64]quickcmd.BusyInfo
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{items: map[int64]quickcmd.Command{},
		states: map[int64]quickcmd.BusyInfo{}}
}

func (f *fakeCommands) List(context.Context) ([]quickcmd.Command, error) {
	var out []quickcmd.Command
	for _, c := range f.items {
		out = append(out, c)
	}
	return out, nil
}

// fakeErr 模仿 quickcmd 的**契约**（空白 → 具名错误），不是在重实现校验：
// 判据的主人与被测处都在 quickcmd 包（那边有 TestCommandValidation）。
// 这里必须让假实现会返回这两种错误，否则"HTTP 层怎么映射领域错误"这件事
// 就完全没被覆盖 —— 一个只会成功的假实现，让 handler 里删掉 errors.Is 分支
// 也照样绿。
func fakeErr(c quickcmd.Command) error {
	if strings.TrimSpace(c.Name) == "" {
		return quickcmd.ErrNameRequired
	}
	if strings.TrimSpace(c.Command) == "" {
		return quickcmd.ErrCommandRequired
	}
	return nil
}

func (f *fakeCommands) Create(_ context.Context, c quickcmd.Command) (quickcmd.Command, error) {
	if err := fakeErr(c); err != nil {
		return quickcmd.Command{}, err
	}
	f.next++
	c.ID = f.next
	f.items[c.ID] = c
	return c, nil
}

func (f *fakeCommands) Update(_ context.Context, id int64, in quickcmd.Command) error {
	if err := fakeErr(in); err != nil {
		return err
	}
	old, ok := f.items[id]
	if !ok {
		return quickcmd.ErrNotFound
	}
	in.ID = id
	in.Sort = old.Sort
	f.items[id] = in
	return nil
}

func (f *fakeCommands) Delete(_ context.Context, id int64) error {
	if _, ok := f.items[id]; !ok {
		return quickcmd.ErrNotFound
	}
	f.deleteCalls = append(f.deleteCalls, id)
	delete(f.items, id)
	return nil
}

func (f *fakeCommands) Move(_ context.Context, id int64, dir string) error {
	if _, ok := f.items[id]; !ok {
		return quickcmd.ErrNotFound
	}
	f.moveCalls = append(f.moveCalls, fmt.Sprintf("%d:%s", id, dir))
	return nil
}

func (f *fakeCommands) Get(_ context.Context, id int64) (quickcmd.Command, error) {
	c, ok := f.items[id]
	if !ok {
		return quickcmd.Command{}, quickcmd.ErrNotFound
	}
	return c, nil
}

func (f *fakeCommands) Run(_ context.Context, c quickcmd.Command) (quickcmd.Result, error) {
	f.runCalls = append(f.runCalls, c.ID)
	if f.runErr != nil {
		return quickcmd.Result{}, f.runErr
	}
	if f.runRes.SessionID != 0 {
		return f.runRes, nil
	}
	return quickcmd.Result{SessionID: 7, Title: "快捷命令 · " + c.Name}, nil
}

func (f *fakeCommands) Busy(_ context.Context, ids []int64) (map[int64]quickcmd.BusyInfo, error) {
	f.busyIDs = append(f.busyIDs, append([]int64(nil), ids...))
	out := map[int64]quickcmd.BusyInfo{}
	for _, id := range ids {
		out[id] = f.states[id]
	}
	return out, nil
}

func newCmdEnv(t *testing.T, c api.Commands) *pwEnv {
	t.Helper()
	const initial = "cmd-initial-123"
	const newer = "cmd-changed-456"
	e := newPWEnvWith(t, initial, func(d *api.AuthDeps) { d.Commands = c })
	e.login(initial)
	if code, body := e.do("POST", "/api/password",
		`{"old":"`+initial+`","new":"`+newer+`"}`); code != http.StatusOK {
		t.Fatalf("改密失败: %d %+v", code, body)
	}
	e.login(newer)
	return e
}

func (e *pwEnv) cmd(method, path, body string) (int, map[string]any) {
	e.t.Helper()
	return e.do(method, "/api/commands"+path, body)
}

// 未登录必须 401：快捷命令能执行任意 shell，这是全面板最敏感的接口。
func TestCommandsRequireAuth(t *testing.T) {
	e := newPWEnvWith(t, "cmd-initial-123", func(d *api.AuthDeps) {
		d.Commands = newFakeCommands()
	})
	if code, _ := e.do("GET", "/api/commands", ""); code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401, got %d", code)
	}
	if code, _ := e.do("POST", "/api/commands/1/run", "{}"); code != http.StatusUnauthorized {
		t.Fatalf("执行接口未登录应 401, got %d", code)
	}
}

func TestCommandsListFields(t *testing.T) {
	f := newFakeCommands()
	e := newCmdEnv(t, f)
	if code, _ := e.cmd("POST", "",
		`{"name":"看磁盘","command":"df -h","cwd":"/root"}`); code != http.StatusCreated {
		t.Fatalf("创建应 201, got %d", code)
	}
	code, m := e.cmd("GET", "", "")
	if code != 200 {
		t.Fatalf("应 200, got %d %+v", code, m)
	}
	raw, _ := json.Marshal(m["commands"])
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("commands 不是数组: %s", raw)
	}
	if len(got) != 1 {
		t.Fatalf("应 1 条, got %s", raw)
	}
	one := got[0]
	// 字段名必须与 Go struct tag 逐字一致：前端直接取，不做大小写转换
	for _, k := range []string{"id", "name", "command", "cwd", "need_confirm", "sort", "created_at"} {
		if _, ok := one[k]; !ok {
			t.Errorf("缺字段 %q: %s", k, raw)
		}
	}
	if one["name"] != "看磁盘" || one["command"] != "df -h" || one["cwd"] != "/root" {
		t.Errorf("内容不对: %s", raw)
	}
}

// 空列表必须是 []，不是 null。
//
// 前端写的是 list.length；null 会直接抛 TypeError，整个"快捷命令"页空白，
// 而控制台里只有一条看不出因果的报错。
func TestCommandsListEmptyIsArray(t *testing.T) {
	e := newCmdEnv(t, newFakeCommands())
	code, body := e.cmd("GET", "", "")
	if code != 200 {
		t.Fatalf("应 200, got %d", code)
	}
	if body["commands"] == nil {
		t.Fatalf("空列表应序列化成 []，不能是 null: %+v", body)
	}
}

// 空白命令必须 400，而且不能落库。
func TestCommandsCreateRejectsBlank(t *testing.T) {
	f := newFakeCommands()
	e := newCmdEnv(t, f)
	for _, body := range []string{
		`{"name":"","command":"uptime"}`,        // 没名字
		`{"name":"x","command":"   "}`,          // 只有空白
		`{"name":"x","command":""}`,             // 空命令
		`{"name":"x","command":"ls","bogus":1}`, // 未知字段
	} {
		code, m := e.cmd("POST", "", body)
		if code != http.StatusBadRequest {
			t.Errorf("应 400: %s → %d %+v", body, code, m)
		}
	}
	if len(f.items) != 0 {
		t.Fatalf("非法输入不该落库: %+v", f.items)
	}
}

// 用户主动开的确认必须原样送到领域层，handler 不能把它掺手改掉。
//
// 危险命令的**强制**确认属于 quickcmd.normalized()（只有那一处主人，
// 已在 quickcmd 包测过）；这里只验“透传”：handler 如果为了方便而
// 写死 need_confirm:false，用户在表单里勾的“每次确认”就会默默失效；
// 反过来写死 true 则每条都要确认，会把人赶回 SSH。
func TestCommandsCreatePassesConfirmThrough(t *testing.T) {
	f := newFakeCommands()
	e := newCmdEnv(t, f)
	code, m := e.cmd("POST", "",
		`{"name":"每天重启一次 nginx","command":"systemctl reload nginx","need_confirm":true}`)
	if code != http.StatusCreated {
		t.Fatalf("应 201, got %d %+v", code, m)
	}
	raw, _ := json.Marshal(m["command"])
	var one map[string]any
	_ = json.Unmarshal(raw, &one)
	if one["need_confirm"] != true {
		t.Fatalf("用户勾的确认被handler 丢了: %s", raw)
	}
	if code, _ := e.cmd("POST", "", `{"name":"x","command":"ls"}`); code != 201 {
		t.Fatalf("创建失败 %d", code)
	}
	if len(f.items) != 2 {
		t.Fatalf("应落库两条, got %d", len(f.items))
	}
}

func TestCommandsUpdateAndDelete(t *testing.T) {
	f := newFakeCommands()
	e := newCmdEnv(t, f)
	code, m := e.cmd("POST", "", `{"name":"旧名","command":"uptime"}`)
	if code != 201 {
		t.Fatalf("创建应 201, got %d", code)
	}
	raw, _ := json.Marshal(m["command"])
	var one map[string]any
	_ = json.Unmarshal(raw, &one)
	id := int64(one["id"].(float64))

	if code, _ := e.cmd("PATCH", fmt.Sprintf("/%d", id),
		`{"name":"新名","command":"free -m"}`); code != http.StatusOK {
		t.Fatalf("改应 200, got %d", code)
	}
	if f.items[id].Name != "新名" || f.items[id].Command != "free -m" {
		t.Fatalf("没改到: %+v", f.items[id])
	}
	if code, _ := e.cmd("DELETE", fmt.Sprintf("/%d", id), ""); code != http.StatusOK {
		t.Fatalf("删应 200, got %d", code)
	}
	if len(f.deleteCalls) != 1 {
		t.Fatalf("该调到 Delete, got %v", f.deleteCalls)
	}
	// 都不存在的 id 必须 404，不能静默成功
	if code, _ := e.cmd("PATCH", "/9999", `{"name":"x","command":"y"}`); code != http.StatusNotFound {
		t.Fatalf("改不存在的应 404, got %d", code)
	}
	if code, _ := e.cmd("DELETE", "/9999", ""); code != http.StatusNotFound {
		t.Fatalf("删不存在的应 404, got %d", code)
	}
}

func TestCommandsMove(t *testing.T) {
	f := newFakeCommands()
	e := newCmdEnv(t, f)
	if code, _ := e.cmd("POST", "", `{"name":"a","command":"a"}`); code != 201 {
		t.Fatalf("创建失败 %d", code)
	}
	if code, _ := e.cmd("POST", "/1/move", `{"dir":"up"}`); code != http.StatusOK {
		t.Fatalf("上移应 200, got %d", code)
	}
	if code, _ := e.cmd("POST", "/1/move", `{"dir":"down"}`); code != http.StatusOK {
		t.Fatalf("下移应 200, got %d", code)
	}
	// 方向写错必须 400：静默当"没方向"处理的话，前端传错值表现为
	// "点了按钮什么都不动"，没有任何地方报错
	if code, _ := e.cmd("POST", "/1/move", `{"dir":"left"}`); code != http.StatusBadRequest {
		t.Fatalf("非法方向应 400, got %d", code)
	}
	if strings.Join(f.moveCalls, ",") != "1:up,1:down" {
		t.Fatalf("传给存储的方向不对: %v", f.moveCalls)
	}
}

// ---- 执行 ----

// 点一下就跑，返回会话 id 与"是否新建"。
//
// 必须回 2xx：前端拿到 session_id 才能跳到终端页并选中那个标签；
// is_new_session 决定 toast 文案（"已在你当前会话执行" vs "会话忙，已另开一个"）。
func TestCommandsRunReturnsTarget(t *testing.T) {
	f := newFakeCommands()
	f.runRes = quickcmd.Result{SessionID: 42, IsNewSession: true, Title: "快捷命令 · 看磁盘"}
	e := newCmdEnv(t, f)
	if code, _ := e.cmd("POST", "", `{"name":"看磁盘","command":"df -h"}`); code != 201 {
		t.Fatalf("创建失败 %d", code)
	}
	code, m := e.cmd("POST", "/1/run", "")
	if code < 200 || code >= 300 {
		t.Fatalf("应 2xx, got %d %+v", code, m)
	}
	if m["session_id"] != float64(42) {
		t.Fatalf("该回 session_id=42: %+v", m)
	}
	if m["is_new_session"] != true {
		t.Fatalf("该回 is_new_session=true: %+v", m)
	}
	if len(f.runCalls) != 1 || f.runCalls[0] != 1 {
		t.Fatalf("该执行 id=1 一次, got %v", f.runCalls)
	}
}

// 已标 need_confirm 的命令，不带 confirm 参数时**执行层一次都不能被调到**。
//
// 这是整个 D20 最关键的一条守卫，而且必须在服务端：确认框只在前端的话，
// 任何漏改的调用、脚本、curl 都会让 `rm -rf` 直接落到会话里。
//
// 这里直接写 need_confirm:true、而不是靠“rm -rf 会被自动标上”：
// 后者是 quickcmd.normalized() 的职责并且已在那边测过，假实现不该
// 把真存储的行为再实现一遗（两处主人必终漂开）。“危险→自动标上→
// 跑不了”这条链的整体liveness 由 cmd/litepanel 的真存储装配测试兜。
//
// 断言看 runCalls 而不是只看状态码 —— 先执行再回 400 的实现也能骗过后者。
func TestCommandsRunNeedsConfirm(t *testing.T) {
	f := newFakeCommands()
	e := newCmdEnv(t, f)
	if code, _ := e.cmd("POST", "",
		`{"name":"删日志","command":"rm -rf /var/log/*","need_confirm":true}`); code != 201 {
		t.Fatalf("创建失败 %d", code)
	}
	code, m := e.cmd("POST", "/1/run", "")
	if code != http.StatusBadRequest {
		t.Fatalf("需确认而未确认应 400, got %d %+v", code, m)
	}
	if m["code"] != "confirm_required" {
		t.Fatalf("应回 confirm_required 让前端弹确认框: %+v", m)
	}
	if len(f.runCalls) != 0 {
		t.Fatalf("未确认时绝不能执行! runCalls=%v", f.runCalls)
	}
	// 带上确认就放行（这条单独不能省：否则"永远拒绝"也能过上面那条）
	if code, _ := e.cmd("POST", "/1/run?confirm=1", ""); code < 200 || code >= 300 {
		t.Fatalf("确认后应 2xx, got %d", code)
	}
	if len(f.runCalls) != 1 {
		t.Fatalf("确认后该执行一次, got %v", f.runCalls)
	}
}

// 安全命令不需要 confirm 参数（每条都要确认会把人赶回 SSH）。
func TestCommandsRunSafeNeedsNoConfirm(t *testing.T) {
	f := newFakeCommands()
	e := newCmdEnv(t, f)
	if code, _ := e.cmd("POST", "", `{"name":"负载","command":"uptime"}`); code != 201 {
		t.Fatalf("创建失败 %d", code)
	}
	code, _ := e.cmd("POST", "/1/run", "")
	if code < 200 || code >= 300 {
		t.Fatalf("安全命令不该要确认, got %d", code)
	}
	if len(f.runCalls) != 1 {
		t.Fatalf("该执行一次, got %v", f.runCalls)
	}
}

func TestCommandsRunUnknownID(t *testing.T) {
	f := newFakeCommands()
	e := newCmdEnv(t, f)
	code, _ := e.cmd("POST", "/9999/run", "")
	if code != http.StatusNotFound {
		t.Fatalf("不存在的命令应 404, got %d", code)
	}
	if len(f.runCalls) != 0 {
		t.Fatalf("不存在的命令不该执行: %v", f.runCalls)
	}
}

// busy 探测：一次请求问全部会话，返回每个的忙闲。
//
// 必须是**一个**端点返回全部：前端要刷四个标签，逐个请求就是四倍往返，
// 每次往返在 tmux 上都是 fork。
func TestCommandsBusyBatch(t *testing.T) {
	f := newFakeCommands()
	f.states[1] = quickcmd.BusyInfo{Busy: true, Foreground: "sleep", ShellName: "bash"}
	f.states[2] = quickcmd.BusyInfo{Busy: false, Foreground: "bash", ShellName: "bash"}
	e := newCmdEnv(t, f)
	if code, _ := e.cmd("POST", "", `{"name":"a","command":"a"}`); code != 201 {
		t.Fatalf("创建失败 %d", code)
	}
	// 会话列表来自终端模块，不在这里造：busy 只按传入的 id 问 tmux
	code, m := e.cmd("GET", "/busy?session=1&session=2", "")
	if code != 200 {
		t.Fatalf("应 200, got %d %+v", code, m)
	}
	raw, _ := json.Marshal(m["sessions"])
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("sessions 不是数组: %s", raw)
	}
	if len(got) != 2 {
		t.Fatalf("应两条, got %s", raw)
	}
	byID := map[float64]bool{}
	for _, s := range got {
		byID[s["session_id"].(float64)] = s["busy"] == true
	}
	if !byID[1] {
		t.Errorf("前台是 sleep 的会话该判忙: %s", raw)
	}
	if byID[2] {
		t.Errorf("前台就是 shell 的会话该判空闲: %s", raw)
	}
}

// 重复的 session 参数必须先去重再往下传。
//
// 前端一次刷四个标签，URL 里出现重复 id 很常见（用户连点、或者列表还没
// 刷完又拼了一遍）。每个 id 一次 tmux fork 是这台机器上最贵的操作，而
// 重复查询不会让结果更对 —— 结果是同一份状态被问两遍。
func TestCommandsBusyDedupes(t *testing.T) {
	f := newFakeCommands()
	f.states[1] = quickcmd.BusyInfo{Busy: true, Foreground: "sleep"}
	e := newCmdEnv(t, f)
	if code, _ := e.cmd("GET", "/busy?session=1&session=1&session=2", ""); code != 200 {
		t.Fatalf("应 200, got %d", code)
	}
	if len(f.busyIDs) != 1 {
		t.Fatalf("该只往下传一次, got %v", f.busyIDs)
	}
	if got := fmt.Sprint(f.busyIDs[0]); got != "[1 2]" {
		t.Fatalf("重复 id 该被去掉且保持首次顺序, got %s", got)
	}
}

// session 参数不是数字时必须 400。
//
// 静默丢掉非法项会让前端的一个 bug 表现为"某个标签永远不刷状态"，
// 而那种"少了但不报错"最难查。
func TestCommandsBusyRejectsGarbage(t *testing.T) {
	e := newCmdEnv(t, newFakeCommands())
	code, _ := e.cmd("GET", "/busy?session=abc", "")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 session 参数应 400, got %d", code)
	}
	// 一个参数都没有则是正常状态（页面刚打开）
	if code, _ := e.cmd("GET", "/busy", ""); code != http.StatusOK {
		t.Fatalf("无参数应 200, got %d", code)
	}
}

// 依赖没接线时必须 501，不能回空列表。
//
// 与 metrics / 终端同样的理由：200 + [] 会被渲染成"服务器上没有任何快捷
// 命令"，用户会以为功能坏了，而真问题是面板没接上这个模块。
func TestCommandsNotWiredIs501(t *testing.T) {
	// 必须传一个"已登录且不必再改密"的环境：否则拿到的是 403（强制改密），
	// 结论与"接没接线"毫无关系，测试就变成了自证。
	e := newCmdEnv(t, nil)
	for _, r := range [][2]string{{"GET", "/api/commands"}, {"POST", "/api/commands/1/run"}} {
		code, m := e.do(r[0], r[1], "")
		if code != http.StatusNotImplemented {
			t.Errorf("%s %s 应 501, got %d %+v", r[0], r[1], code, m)
		}
	}
}

// 存储层报错必须是 5xx，不能伪装成空列表。
func TestCommandsListStoreError(t *testing.T) {
	e := newCmdEnv(t, &errCommands{})
	code, m := e.cmd("GET", "", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("存储报错应 500, got %d %+v", code, m)
	}
	if _, ok := m["commands"]; ok {
		t.Fatalf("500 的错误体里不该带 commands，前端会把它当空列表渲染: %+v", m)
	}
}

type errCommands struct{ fakeCommands }

func (errCommands) List(context.Context) ([]quickcmd.Command, error) {
	return nil, fmt.Errorf("db is broken")
}
