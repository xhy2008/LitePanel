package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/terminal"
)

// M5-T1：GET /api/term/health（设计 7.3）。

type stubProber struct {
	h     terminal.Health
	calls int
}

func (s *stubProber) Health() terminal.Health { s.calls++; return s.h }

// newTermEnv 走完整的真实流程：初始密码 → 登录 → 改密 → 重新登录。
//
// 必须真的把"强制改密"状态摘掉：新库的 must_change_password=1 会让本接口被
// 中间件挡成 403，测试意图就全埋在状态处理里了。走真实登录/改密链路而不是
// 直接改 DB 标志位 —— 这条链路一旦变化这里立刻红（测试桩与真实装配漂移是
// M1 冻结事故的成因）。
func newTermEnv(t *testing.T, p api.TermProber) *pwEnv {
	t.Helper()
	const initial = "term-initial-123"
	const newer = "term-changed-456"
	e := newPWEnvWith(t, initial, func(d *api.AuthDeps) { d.Term = p })
	e.login(initial)
	if code, body := e.do("POST", "/api/password",
		`{"old":"`+initial+`","new":"`+newer+`"}`); code != http.StatusOK {
		t.Fatalf("改密失败: %d %+v", code, body)
	}
	// 改密会吊销全部会话，必须重新登录才能继续用这个 client。
	e.login(newer)
	return e
}

func (e *pwEnv) termHealth() (int, map[string]any) {
	e.t.Helper()
	return e.do("GET", "/api/term/health", "")
}

// 未登录必须 401：这个端点只在终端页（登录后）被消费，不像 metrics 那样
// 登录页也要画，所以没有公开的理由。
func TestTermHealthRequiresAuth(t *testing.T) {
	// 环境本身必须是"终端已接线"的：否则 401 可能被误读成 501 兜底的结果。
	// newPWEnvWith 造的服务器默认就是未登录态（cookie jar 里没有会话），
	// 所以不需要额外的登出动作。
	e := newPWEnvWith(t, "term-initial-123", func(d *api.AuthDeps) {
		d.Term = &stubProber{h: terminal.Health{Available: true, Version: "3.7c"}}
	})
	code, _ := e.termHealth()
	if code != 401 {
		t.Fatalf("未登录应 401, got %d", code)
	}
}

func TestTermHealthReportsAvailable(t *testing.T) {
	e := newTermEnv(t, &stubProber{h: terminal.Health{
		Available: true, Version: "3.7c", MinVersion: "3.2", Bin: "/usr/bin/tmux"}})

	code, m := e.termHealth()
	if code != 200 {
		t.Fatalf("应 200, got %d %+v", code, m)
	}
	if m["available"] != true {
		t.Errorf("available 应为 true: %+v", m)
	}
	// 字段名走 snake_case，与 Go struct tag 一致（前端不做大小写转换）
	if m["version"] != "3.7c" || m["min_version"] != "3.2" || m["bin"] != "/usr/bin/tmux" {
		t.Errorf("字段不对: %+v", m)
	}
	if _, ok := m["reason"]; ok {
		t.Errorf("可用时不该带 reason: %+v", m)
	}
}

// tmux 不可用时必须是 **200 + available:false**，不是 4xx/5xx。
//
// 理由：请求本身成功了，"tmux 没装"是被查询对象的**状态**，而设计 7.3 要求
// 终端页针对这个状态渲染Installation引导。回 503 会让前端的 fetch 封装把它
// 当网络/服务异常处理（统一错误体那条路径），引导框就永远出不来。
// 与 metrics 的取舍区别也在这里：采集器报错是意外故障（回 500，避免渲染成
// "各项 0%"），而 tmux 缺失是预期内、有专门文案的状态。
func TestTermHealthReportsUnavailableAs200(t *testing.T) {
	e := newTermEnv(t, &stubProber{h: terminal.Health{
		Available: false, MinVersion: "3.2", Bin: "tmux",
		Reason: "找不到 tmux"}})

	code, m := e.termHealth()
	if code != 200 {
		t.Fatalf("tmux 缺失应回 200 让前端画引导, got %d", code)
	}
	if m["available"] != false {
		t.Errorf("available 应为 false: %+v", m)
	}
	if m["reason"] != "找不到 tmux" {
		t.Errorf("reason 必须原样带给前端: %+v", m)
	}
	if m["min_version"] != "3.2" {
		t.Errorf("引导文案需要版本下限: %+v", m)
	}
}

// 没接线时必须 501，不能回 200 + available:false。
// 后者会把"面板忘了接这个模块"渲染成"你的服务器没装 tmux"——
// 一个指向错误方向的引导比没有引导更糟。与 Services/Metrics 同一取舍。
func TestTermHealthIs501WhenUnwired(t *testing.T) {
	e := newTermEnv(t, nil)
	code, _ := e.termHealth()
	if code != 501 {
		t.Fatalf("未接线应 501, got %d", code)
	}
}

// 探测结果由装配层缓存，处理器每次请求只读一次缓存 —— 不在请求路径上
// 反复 exec 子进程。这条断言钉住"处理器不自己决定探测次数"。
func TestTermHealthReadsProbeOncePerRequest(t *testing.T) {
	p := &stubProber{h: terminal.Health{Available: true, Version: "3.6"}}
	e := newTermEnv(t, p)
	p.calls = 0
	e.termHealth()
	if p.calls != 1 {
		t.Fatalf("一次请求应恰好读一次探测结果, got %d", p.calls)
	}
}

// 响应体必须是合法 JSON 且能反解回同构结构（防止字段漏标 json tag
// 导致某些字段以 Go 风格的名字漏出去）。
func TestTermHealthJSONRoundTrip(t *testing.T) {
	e := newTermEnv(t, &stubProber{h: terminal.Health{Available: true, Version: "3.6"}})
	code, m := e.termHealth()
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back terminal.Health
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("响应体应能反解回 terminal.Health: %v (%s)", err, raw)
	}
	if back.Version != "3.6" || !back.Available {
		t.Fatalf("往返后数据不一致: %+v", back)
	}
	for k := range m {
		for _, r := range k {
			if r >= 'A' && r <= 'Z' {
				t.Fatalf("响应字段名含大写（前端约定 snake_case）: %q", k)
			}
		}
	}
}
