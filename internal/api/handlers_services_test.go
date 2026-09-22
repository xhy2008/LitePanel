package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/service"
)

// ---------- 脚手架 ----------

type svcEnv struct {
	*pwEnv
	sup *service.Supervisor
}

func newSvcEnv(t *testing.T) *svcEnv {
	t.Helper()
	e := &svcEnv{}
	pw := newPWEnvWith(t, "initial-pass-123", func(d *api.AuthDeps) {
		sup := service.NewSupervisor(d.DB)
		// 生产里这步在 cmd/litepanel 的 wireServices（装配归装配、路由归路由）。
		sup.OnEvent(api.BroadcastServiceEvent(d.Hub))
		sup.OnLog(api.BroadcastServiceLog(d.Hub))
		d.Services = sup
		e.sup = sup
	})
	e.pwEnv = pw
	pw.login("initial-pass-123")
	// 初始密码状态下除改密外一律 403，先把它改掉。
	if code, body := pw.do("POST", "/api/password",
		`{"old":"initial-pass-123","new":"service-test-pass-9"}`); code != http.StatusOK {
		pw.t.Fatalf("改初始密码失败: %d %+v", code, body)
	}
	// 改密会吊销所有会话（包括当前这个），必须重新登录。
	pw.login("service-test-pass-9")
	return e
}

func (e *svcEnv) create(body string) (int, map[string]any) {
	return e.do("POST", "/api/services", body)
}

func (e *svcEnv) list() []map[string]any {
	e.t.Helper()
	code, body := e.do("GET", "/api/services", "")
	if code != http.StatusOK {
		e.t.Fatalf("列表应 200, got %d %+v", code, body)
	}
	raw, err := json.Marshal(body["services"])
	if err != nil {
		e.t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("services 应是数组: %v (%s)", err, raw)
	}
	return out
}

// waitRow 等 watcher 把状态写回 DB 后被下一次列表读到。
func (e *svcEnv) waitRow(id float64, want string) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, row := range e.list() {
			if v, _ := row["id"].(float64); v == id && row["state"] == want {
				return row
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	e.t.Fatalf("等服务 %v 变成 %s 超时", id, want)
	return nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// 僵尸算已死：被杀但没被 wait 时 /proc/<pid> 仍然存在。
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := strings.LastIndex(string(b), ")")
	if i < 0 || i+2 >= len(b) {
		return false
	}
	return b[i+2] != 'Z'
}

// ---------- 鉴权 ----------

// 服务接口能启停任意命令，必须鉴权。回归目标是真实路由表，
// 不是测试自己搭的小 router —— 漏挂 authed() 这类错只有这里能抓到。
func TestServicesRequireAuth(t *testing.T) {
	var sup *service.Supervisor
	e := newPWEnvWith(t, "initial-pass-123", func(d *api.AuthDeps) {
		sup = service.NewSupervisor(d.DB)
		d.Services = sup
	})
	// 故意不登录。
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/services"},
		{"POST", "/api/services/1/toggle"},
		{"DELETE", "/api/services/1"},
	} {
		code, _ := e.do(c.method, c.path, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未登录应 401, got %d", c.method, c.path, code)
		}
	}
	_ = sup
}

// ---------- CRUD ----------

func TestServicesListEmptyIsArray(t *testing.T) {
	e := newSvcEnv(t)
	if got := e.list(); len(got) != 0 {
		t.Fatalf("空列表应是 []，不能是 null: %+v", got)
	}
}

