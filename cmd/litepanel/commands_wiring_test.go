package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/terminal"
	"litepanel/internal/ws"
)

// M5-T9 装配验收：快捷命令走完整链路（真 HTTP + 真 DB + 真 tmux）。
//
// 这里不重写任何单元层已验过的逻辑，只验"接上了"：路由挂对、facade 把
// 存储与注入器接对、危险命令的确认在真链路上真的拦得住。
//
// 服务与注入器都由生产装配函数给出，测试里绝不自己 new —— 那等于替生产
// 补上漏掉的一行，main 少接线也照样绿（M1/M4 都踩过这个坑）。

func newCmdWiringServer(t *testing.T, pwd string) (http.Handler, func(method, path, body string) (int, string)) {
	t.Helper()
	db := openTestDB(t)
	requireTermSessionsTable(t, db)
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	tw := wireTerminal(db, ws.NewHub())
	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, nil)
	// 走生产的装配入口，而不是在测试里逐个字段赋值：main 少接一根线时
	// 这里必须红。
	attachTerminalDeps(&deps, db, tw)

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
	return srv.Config.Handler, call
}

func TestCommandsAreWired(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("没有 tmux")
	}
	_, call := newCmdWiringServer(t, "cmd-wire-pass-1")

	code, body := call("POST", "/api/commands", `{"name":"装配验收","command":"echo 装配通了"}`)
	if code != http.StatusCreated {
		t.Fatalf("真装配下创建失败: %d %s", code, body)
	}
	id := jsonInt(t, body, "command", "id")

	if code, body := call("GET", "/api/commands", ""); code != 200 ||
		!strings.Contains(body, "装配验收") {
		t.Fatalf("列表该含刚建的命令: %d %s", code, body)
	}

	code, body = call("POST", "/api/commands/"+strconv.FormatInt(id, 10)+"/run", "")
	if code != http.StatusOK {
		t.Fatalf("执行应 200, got %d %s", code, body)
	}
	sessID := jsonInt(t, body, "", "session_id")
	name := terminal.TmuxName(sessID)
	t.Cleanup(func() {
		exec.Command("tmux", "kill-session", "-t", "="+name).Run()
	})

	// 断言取自 tmux 本身，不读面板返回的字符串：面板说自己"注入了"不算证据
	deadline := time.Now().Add(25 * time.Second)
	var screen string
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", "capture-pane", "-p", "-S", "-",
			"-t", name+":0.0").CombinedOutput()
		if err == nil {
			screen = string(out)
			if strings.Contains(screen, "装配通了") {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(screen, "装配通了") {
		t.Fatalf("命令没在真会话里执行:\n%s", screen)
	}
}

// 危险命令在真链路上真的被挡住：库里存的是 need_confirm=true（由 quickcmd
// 强制），HTTP 层据此拒绝，注入器一次都没被调到。
//
// 这条测的是"整条链是否闭合"，不是各段是否正确 —— 单元层各自都绿，而
// handler 改成"不查库直接执行"时只有这里会红。
func TestWiredDangerousCommandBlockedThenAllowed(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("没有 tmux")
	}
	_, call := newCmdWiringServer(t, "cmd-wire-pass-2")

	// 绝对路径：tmux 会话的 cwd 继承自测试进程（= 包目录），相对路径会
	// 把验收产物落进仓库。
	marker := filepath.Join(t.TempDir(), "cmd-danger-marker")
	// 用 touch 而不是 rm -rf 做验收：断言"没执行"必须是可观测的，而
	// "某个文件没被删"要先证明它本来在，噪声大且不稳。
	body := `{"name":"危险验收","command":"touch ` + marker + `"}`
	code, resp := call("POST", "/api/commands", body)
	if code != http.StatusCreated {
		t.Fatalf("创建失败: %d %s", code, resp)
	}
	// 没写 need_confirm，但命令里有 touch 之外的危险词才会被标；这里显式
	// 带 true 走"库里标了"的那条路径（自动标记由 quickcmd 包测）。
	id := jsonInt(t, resp, "command", "id")
	if code, resp := call("PATCH", "/api/commands/"+strconv.FormatInt(id, 10),
		`{"name":"危险验收","command":"touch `+marker+`","need_confirm":true}`); code != 200 {
		t.Fatalf("标记确认失败: %d %s", code, resp)
	}

	code, resp = call("POST", "/api/commands/"+strconv.FormatInt(id, 10)+"/run", "")
	if code != http.StatusBadRequest {
		t.Fatalf("未确认应 400, got %d %s", code, resp)
	}
	if !strings.Contains(resp, "confirm_required") {
		t.Fatalf("应回 confirm_required: %s", resp)
	}
	if _, err := exec.Command("test", "-e", marker).CombinedOutput(); err == nil {
		t.Fatal("未确认的命令被执行了!")
	}

	code, resp = call("POST", "/api/commands/"+strconv.FormatInt(id, 10)+"/run?confirm=1", "")
	if code != http.StatusOK {
		t.Fatalf("确认后应 200, got %d %s", code, resp)
	}
	// 必须回收这个会话：会话名由库里的 id 决定（lp-1），而每条测试都用
	// 全新的库（id 又从 1 开始）。留着它，下一条测试建 lp-1 时会在 tmux
	// 上撞名，报出来的错与被测逻辑毫无关系。
	t.Cleanup(func() {
		if sid := jsonInt(t, resp, "", "session_id"); sid > 0 {
			exec.Command("tmux", "kill-session", "-t", "="+terminal.TmuxName(sid)).Run()
		}
	})
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := exec.Command("test", "-e", marker).CombinedOutput(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("确认后命令没执行: %s", resp)
}

// busy 接口在真链路上报出真实忙闲（前端据此标标签）。
func TestWiredBusyReportsTruth(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("没有 tmux")
	}
	_, call := newCmdWiringServer(t, "cmd-wire-pass-3")

	// 先用一次执行造出一个真会话
	code, resp := call("POST", "/api/commands", `{"name":"造会话","command":"echo 起"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建失败: %d %s", code, resp)
	}
	cmdID := jsonInt(t, resp, "command", "id")
	code, resp = call("POST", "/api/commands/"+strconv.FormatInt(cmdID, 10)+"/run", "")
	if code != http.StatusOK {
		t.Fatalf("执行失败: %d %s", code, resp)
	}
	sessID := jsonInt(t, resp, "", "session_id")
	name := terminal.TmuxName(sessID)
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+name).Run() })

	// 等它闲下来（输出静默窗口过了、前台回到 shell）
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if foregroundOf(t, name) == shellOf(t, name) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 再等满静默窗口，否则"最近有输出"这条判据会一直成立
	time.Sleep(3 * time.Second)

	code, resp = call("GET", "/api/commands/busy?session="+strconv.FormatInt(sessID, 10), "")
	if code != 200 {
		t.Fatalf("应 200, got %d %s", code, resp)
	}
	if !strings.Contains(resp, `"busy":false`) {
		t.Fatalf("空闲会话该报 busy:false: %s", resp)
	}

	exec.Command("tmux", "send-keys", "-t", name+":0.0", "sleep 50", "Enter").Run()
	deadline = time.Now().Add(20 * time.Second)
	idle := resp
	for time.Now().Before(deadline) {
		_, resp = call("GET", "/api/commands/busy?session="+strconv.FormatInt(sessID, 10), "")
		if strings.Contains(resp, `"busy":true`) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("跑着 sleep 的会话该报 busy:true（先前: %s）", idle)
}

// shellOf 独立算出会话里那个 shell 的名字：取 pane_pid，再读
// /proc/<pid>/comm。
//
// 绝不复用被测代码里的 procComm，也绝不用 pane_current_command 当基准 ——
// 后者正是被测实现用来判忙的那个字段，拿它当"应该空闲"的判据等于让实现
// 自己作证（把 Busy 写成永远 false，这条等待循环第一次就满足）。
func shellOf(t *testing.T, name string) string {
	t.Helper()
	pidOut, err := exec.Command("tmux", "display-message", "-p", "-t", name+":0.0",
		"#{pane_pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(pidOut))
	b, err := os.ReadFile("/proc/" + pid + "/comm")
	if err != nil {
		t.Skipf("读不到 /proc/%s/comm: %v", pid, err)
	}
	return strings.TrimSpace(string(b))
}

// foregroundOf 问 tmux 要前台命令名（只用于"等它变成 sleep"这类等待，
// 不作为空闲判据）。
func foregroundOf(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("tmux", "display-message", "-p", "-t", name+":0.0",
		"#{pane_current_command}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func jsonInt(t *testing.T, body, obj, field string) int64 {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	if obj != "" {
		inner, ok := m[obj].(map[string]any)
		if !ok {
			t.Fatalf("%s 里缺 %s: %s", field, obj, body)
		}
		m = inner
	}
	v, ok := m[field].(float64)
	if !ok {
		t.Fatalf("字段 %s 不是数字: %s", field, body)
	}
	return int64(v)
}

// 排序方向必须真的改变列表顺序。
//
// api 层只验了"dir 原样送到存储"，而"up 意味着 sort 变小"这层映射在装配
// facade 里；两处都只测"参数传到了"，方向写反就没人管 —— 用户看到的
// 是"点上移，命令跑到下面去了"，一个每次点击都能重现但不报任何错的错。
func TestWiredCommandOrdering(t *testing.T) {
	_, call := newCmdWiringServer(t, "cmd-wire-pass-4")
	names := []string{"排在前面", "排在后面"}
	ids := make([]int64, 0, 2)
	for _, n := range names {
		code, resp := call("POST", "/api/commands", `{"name":"`+n+`","command":"echo x"}`)
		if code != http.StatusCreated {
			t.Fatalf("创建 %s 失败: %d %s", n, code, resp)
		}
		ids = append(ids, jsonInt(t, resp, "command", "id"))
	}

	list := func() string {
		code, resp := call("GET", "/api/commands", "")
		if code != 200 {
			t.Fatalf("列表失败 %d %s", code, resp)
		}
		return resp
	}
	before := list()
	if strings.Index(before, names[0]) > strings.Index(before, names[1]) {
		t.Fatalf("前置条件不成立：新建顺序就该是先建的在前: %s", before)
	}

	// 把第二条上移 → 它必须跑到第一条前面
	if code, resp := call("POST", "/api/commands/"+
		strconv.FormatInt(ids[1], 10)+"/move", `{"dir":"up"}`); code != 200 {
		t.Fatalf("上移失败: %d %s", code, resp)
	}
	after := list()
	if strings.Index(after, names[1]) > strings.Index(after, names[0]) {
		t.Fatalf("上移后顺序没变（方向可能反了）:\n前: %s\n后: %s", before, after)
	}
	// 再下移回来
	if code, _ := call("POST", "/api/commands/"+
		strconv.FormatInt(ids[1], 10)+"/move", `{"dir":"down"}`); code != 200 {
		t.Fatalf("下移失败: %d", code)
	}
	back := list()
	if strings.Index(back, names[0]) > strings.Index(back, names[1]) {
		t.Fatalf("下移没回到原顺序: %s", back)
	}
}
