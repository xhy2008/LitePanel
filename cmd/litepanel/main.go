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
	"syscall"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/auth"
	"litepanel/internal/config"
	"litepanel/internal/logx"
	"litepanel/internal/metrics"
	"litepanel/internal/store"
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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx) // TODO(M4-T5): 此处遍历停止所有托管服务进程组。
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
