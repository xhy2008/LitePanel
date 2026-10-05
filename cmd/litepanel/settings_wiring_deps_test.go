package main

// 装配层的设置接线测试（M7-T5 第 4 步收尾）。
//
// 这里测的是"接上没有"，settings_wiring_test.go 测的是"每一项推对了地方"。
// 分工是有意的：装配漏接一根线时，逐项测试全绿而生产是死的（本项目栽过
// 三次），所以"接上"这件事必须单独钉住。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"litepanel/internal/config"
	"litepanel/internal/metrics"
	"litepanel/internal/service"
	"litepanel/internal/settings"
	"litepanel/internal/store"
	"litepanel/internal/ws"
)

// wireSettings 必须把 Settings 与 SettingsApply 都接进 deps。
//
// 漏接 Settings -> /api/settings 整片 501（用户眼里"设置页坏了"）；漏接
// SettingsApply -> 保存永远只落库、界面上却显示"已保存"（只存不读）。两个
// 都是静默的，所以必须钉。
func TestWireSettingsWiresBothFields(t *testing.T) {
	db := openDBForWiring(t)
	deps := buildDeps(db, config.Config{}, ws.NewHub(), false, nil)
	col := metrics.NewCollector(nil, nil, time.Second)
	sup := service.NewSupervisor(db)

	wireSettings(&deps, db, config.Config{}, col, sup)

	if deps.Settings == nil {
		t.Error("deps.Settings 没接上：/api/settings 会整片 501")
	}
	if deps.SettingsApply == nil {
		t.Error("deps.SettingsApply 没接上：保存只落库，设置页显示已保存而什么都不生效")
	}
}

// 启动时 wireSettings 必须把库里的值推到子系统（异步）。
//
// 这是"重启后设置不会丢效果"的唯一保证：各子系统构造器只看 config，库里的
// 用户值不会被任何人读取 —— 除了这一步。缺它的话用户改过的设置在重启后会
// 悄悄退回 config 的值，而 GET 仍然显示用户那个值。
func TestWireSettingsAppliesOnBoot(t *testing.T) {
	db := openDBForWiring(t)
	cfg := config.Config{TrashDirName: ".trash", TrashRetainDays: 3, JobConcurrency: 2}
	// 先在库里写一个与 config 不同的值（模拟"用户在设置页改过"）。
	seed := settings.NewStore(db, settingsDefaultsFromConfig(cfg), nil)
	if _, err := seed.Apply(context.Background(), raw(t, `{"job_concurrency": 7}`)); err != nil {
		t.Fatalf("预置库值失败: %v", err)
	}

	deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
	col := metrics.NewCollector(nil, nil, time.Second)
	wireSettings(&deps, db, cfg, col, service.NewSupervisor(db))

	files, ok := deps.Files.(interface{ JobDesired() int })
	if !ok {
		t.Fatal("deps.Files 不暴露 JobDesired，测试读不到效果")
	}
	// 异步应用，轮询等它从构造期的 config 值 2 落到库里的 7。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if files.JobDesired() == 7 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("启动时没把库里的 job_concurrency 推下去：仍是 %d（重启会丢设置的典型形态）",
		files.JobDesired())
}

// SettingsApply 触发时要真的推（不只是个非 nil 的空函数）。
func TestWireSettingsApplyActuallyPushes(t *testing.T) {
	db := openDBForWiring(t)
	cfg := config.Config{TrashDirName: ".trash", TrashRetainDays: 3, JobConcurrency: 2}
	deps := buildDeps(db, cfg, ws.NewHub(), false, nil)
	col := metrics.NewCollector(nil, nil, time.Second)
	wireSettings(&deps, db, cfg, col, service.NewSupervisor(db))

	// 改一项再 apply，要立刻从读侧看见。
	st := settings.NewStore(db, settingsDefaultsFromConfig(cfg), nil)
	if _, err := st.Apply(context.Background(), raw(t, `{"metric_interval_sec": 5}`)); err != nil {
		t.Fatal(err)
	}
	if err := deps.SettingsApply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := col.Interval(); got != 5*time.Second {
		t.Errorf("apply 之后采样间隔应是 5s，得 %v", got)
	}
}

