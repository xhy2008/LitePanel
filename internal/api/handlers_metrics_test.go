package api_test

import (
	"errors"
	"net/http"
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/metrics"
)

// stubMetrics 是可控的指标来源。只实现 OnDemand —— 节流、"有实时快照就别再采"
// 这些行为属于 Collector，由 metrics 包自己的测试负责；在 HTTP 层重复断言
// 只会得到一套与真实实现无关的测试。
type stubMetrics struct {
	snap    metrics.Snapshot
	err     error
	samples int
}

func (s *stubMetrics) OnDemand() (*metrics.Snapshot, error) {
	s.samples++
	if s.err != nil {
		return nil, s.err
	}
	cp := s.snap
	return &cp, nil
}

// newMetricsEnv 走完整真实流程：初始密码 → 登录 → 改密 → 重新登录。
//
// 必须真的把「强制改密」状态摘掉：新库的 must_change_password=1 会让
// 本接口被中间件挡成 403，那样每个用例都得各自处理改密，测试意图就被埋掉了。
// 走真实的登录/改密/再登录，而不是直接写 DB 标志位 —— 这条链路一旦变化
// 这里立刻红。测试桩与真实装配漂移正是 M1 冻结事故的成因。
func newMetricsEnv(t *testing.T, src api.MetricsSource) *pwEnv {
	t.Helper()
	const initial = "initial-pass-123"
	const newer = "changed-pass-456"
	e := newPWEnvWith(t, initial, func(d *api.AuthDeps) { d.Metrics = src })
	e.login(initial)
	if code, body := e.do("POST", "/api/password",
		`{"old":"`+initial+`","new":"`+newer+`"}`); code != http.StatusOK {
		t.Fatalf("改密失败: %d %+v", code, body)
	}
	// 改密会吊销全部会话（设计 M7-T5），必须重新登录才能继续用这个 client。
	e.login(newer)
	if code, body := e.do("GET", "/api/me", ""); code != http.StatusOK ||
		body["must_change_password"] != false {
		t.Fatalf("前置状态不对: %d %+v", code, body)
	}
	return e
}

func snapshot(t *testing.T, e *pwEnv) (int, map[string]any) {
	t.Helper()
	return e.do("GET", "/api/metrics/snapshot", "")
}

func memSrc() *stubMetrics {
	return &stubMetrics{snap: metrics.Snapshot{
		Mem: &metrics.MemStat{Total: 1000, Used: 500, Percent: 50},
	}}
}

// 首屏时 WS 还没连上、采集器按 D6 是停着的。此时接口必须给出真实数据，
// 否则打开面板先看到一片 --，只能干等 WS 连上后的第一帧。
func TestSnapshotOnDemandWhenCollectorIdle(t *testing.T) {
	src := memSrc()
	e := newMetricsEnv(t, src)

	code, body := snapshot(t, e)
	if code != http.StatusOK {
		t.Fatalf("code=%d %+v", code, body)
	}
	if src.samples != 1 {
		t.Errorf("应调用 1 次, got %d", src.samples)
	}
	mem, ok := body["mem"].(map[string]any)
	if !ok {
		t.Fatalf("缺 mem 段: %+v", body)
	}
	if mem["total"] != float64(1000) {
		t.Errorf("mem.total = %v", mem["total"])
	}
}

// 每个请求都必须真的走一次 OnDemand：HTTP 层不得自己缓存。
// 在这里加一层缓存就会与 Collector 的节流状态漂移，
// 变成"两处缓存谁更新"这种没人说得清的读数。
func TestSnapshotDelegatesEveryRequest(t *testing.T) {
	src := memSrc()
	e := newMetricsEnv(t, src)

	for i := 0; i < 3; i++ {
		if code, _ := snapshot(t, e); code != http.StatusOK {
			t.Fatalf("第%d次 code=%d", i, code)
		}
	}
	if src.samples != 3 {
		t.Errorf("应委托 3 次, got %d", src.samples)
	}
}

// 未登录绝不能读到系统指标 —— 磁盘容量、挂载点、核数都是有用的侦察信息。
func TestSnapshotRequiresAuth(t *testing.T) {
	e := newPWEnv(t, "initial-pass-123")
	code, _ := e.do("GET", "/api/metrics/snapshot", "")
	if code != http.StatusUnauthorized {
		t.Errorf("未登录应 401, got %d", code)
	}
}

// 未注入采集器时必须是明确的 501，不能是空 200。
// 空 200 会让前端把一片 0% 的仪表画出来，看起来像"这台机器很闲"。
func TestSnapshotNotImplementedWithoutSource(t *testing.T) {
	e := newMetricsEnv(t, nil)
	code, _ := snapshot(t, e)
	if code != http.StatusNotImplemented {
		t.Errorf("应 501, got %d", code)
	}
}

// 采样失败必须 500，不能静默返回 200 + 空壳：空壳会被前端渲染成"各项 0%"，
// 看起来像机器空闲 —— 一个明确的错误远好过一个看起来正常的假数据。
func TestSnapshotSampleErrorIs500(t *testing.T) {
	src := memSrc()
	src.err = errors.New("/proc 读不到")
	e := newMetricsEnv(t, src)

	code, body := snapshot(t, e)
	if code != http.StatusInternalServerError {
		t.Errorf("应 500, got %d %+v", code, body)
	}
}

// 来源给出的字段必须原样出现在响应里。
// 这里刻意用结构体直出（不是手拼 map），保证 HTTP 响应与 WS 帧的 d
// 是同一类型同一套 tag —— 前端只需一套解析，也不会出现"WS 有 http 没有"
// 这种字段漂移。
func TestSnapshotPassesThroughSourceFields(t *testing.T) {
	src := memSrc()
	src.snap.TS = 1700000000
	src.snap.Seq = 12
	src.snap.Disks = []metrics.DiskStat{{Mountpoint: "/DISK", Device: "/dev/sdb1", Percent: 88}}
	e := newMetricsEnv(t, src)

	code, body := snapshot(t, e)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if ts, _ := body["ts"].(float64); int64(ts) != 1700000000 {
		t.Errorf("ts 未原样透传: %v", body["ts"])
	}
	if seq, _ := body["seq"].(float64); int64(seq) != 12 {
		t.Errorf("seq 未透传: %v", body["seq"])
	}
	disks, ok := body["disks"].([]any)
	if !ok || len(disks) != 1 {
		t.Fatalf("disks 未透传: %+v", body["disks"])
	}
	d := disks[0].(map[string]any)
	if d["mountpoint"] != "/DISK" || d["device"] != "/dev/sdb1" {
		t.Errorf("磁盘字段错: %+v", d)
	}
}

// warming 必须透传：CPU 没有差分基线时前端要显示 --。
// 丢掉这个标记，前端就会把 percent=null 渲染成 0%（"系统空闲"）。
func TestSnapshotPreservesWarming(t *testing.T) {
	src := &stubMetrics{snap: metrics.Snapshot{
		Mem:     &metrics.MemStat{Total: 1},
		CPU:     &metrics.CPUStat{}, // percent 为 null
		Warming: true,
	}}
	e := newMetricsEnv(t, src)

	_, body := snapshot(t, e)
	if body["warming"] != true {
		t.Errorf("warming 应为 true, got %v", body["warming"])
	}
	cpu, ok := body["cpu"].(map[string]any)
	if !ok {
		t.Fatalf("缺 cpu 段: %+v", body)
	}
	v, present := cpu["percent"]
	if !present || v != nil {
		t.Errorf("无基线时 cpu.percent 必须是 null, got %v (present=%v)", v, present)
	}
}
