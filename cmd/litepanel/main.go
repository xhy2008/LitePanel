// Command litepanel 是轻量服务器管理面板的单二进制入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/auth"
	"litepanel/internal/config"
	"litepanel/internal/logx"
	"litepanel/internal/metrics"
	"litepanel/internal/service"
	"litepanel/internal/store"
	"litepanel/internal/terminal"
	"litepanel/internal/termws"
	"litepanel/internal/webdist"
	"litepanel/internal/ws"
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "配置文件路径")
	listenOverride := flag.String("listen", "", "覆盖配置中的监听地址（调试用）")
	debug := flag.Bool("debug", false, "打印逐请求访问日志（排查浏览器白屏/请求挂起用）")
	flag.Parse()

	// 日志接线按构建标签分流（§12.1/D9）：发布构建里下面整段是
	// io.Discard，标准 log 与 logx 都不产生任何输出。
	wireLogging(*debug)
	tuneRuntime()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("配置加载失败: %v", err)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		fatal("数据库打开失败: %v", err)
	}
	defer db.Close()

	passwordSet, err := auth.PasswordIsSet(db)
	if err != nil {
		fatal("读取密码状态失败: %v", err)
	}
	cfg.PasswordSet = passwordSet
	if err := cfg.Validate(); err != nil {
		fatal("配置校验失败: %v", err)
	}

	// 首次启动：生成一次性初始密码，只打到 stderr（不落文件）。
	if !passwordSet {
		pwd := auth.GenerateInitialPassword()
		if err := auth.SetInitialPassword(db, pwd); err != nil {
			fatal("写入初始密码失败: %v", err)
		}
		// R4 例外：这不是运行日志而是面板可用性的最低保障 —— 发布构建
		// 里若不吐这一行，初始密码就永远没人知道（logx 会被编译掉）。
		stderrNote("一次性初始密码（首次登录后请立即修改）：%s", pwd)
	}

	addr := resolveListen(cfg, *listenOverride)
	hub := ws.NewHub()
	// D6：指标采集随 WS 订阅数启停。
	col := wireMetrics(hub, metrics.NewSystemSource(metrics.DefaultProcDir), time.Second)
	defer col.Stop()
	deps := buildDeps(db, cfg, hub, *debug, os.Stderr)
	deps.Metrics = col
	sup := wireServices(db, hub)
	deps.Services = sup
	// 终端：会话 CRUD 走 HTTP（deps.TermSessions），按键/输出走 WS（桥接已
	// 挂在 hub 上）。Bridge 必须在退出时 Close —— 否则面板停止时那些
	// `tmux -CC attach` 子进程会留着（tmux 会话本身要活下去，这是 D5）。
	tw := wireTerminal(db, hub)
	deps.TermSessions = tw.Sessions
	defer tw.Bridge.Close()

	sub, err := webdist.Dist()
	if err != nil {
		fatal("前端产物不可用: %v", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewRouter(sub, deps),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := listenAndServeTLSOrPlain(srv, cfg); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("监听 %s 失败: %v", addr, err)
		}
	}()
	logx.Info("已启动，监听 %s", addr)

	// 顺序是硬要求：先对账收走上次遗留的进程，再 autostart。
	// 颠倒的话上次没退干净的与新拉起的同时在跑（两个 nginx 抢 80）。
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := bootServices(bootCtx, sup, db); err != nil {
		logx.Info("启动托管服务: %v", err)
	}
	// 终端对账：把库刷成 tmux 的样子，并接管上次面板遗留的会话（D5）。
	tw.Reconcile(bootCtx)
	bootCancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	// 关停顺序不能反：先停止接新请求，再把托管服务整组收走。
	// 反过来会让服务在被关停的过程中仍能收到请求并重新拉起进程。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(context.Background())
	if err := sup.Shutdown(ctx); err != nil {
		logx.Info("关停托管服务: %v", err)
	}
}