// config 里与设置项同名的 toml 字段，settingsDefaultsFromConfig 必须映射到,
// 且返回的是**那个字段自己的值**。
//
// 漂移的方向很隐蔽：漏映射一个键不会报错，只会让它退回内置默认而静默无视
// config 里部署时写的值。断言"返回值等于该字段的哨兵"而不是"返回非空"——
// 后者对"映射错了键"也成立，等于没测。
func TestSettingsConfigMapperCoversAllMatchingFields(t *testing.T) {
	cfg := config.Config{
		TrashDirName:     "sentinel-trash",
		TrashRetainDays:  11,
		JobConcurrency:   13,
		Aria2RPCURL:      "http://127.0.0.1:1/jsonrpc",
		Aria2RPCSecret:   "sentinel-secret-xyz",
		Aria2DownloadDir: "/sentinel-dl",
	}
	// 每个 toml tag 的期望映射值（哨兵）。数字项走 positiveOrEmpty。
	want := map[string]string{
		"trash_dir_name":     cfg.TrashDirName,
		"trash_retain_days":  "11",
		"job_concurrency":    "13",
		"aria2_rpc_url":      cfg.Aria2RPCURL,
		"aria2_rpc_secret":   cfg.Aria2RPCSecret,
		"aria2_download_dir": cfg.Aria2DownloadDir,
	}
	// 用反射确认这些 toml tag 确实都存在于 config.Config（防止字段改名后
	// 本测试的 want 表变成指向空气）。
	tagOf := map[string]bool{}
	rt := reflect.TypeOf(config.Config{})
	for i := 0; i < rt.NumField(); i++ {
		if tag := rt.Field(i).Tag.Get("toml"); tag != "" {
			tagOf[tag] = true
		}
	}
	regKey := map[string]bool{}
	for _, d := range settings.Defs() {
		regKey[string(d.Key)] = true
	}

	m := settingsDefaultsFromConfig(cfg)
	checked := 0
	for tag, exp := range want {
		if !tagOf[tag] {
			t.Errorf("want 表里的 %q 在 config.Config 里没有 correspondent toml 字段", tag)
			continue
		}
		if !regKey[tag] {
			t.Errorf("%q 是 config 字段却不是注册设置项（命名漂了？）", tag)
			continue
		}
		if got := m(settings.Key(tag)); got != exp {
			t.Errorf("config.%s 应兜底成 %q，得 %q", tag, exp, got)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("前置条件：一个 config↔设置项都不想等，这条什么都没测")
	}
}

// positiveOrEmpty：数字型 config 项为 0（没写）时返回空串而不是 "0"。
// "0" 会被当成合法兜底盖掉内置默认，再被子系统夹到最小值 —— 用户没写这项,
// 却因为一个零值被拖到边界。config.Load 总会填默认所以生产走不到，这里挡的
// 是直接构造 config.Config{} 的测试与将来的装配改动。
func TestPositiveOrEmpty(t *testing.T) {
	for _, n := range []int{0, -3} {
		if got := positiveOrEmpty(n); got != "" {
			t.Errorf("%d 应返回空串，得 %q", n, got)
		}
	}
	if got := positiveOrEmpty(7); got != "7" {
		t.Errorf("7 应返回 \"7\"，得 %q", got)
	}
}

// ---- 辅助 ----

func openDBForWiring(t *testing.T) *store.DB {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(base, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// raw 把一段 JSON 字面量转成 Store.Apply 的入参类型。
func raw(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// main() 必须真的调用 wireSettings，而且在 NewRouter 之前。
//
// 为什么用读源码这种看起来笨的办法：上面所有测试都直接调 wireSettings 本身,
// 把 main.go 里那一行删掉它们**全都照样绿** —— 而生产里设置功能整个是死的。
// main 没有测试抓手（它是 main），而恰恰是这一行决定"接线到底存不存在"。
// 同一条思路见仓库里其它"装配层测试不检查字段非 nil、只检查端到端"的注释。
//
// 顺序也检查：NewRouter 已经把 deps 按值拷进各个 handler 闭包，之后再往
// deps 上写 Settings 也不会被任何已建好的 handler 看到 —— 那种 bug 表现为
// "设置接口永远 501"，而且代码看上去完全正确。
func TestMainWiresSettingsBeforeRouter(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go: %v", err)
	}
	s := string(src)
	iWire := strings.Index(s, "wireSettings(")
	iRouter := strings.Index(s, "api.NewRouter(")
	if iWire < 0 {
		t.Fatal("main.go 里没有调用 wireSettings：设置保存将只落库、不生效")
	}
	if iRouter < 0 {
		t.Fatal("找不到 api.NewRouter 调用，这条检查需要更新")
	}
	if iWire > iRouter {
		t.Error("wireSettings 必须在 api.NewRouter 之前：NewRouter 会把 deps 按值" +
			"拷进 handler 闭包，之后补接的 Settings 不会有任何 handler 看到")
	}
}