// 前端直接读这些 key，字段名必须与 Go struct tag 一致（snake_case）。
func TestCreateServiceReturnsSnakeCase(t *testing.T) {
	e := newSvcEnv(t)
	code, body := e.create(`{"name":"web","kind":"command","start_cmd":"sleep 1",
		"cwd":"/tmp","autostart":true,"sort":3}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("创建应成功, got %d %+v", code, body)
	}
	for _, k := range []string{"id", "name", "kind", "start_cmd", "stop_cmd",
		"cwd", "autostart", "sort", "state", "pid"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("响应缺字段 %q（前端按 snake_case 直接取）: %+v", k, body)
		}
	}
	if body["state"] != "stopped" {
		t.Fatalf("新建服务应是 stopped, got %v", body["state"])
	}
}

func TestCreateServiceValidation(t *testing.T) {
	e := newSvcEnv(t)
	cases := map[string]string{
		"command 缺启动命令": `{"name":"a","kind":"command"}`,
		"systemd 缺单元":   `{"name":"b","kind":"systemd"}`,
		"未知类型":          `{"name":"c","kind":"docker","start_cmd":"x"}`,
		"名字为空":          `{"name":"  ","kind":"command","start_cmd":"x"}`,
	}
	for name, body := range cases {
		code, _ := e.create(body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: 应 400, got %d", name, code)
		}
	}
	if got := e.list(); len(got) != 0 {
		t.Fatalf("校验失败的请求不该留下记录: %+v", got)
	}
}

// 重名无法在磁贴上区分，原型也没打算处理，直接拒。
func TestCreateServiceDuplicateName(t *testing.T) {
	e := newSvcEnv(t)
	if code, _ := e.create(`{"name":"dup","kind":"command","start_cmd":"sleep 1"}`); code >= 400 {
		t.Fatal("首次创建应成功")
	}
	code, body := e.create(`{"name":"dup","kind":"command","start_cmd":"sleep 2"}`)
	if code != http.StatusConflict {
		t.Fatalf("重名应 409, got %d %+v", code, body)
	}
}

func TestUpdateService(t *testing.T) {
	e := newSvcEnv(t)
	_, created := e.create(`{"name":"old","kind":"command","start_cmd":"sleep 1"}`)
	id := created["id"].(float64)

	code, body := e.do("PATCH", fmt.Sprintf("/api/services/%d", int(id)),
		`{"name":"new","sort":9}`)
	if code != http.StatusOK {
		t.Fatalf("更新应 200, got %d %+v", code, body)
	}
	rows := e.list()
	if len(rows) != 1 || rows[0]["name"] != "new" || rows[0]["sort"] != float64(9) {
		t.Fatalf("更新没生效: %+v", rows)
	}
}

// 更新不能把 command 改成缺 start_cmd 的非法组合。
func TestUpdateServiceRejectsInvalid(t *testing.T) {
	e := newSvcEnv(t)
	_, created := e.create(`{"name":"x","kind":"command","start_cmd":"sleep 1"}`)
	id := int(created["id"].(float64))
	code, _ := e.do("PATCH", fmt.Sprintf("/api/services/%d", id), `{"kind":"systemd"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("改成 systemd 却不给 unit 应 400, got %d", code)
	}
}

func TestDeleteService(t *testing.T) {
	e := newSvcEnv(t)
	_, created := e.create(`{"name":"del","kind":"command","start_cmd":"sleep 1"}`)
	id := int(created["id"].(float64))
	if code, _ := e.do("DELETE", fmt.Sprintf("/api/services/%d", id), ""); code != http.StatusOK {
		t.Fatalf("删除应 200, got %d", code)
	}
	if got := e.list(); len(got) != 0 {
		t.Fatalf("删除后仍在: %+v", got)
	}
}

// 运行中的服务被删除时必须先停掉，否则进程变孤儿继续吃内存 —— 面板里
// 却再也看不到它，这是最难查的一种泄漏。
func TestDeleteRunningServiceStopsProcess(t *testing.T) {
	e := newSvcEnv(t)
	_, created := e.create(`{"name":"live","kind":"command","start_cmd":"sleep 3000"}`)
	id := int(created["id"].(float64))

	code, st := e.do("POST", fmt.Sprintf("/api/services/%d/toggle", id), "")
	if code != http.StatusOK {
		t.Fatalf("启动应 200, got %d %+v", code, st)
	}
	pid := int(st["pid"].(float64))
	if !pidAlive(pid) {
		t.Fatalf("进程应已起来: %d", pid)
	}
	if code, _ := e.do("DELETE", fmt.Sprintf("/api/services/%d", id), ""); code >= 400 {
		t.Fatalf("删除运行中的服务不该报错")
	}
	deadline := time.Now().Add(8 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(30 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("删除后进程成了看不见的孤儿: %d", pid)
	}
}

// ---------- toggle ----------

func TestToggleStartThenStop(t *testing.T) {
	e := newSvcEnv(t)
	_, created := e.create(`{"name":"t","kind":"command","start_cmd":"sleep 3000"}`)
	id := int(created["id"].(float64))
	path := fmt.Sprintf("/api/services/%d/toggle", id)

	code, started := e.do("POST", path, "")
	if code != http.StatusOK || started["state"] != "running" {
		t.Fatalf("第一次 toggle 应启动: %d %+v", code, started)
	}
	pid := int(started["pid"].(float64))
	if !pidAlive(pid) {
		t.Fatalf("state=running 但进程不在: %d", pid)
	}

	code, stopped := e.do("POST", path, "")
	if code != http.StatusOK || stopped["state"] != "stopped" {
		t.Fatalf("第二次 toggle 应停止: %d %+v", code, stopped)
	}
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(30 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("state=stopped 但进程还活着: %d", pid)
	}
	// D21：用户点停止算正常退出。
	row := e.waitRow(float64(id), "stopped")
	if row["exit_reason"] != "clean" || row["stopped_by"] != "user" {
		t.Fatalf("退出归因不对: %+v", row)
	}
}

func TestToggleUnknownService(t *testing.T) {
	e := newSvcEnv(t)
	code, _ := e.do("POST", "/api/services/999/toggle", "")
	if code != http.StatusNotFound {
		t.Fatalf("不存在的服务应 404, got %d", code)
	}
}

// 服务自己崩了要能如实报出 code，UI 才能显示「异常退出 · code 3」。
func TestToggleReportsAbnormalExit(t *testing.T) {
	e := newSvcEnv(t)
	_, created := e.create(`{"name":"boom","kind":"command","start_cmd":"exit 3"}`)
	id := int(created["id"].(float64))
	if code, _ := e.do("POST", fmt.Sprintf("/api/services/%d/toggle", id), ""); code >= 400 {
		t.Fatalf("启动本身是成功的, 退出是之后的事")
	}
	row := e.waitRow(float64(id), "stopped")
	if row["exit_reason"] != "error" || row["exit_code"] != float64(3) {
		t.Fatalf("应报异常退出 code 3: %+v", row)
	}
}

// ---------- 日志（D19：只在内存里）----------

func TestServiceLogTail(t *testing.T) {
	e := newSvcEnv(t)
	dir := t.TempDir()
	_, created := e.create(`{"name":"log","kind":"command","start_cmd":"echo l1; echo l2; echo l3; sleep 3000","cwd":"` + dir + `"}`)
	id := int(created["id"].(float64))
	e.do("POST", fmt.Sprintf("/api/services/%d/toggle", id), "")

	var (
		lines []any
		body  map[string]any
	)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, body = e.do("GET", fmt.Sprintf("/api/services/%d/log?tail=2", id), "")
		if lines, _ = body["lines"].([]any); len(lines) == 2 {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if len(lines) != 2 || lines[0] != "l2" || lines[1] != "l3" {
		t.Fatalf("tail=2 应只回最后 2 行 l2/l3: %+v", lines)
	}
	// buffer_limit 让前端能区分"日志就这么点"和"前面被环形覆盖掉了"。
	if _, ok := body["buffer_limit"].(float64); !ok {
		t.Fatalf("响应应带 buffer_limit: %+v", body)
	}
}

func TestClearServiceLog(t *testing.T) {
	e := newSvcEnv(t)
	_, created := e.create(`{"name":"clr","kind":"command","start_cmd":"echo a; sleep 3000"}`)
	id := int(created["id"].(float64))
	e.do("POST", fmt.Sprintf("/api/services/%d/toggle", id), "")
	e.waitLog(t, id, 1)

	if code, _ := e.do("DELETE", fmt.Sprintf("/api/services/%d/log", id), ""); code >= 400 {
		t.Fatalf("清空应成功, got %d", code)
	}
	_, body := e.do("GET", fmt.Sprintf("/api/services/%d/log", id), "")
	if got, _ := body["lines"].([]any); len(got) != 0 {
		t.Fatalf("清空后应没有日志: %+v", got)
	}
}

// 服务退出后日志必须还在：崩溃后点开日志是最重要的用途。
func TestLogSurvivesExit(t *testing.T) {
	e := newSvcEnv(t)
	_, created := e.create(`{"name":"dead","kind":"command","start_cmd":"echo 崩之前; exit 1"}`)
	id := int(created["id"].(float64))
	e.do("POST", fmt.Sprintf("/api/services/%d/toggle", id), "")
	e.waitRow(float64(id), "stopped")

	e.waitLog(t, id, 1)
	_, body := e.do("GET", fmt.Sprintf("/api/services/%d/log", id), "")
	lines, _ := body["lines"].([]any)
	if len(lines) == 0 || lines[0] != "崩之前" {
		t.Fatalf("退出后日志丢了: %+v", body)
	}
}

func (e *svcEnv) waitLog(t *testing.T, id, n int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, body := e.do("GET", fmt.Sprintf("/api/services/%d/log", id), "")
		if lines, _ := body["lines"].([]any); len(lines) >= n {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("等 %d 行日志超时", n)
}

func TestUnknownServicePaths(t *testing.T) {
	e := newSvcEnv(t)
	for _, c := range []struct{ method, path string }{
		{"PATCH", "/api/services/999"},
		{"DELETE", "/api/services/999"},
		{"GET", "/api/services/999/log"},
	} {
		code, _ := e.do(c.method, c.path, "")
		if code != http.StatusNotFound {
			t.Errorf("%s %s 应 404, got %d", c.method, c.path, code)
		}
	}
}