// wireMetrics 把采集器接到 hub 的订阅计数上并返回采集器（D6）。
//
// 单独成函数是为了让它可测：接线只有一行，但它决定"无人观看时面板是否
// 真的零开销"。测试必须只调用这里、绝不在测试里自己补一句 OnCount ——
// 那样即便生产侧漏接，测试也照样全绿。
//
// 频道名必须是 metrics.ChannelMetrics 而不是字符串字面量：
// 写错频道名的话 hub 的回调永远不会命中，采集器就变成"永远在跑"，
// 而所有单测仍然通过。
func wireMetrics(hub *ws.Hub, src metrics.Source, interval time.Duration) *metrics.Collector {
	col := metrics.NewCollector(src, hub, interval)
	hub.OnCount(metrics.ChannelMetrics, col.SetSubscribers)
	return col
}

// wireServices 建监管器并把事件出口接到 hub（services / svclog:{id} 频道）。
//
// 与 wireMetrics 同理，单独成函数是为了让"到底接没接"可测：漏接的代价是
// 前端只能靠轮询，或服务自己崩了 UI 上还显示运行中 —— 都不会让别的测试变红。
func wireServices(db *store.DB, hub *ws.Hub) *service.Supervisor {
	sup := service.NewSupervisor(db)
	// 事件由 supervisor 直接推 hub：状态变化多数来自 watcher，handler 不在场。
	sup.OnEvent(api.BroadcastServiceEvent(hub))
	sup.OnLog(api.BroadcastServiceLog(hub))
	return sup
}

// terminalHealth 是进程级缓存的 tmux 探测结果。
//
// 只探一次的理由：tmux 不会在面板运行期间自己装上或换版本，而
// /api/term/health 会被终端页反复拉取。这个值没有失效路径，所以不需要
// TTL、不需要锁、不需要刷新入口 —— sync.Once 就是它该有的全部复杂度。
type terminalHealth struct {
	once sync.Once
	h    terminal.Health
	// probe 是把真实 exec 隔开的缝，存在的唯一理由是让"到底探了几次"
	// 可测：不比较次数的缓存断言是假断言（版本本身就不变）。
	// 生产路径不赋它，默认就是 terminal.Probe。
	probe func() terminal.Health
}

func (t *terminalHealth) Health() terminal.Health {
	t.once.Do(func() {
		probe := t.probe
		if probe == nil {
			probe = func() terminal.Health { return terminal.Probe(terminal.DefaultBin) }
		}
		t.h = probe()
	})
	return t.h
}

// wireTerminalHealth 建 tmux 健康探测器（GET /api/term/health 的依赖）。
//
// 与 wireMetrics/wireServices 同理单独成函数，是为了让"到底接没接"可测：
// 漏接的表现是终端页永远 501，用户以为面板没做终端功能，而 terminal 与 api
// 两边各自的测试仍然全绿。
func wireTerminalHealth() *terminalHealth { return &terminalHealth{} }

// terminalWiring 是终端模块的三件套：健康探测、会话 CRUD、WS 桥接。
type terminalWiring struct {
	Health   *terminalHealth
	Sessions *terminal.Service
	Bridge   *termws.Manager
}

// wireTerminal 装配整个终端模块。
//
// 会话 CRUD 走 REST（buildDeps 里赋 deps.TermSessions），按键/输出走 WS
// （桥接直接挂在 hub 上）—— 两条路互不依赖，所以桥接不进 AuthDeps：
// 它是 WS 侧的组件，塞进 HTTP 依赖里只会让人以为有个"终端桥接 API"。
//
// 顺序有讲究：先建 Manager（它要在开始服务前把 hub 的前缀回调登记好），
// 会话服务随后（它不碰 hub）。
func wireTerminal(db *store.DB, hub *ws.Hub) *terminalWiring {
	return &terminalWiring{
		Health:   wireTerminalHealth(),
		Sessions: terminal.NewService(db, terminal.DefaultBin),
		Bridge:   termws.NewManager(termHub{h: hub}, terminal.DefaultBin),
	}
}

// Reconcile 在启动时把库刷成 tmux 的样子，并接管 tmux 里活着的会话。
//
// D5 的核心承诺是"面板重启，终端里的任务不死"：只做库的对账而漏掉桥接，
// 表现是侧栏列出了会话而点进去没反应。两边必须一起对。
// 单个会话接不上不拖垮启动（terminal/termws 内部已逐个容错）。
func (tw *terminalWiring) Reconcile(ctx context.Context) {
	if err := tw.Sessions.Reconcile(ctx); err != nil {
		logx.Info("终端会话对账: %v", err)
	}
	if _, err := tw.Bridge.Reconcile(ctx); err != nil {
		logx.Info("终端桥接对账: %v", err)
	}
}

