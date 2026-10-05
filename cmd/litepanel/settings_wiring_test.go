package main

// 设置"保存即生效"的穷举证明（M7-T5 最重要的一个测试文件）。
//
// 这个文件的核心不是逐个键的行为测试，而是 TestEverySettingIsWired：它遍历
// 注册表，要求每一项要么被 applier 认识（且有对应的逐项测试观察到真实行为
// 変化），要么明确标了 RestartRequired。新增设置项而忘了接线，这里会红。
//
// 为什么非要穷举："领域层全绿、装配层少接一根线、生产是死的"这个模式本项目
// 已经栽过三次（jobs/download/metrics 的接线都事后补过测试）。对设置页来说
// 漏接的表现格外恶劣：界面显示"已保存"而值一个字节都没生效，而且看起来完全
// 正常。设计方案管这叫"只存不读"，是明令禁止的失效模式。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litepanel/internal/auth"
	"litepanel/internal/download"
	"litepanel/internal/filemgr"
	"litepanel/internal/metrics"
	"litepanel/internal/service"
	"litepanel/internal/settings"
	"litepanel/internal/store"
	"litepanel/internal/terminal"
)

type wiringEnv struct {
	targets settingsTargets
	store   *settings.Store
	// aria2Reqs 收集假 aria2 收到的 changeGlobalOption 请求体（JSON-RPC params）。
	aria2Reqs []map[string]any
	srv       *httptest.Server
}