// bootServices 做启动对账与自启动。
//
// 对账在前：上次面板被 kill -9 留下的进程（若还活着）必须先收走，
// 否则 autostart 会再拉一份，两份抢同一批端口/文件。
// 单个服务起不来不该拖垮整个面板 —— 记一条日志继续，UI 上如实显示 stopped。
func bootServices(ctx context.Context, sup *service.Supervisor, db *store.DB) error {
	if err := sup.Reconcile(ctx); err != nil {
		return err
	}
	list, err := service.List(db)
	if err != nil {
		return err
	}
	for _, svc := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !svc.Autostart || svc.Kind != service.KindCommand {
			continue // systemd 单元交给 systemd 自己管（T8）
		}
		if _, err := sup.Start(svc); err != nil {
			logx.Info("自启动 %s 失败: %v", svc.Name, err)
		}
	}
	return nil
}

// buildDeps 装配路由依赖。抽成函数是为了让安全参数与 -debug 连线可测。
func buildDeps(db *store.DB, cfg config.Config, hub *ws.Hub, debug bool, logw io.Writer) api.AuthDeps {
	return api.AuthDeps{
		DB:       db,
		Hub:      hub,
		Sessions: auth.NewSessionStore(db, time.Now, 7*24*time.Hour),
		// 设计 5.6：同一 IP 连续 5 次失败锁 10 分钟。
		Limiter:      auth.NewLoginLimiter(time.Now, 5, 10*time.Minute),
		Clock:        time.Now,
		SecureCookie: cfg.TLS.Enabled,
		Debug:        debug,
		LogWriter:    logw,
		// Term 在这里接、而不是在 main 里接：它不需要任何外部入参
		// （探测器自己知道去 PATH 找 tmux），放进 buildDeps 让"接没接"
		// 变成装配函数自己的责任，测试也就能只调 buildDeps 来验证。
		// Metrics / Services 需要 source、interval、db 等入参，仍由 main 接。
		Term: wireTerminalHealth(),
	}
}

// resolveListen 实施 D7：默认只绑 tailscale 地址，探测不到则回落 127.0.0.1。
func resolveListen(cfg config.Config, override string) string {
	if override != "" {
		return override
	}
	host := cfg.Listen
	if host == config.ListenAuto {
		if ip := detectIP(); ip != "" {
			host = ip
		} else {
			logx.Info("未探测到 tailscale 地址，回落绑定 127.0.0.1")
			host = "127.0.0.1"
		}
	}
	return net.JoinHostPort(host, fmt.Sprint(cfg.Port))
}

// detectIP 是探测入口，抽成变量以便测试注入。
var detectIP = detectTailscaleIP

func detectTailscaleIP() string {
	out, err := execCommand("tailscale", "ip", "--4")
	if err == nil {
		if ip := firstLine(out); net.ParseIP(ip) != nil {
			return ip
		}
	}
	// 退路：找 tailscale0 网卡的 100.64.0.0/10 地址。
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifa := range ifaces {
		if ifa.Name != "tailscale0" {
			continue
		}
		addrs, err := ifa.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP[0] == 100 {
				return ipn.IP.String()
			}
		}
	}
	return ""
}

func listenAndServeTLSOrPlain(srv *http.Server, cfg config.Config) error {
	if !cfg.TLS.Enabled {
		return srv.ListenAndServe()
	}
	return srv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
}

func defaultConfigPath() string {
	if v := os.Getenv("LITEPANEL_CONFIG"); v != "" {
		return v
	}
	const p = "/etc/litepanel/config.toml"
	if _, err := os.Stat(filepath.Dir(p)); err != nil {
		return "./config.toml"
	}
	return p
}

func fatal(format string, args ...any) {
	// R4 例外：启动失败必须可见。发布构建里标准 log 是 io.Discard，
	// 唯独这里直写 stderr —— systemd 下启动失败若一个字都不吐，
	// systemctl status 是一片空白。stderr 归 journald，面板仍不落文件。
	fmt.Fprintf(os.Stderr, "litepanel: "+format+"\n", args...)
	os.Exit(1)
}