func newWiringEnv(t *testing.T) *wiringEnv {
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

	env := &wiringEnv{}
	// 假 aria2：只认 JSON-RPC 的形状，任何方法都回 OK，并把请求记下来。
	// 用真 HTTP + 真 Client 而不是接口替身：装配层最该证明的是"推下去的
	// 那串 JSON aria2 认识"，替身会把这层契约整个跳过。
	env.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if m, _ := req["method"].(string); strings.HasPrefix(m, "aria2.changeGlobalOption") {
			env.aria2Reqs = append(env.aria2Reqs, req)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"OK"}`))
	}))
	t.Cleanup(env.srv.Close)

	client, err := download.NewClient(env.srv.URL, "sec")
	if err != nil {
		t.Fatal(err)
	}
	dl := download.NewService(client, download.NewTaskStore(db, nil), download.ServiceOptions{})

	files := filemgr.NewService(filemgr.Options{
		DB:         db,
		UploadRoot: filepath.Join(base, "up"),
		// 盘根注入成这个临时目录：migrateTrashDirs 只动"盘根/旧名"真实存在
		// 的盘，临时目录里什么都不放，改名路径就是纯策略更新，不碰真实挂载。
		TrashRoots: func(context.Context) ([]string, error) { return []string{base}, nil },
	})

	// configDefault 注入：与生产 buildDeps 一致（回收站目录名、保留天数、
	// 并发这些键的内置默认来自 config，不是 settings 常量）。漏接它会让
	// trash_dir_name 解析成空串、SetTrashPolicy 每项都报错 —— 测试正是在
	// 这里第一次暴露了"生产还没接 configDefault"这个真问题。
	cfgDefault := func(k settings.Key) string {
		switch k {
		case settings.TrashDirName:
			return ".trash"
		case settings.TrashRetainDays:
			return "3"
		case settings.JobConcurrencyFile:
			return "2"
		}
		return ""
	}
	env.store = settings.NewStore(db, cfgDefault, nil)
	env.targets = settingsTargets{
		Collector:  metrics.NewCollector(nil, nil, time.Second),
		Sessions:   auth.NewSessionStore(db, nil, 30*24*time.Hour),
		Limiter:    auth.NewLoginLimiter(nil, 5, 10*time.Minute),
		Files:      files,
		Supervisor: service.NewSupervisor(db),
		Download:   dl,
	}
	return env
}

// set 直接把值写进设置库（走 Store.Apply，与真实 PUT 同一条校验路径）。
func (e *wiringEnv) set(t *testing.T, kv map[string]string) {
	t.Helper()
	in := map[string]json.RawMessage{}
	for k, v := range kv {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		in[k] = raw
	}
	if _, err := e.store.Apply(context.Background(), in); err != nil {
		t.Fatalf("写入设置 %v: %v", kv, err)
	}
}

func (e *wiringEnv) apply(t *testing.T) error {
	t.Helper()
	return applySettings(context.Background(), e.store, e.targets)
}

// ---- 逐个键：改完之后必须能观察到真实行为变化 ----

// hotCase 描述一个热生效键的一次完整验证：写入什么、从哪读回、期望什么。
//
// observe 是强制项而不是可选：一个键如果"setter 被调用"之外什么都观察不到,
// 逐项测试就退化成"断言没报错"，而空实现的 setter 也能满足它。
type hotCase struct {
	key     settings.Key
	set     string
	observe func(*wiringEnv) (got any, ok bool)
	want    any
}

func hotCases() []hotCase {
	return []hotCase{
		{settings.MetricIntervalSec, "5",
			func(e *wiringEnv) (any, bool) { return e.targets.Collector.Interval(), true },
			5 * time.Second},
		{settings.SessionTTLDays, "2",
			func(e *wiringEnv) (any, bool) { return e.targets.Sessions.TTL(), true },
			48 * time.Hour},
		{settings.LoginMaxFails, "9",
			func(e *wiringEnv) (any, bool) { f, _ := e.targets.Limiter.Policy(); return f, true },
			9},
		{settings.LoginWindowMin, "30",
			func(e *wiringEnv) (any, bool) { _, w := e.targets.Limiter.Policy(); return w, true },
			30 * time.Minute},
		{settings.TrashDirName, ".bin",
			func(e *wiringEnv) (any, bool) { n, _ := e.targets.Files.TrashPolicy(); return n, true },
			".bin"},
		{settings.TrashRetainDays, "7",
			func(e *wiringEnv) (any, bool) { _, d := e.targets.Files.TrashPolicy(); return d, true },
			7 * 24 * time.Hour},
		{settings.JobConcurrencyFile, "5",
			func(e *wiringEnv) (any, bool) { return e.targets.Files.JobDesired(), true },
			5},
		{settings.ServiceLogLines, "123",
			func(e *wiringEnv) (any, bool) { return e.targets.Supervisor.LogLimit(), true },
			123},
		{settings.StopGraceSec, "45",
			func(e *wiringEnv) (any, bool) { return e.targets.Supervisor.Grace(), true },
			45 * time.Second},
		{settings.Aria2Split, "12",
			func(e *wiringEnv) (any, bool) { _, sp := e.targets.Download.Defaults(); return sp, true },
			12},
		{settings.Aria2DownloadDir, "/data/dl",
			func(e *wiringEnv) (any, bool) { d, _ := e.targets.Download.Defaults(); return d, true },
			"/data/dl"},
	}
}

// 每个热生效键：写进去、applier 推一遍、从读侧看见变化。
func TestWiringHotKeys(t *testing.T) {
	for _, tc := range hotCases() {
		tc := tc
		t.Run(string(tc.key), func(t *testing.T) {
			e := newWiringEnv(t)
			e.set(t, map[string]string{string(tc.key): tc.set})
			if err := e.apply(t); err != nil {
				t.Fatalf("applySettings: %v", err)
			}
			got, ok := tc.observe(e)
			if !ok {
				t.Fatal("读侧不可用")
			}
			if got != tc.want {
				t.Errorf("写入 %s=%s 后应观察到 %v，得 %v", tc.key, tc.set, tc.want, got)
			}
		})
	}
}

// 历史档位的默认值是包级状态（tmux 在建会话时才定死），单独一条：
// 验证完必须还原，否则同一个二进制里后面的测试会看到 100000。
func TestWiringTermHistory(t *testing.T) {
	t.Cleanup(func() { terminal.SetDefaultHistoryLimit(terminal.DefaultHistoryLimit) })
	e := newWiringEnv(t)
	e.set(t, map[string]string{"term_history_limit": "100000"})
	if err := e.apply(t); err != nil {
		t.Fatal(err)
	}
	if got := terminal.DefaultHistoryLimitOf(); got != 100000 {
		t.Errorf("历史默认档应是 100000，得 %d", got)
	}
}

// 下载目录默认与并发上限：aria2 侧必须有真实动作，不能只改面板内存。
func TestWiringDownloadAria2Side(t *testing.T) {
	e := newWiringEnv(t)
	e.set(t, map[string]string{"aria2_download_dir": "/data/dl", "aria2_max_concurrent": "8"})
	if err := e.apply(t); err != nil {
		t.Fatal(err)
	}
	if dir, _ := e.targets.Download.Defaults(); dir != "/data/dl" {
		t.Errorf("面板默认目录应是 /data/dl，得 %q", dir)
	}
	// aria2 侧：必须真的发过 changeGlobalOption 且带上新的并发数。
	if len(e.aria2Reqs) == 0 {
		t.Fatal("applier 没有向 aria2 发 changeGlobalOption：并发上限只改了在面板内存里，" +
			"而排队是 aria2 做的 —— 用户会看到'设成 8 而同时在下的还是 5 个'")
	}
	params, _ := e.aria2Reqs[0]["params"].([]any)
	var opts map[string]any
	for _, p := range params {
		if m, ok := p.(map[string]any); ok && m["max-concurrent-downloads"] != nil {
			opts = m
		}
	}
	if opts == nil {
		t.Fatalf("changeGlobalOption 没带 max-concurrent-downloads: %v", e.aria2Reqs[0])
	}
	if opts["max-concurrent-downloads"] != "8" {
		t.Errorf("并发数应是 \"8\"（aria2 要字符串），得 %v", opts["max-concurrent-downloads"])
	}
}

// ---- 穷举证明 ----

// 每一个注册项都必须有归宿：被 applier 处理（且有 hotCase 观察），或明确
// 标了 RestartRequired。新增一项而两件事都没做，这里红。
//
// 它与 hotCases() 形成双向闭环：hotCases 里每一项必须存在于注册表（下面
// 反向检查），注册表里每个热生效项必须出现在 handled 集合与 hotCases 里。
// 新增键时三处都要动才能全绿 —— 这正是"忘了接线会被发现"的具体机制。
func TestEverySettingIsWired(t *testing.T) {
	// applier 会推到的键（与 settings_wiring.go 函数体一一对应）。
	handled := map[settings.Key]bool{
		settings.MetricIntervalSec:  true,
		settings.SessionTTLDays:     true,
		settings.LoginMaxFails:      true,
		settings.LoginWindowMin:     true,
		settings.TrashDirName:       true,
		settings.TrashRetainDays:    true,
		settings.JobConcurrencyFile: true,
		settings.ServiceLogLines:    true,
		settings.StopGraceSec:       true,
		settings.TermHistoryLimit:   true,
		settings.Aria2DownloadDir:   true,
		settings.Aria2MaxConcurrent: true,
		settings.Aria2Split:         true,
	}
	observed := map[settings.Key]bool{}
	for _, c := range hotCases() {
		observed[c.key] = true
	}
	observed[settings.TermHistoryLimit] = true   // 单独一条测试（包级状态要还原）
	observed[settings.Aria2MaxConcurrent] = true // aria2 侧，观察在 TestWiringDownloadAria2Side

	for _, d := range settings.Defs() {
		if !handled[d.Key] && !d.RestartRequired {
			t.Errorf("%s（%s）既没被 applier 处理也没标 RestartRequired："+
				"它现在是'保存了但永远不生效'的设置项", d.Key, d.Label)
		}
		// 热生效项必须有观察测试（RestartRequired 的项没有行为可观察，跳过）。
		if handled[d.Key] && !observed[d.Key] && !d.RestartRequired {
			t.Errorf("%s 被 applier 处理但没有任何测试观察到它的行为变化"+
				"（'setter 被调用'与'值进了生效路径'是两件事）", d.Key)
		}
	}
	// 反向：handled 里出现注册表没有的键（改名/删项后忘了同步）。
	for k := range handled {
		found := false
		for _, d := range settings.Defs() {
			if d.Key == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("applier 处理了注册表里不存在的键 %s", k)
		}
	}
	for _, c := range hotCases() {
		if !handled[c.key] {
			t.Errorf("hotCases 里的 %s 不在 handled 集合（applier 并不处理它）", c.key)
		}
	}
	// RestartRequired 的项必须真的在 applier 里"没被悄悄处理" —— 出现
	// "既标了重启又偷偷热推"是最难排查的状态：界面说重启，实际半生效。
	for _, d := range settings.Defs() {
		if d.RestartRequired && handled[d.Key] {
			t.Errorf("%s 标了 RestartRequired 却在 applier 的 handled 里，二者只能选一", d.Key)
		}
	}
}

// applier 不吞错：子系统拒绝时必须把错误交给调用方（界面据此显示
// "已保存但未能立即生效"）。回收站目录名给一个注册表校验器都拦不住的历史
// 坏值路径不好构造，这里直接测 applySettings 的失败传播：Files 收到一个
// 空盘根列表 + 非法目录名（库里允许存在的旧值），SetTrashPolicy 报错。
func TestWiringPropagatesFailure(t *testing.T) {
	base := t.TempDir()
	db, err := store.Open(filepath.Join(base, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 直接写库绕开 Apply 的校验：模拟"旧版本存下的非法值"。
	if _, err := db.SqlDB().Exec(
		`INSERT INTO settings(key,value,updated_at) VALUES('trash_dir_name','bad/name',0)`,
	); err != nil {
		t.Fatal(err)
	}
	files := filemgr.NewService(filemgr.Options{
		DB:         db,
		UploadRoot: filepath.Join(base, "up"),
	})
	targets := settingsTargets{Files: files}
	err = applySettings(context.Background(), settings.NewStore(db, nil, nil), targets)
	if err == nil {
		t.Fatal("非法回收站名必须让 applier 报错，静默吞掉等于告诉界面'生效了'")
	}
	if !strings.Contains(err.Error(), "回收站") {
		t.Errorf("错误要指名是哪一项，得 %v", err)
	}
	_ = os.RemoveAll
}

// aria2 没装/没起时，applier **不**把这次保存判为失败。
//
// 下载页有一个专门的健康通道在报"aria2 不可达"（/dl/health）。设置页再报
// 一次，会让一个根本没装 aria2 的用户每次保存都染红"未能立即生效"，而他
// 改的采样间隔其实生效了 —— 那条红字把他支到完全错误的方向。
//
// 与之相对：aria2 可达却拒绝了 changeGlobalOption（地址对、方法被拒）必须
// 如实上报，那是"设了却没用上"，用户需要知道。两条分开测在下面。
func TestWiringAria2DownIsNotApplyFailure(t *testing.T) {
	e := newWiringEnv(t)
	e.srv.Close() // 让下载服务的端点失效
	client, err := download.NewClient(e.srv.URL, "sec")
	if err != nil {
		t.Fatal(err)
	}
	e.targets.Download = download.NewService(client,
		download.NewTaskStore(nil, nil), download.ServiceOptions{})
	e.set(t, map[string]string{"aria2_max_concurrent": "8", "job_concurrency": "6"})
	if err := e.apply(t); err != nil {
		t.Fatalf("aria2 不可达不该让整次保存失败（会误导没装 aria2 的用户）: %v", err)
	}
	// 其余项不受牵连照常推到。
	if got := e.targets.Files.JobDesired(); got != 6 {
		t.Errorf("其余项应照常生效，job_concurrency 应是 6，得 %d", got)
	}
}

// aria2 可达但拒绝该全局选项时，applier 必须报错并带上原因。
func TestWiringAria2RejectsReports(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 一个"活着但会对 changeGlobalOption 返错"的假 aria2。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "changeGlobalOption") {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"option rejected"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"OK"}`))
	}))
	defer srv.Close()
	client, err := download.NewClient(srv.URL, "sec")
	if err != nil {
		t.Fatal(err)
	}
	dl := download.NewService(client, download.NewTaskStore(db, nil), download.ServiceOptions{})
	st := settings.NewStore(db, nil, nil)
	if _, err := st.Apply(context.Background(), map[string]json.RawMessage{
		"aria2_max_concurrent": json.RawMessage(`"8"`),
	}); err != nil {
		t.Fatal(err)
	}
	err = applySettings(context.Background(), st, settingsTargets{Download: dl})
	if err == nil {
		t.Fatal("aria2 可达却拒绝了选项时必须报错，静默等于告诉界面'生效了'")
	}
	if !strings.Contains(err.Error(), "aria2") {
		t.Errorf("错误要指名是 aria2 这一项，得 %v", err)
	}
}
