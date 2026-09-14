// Command litepanel 是轻量服务器管理面板的单二进制入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
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
	"litepanel/internal/store"
	"litepanel/internal/webdist"
	"litepanel/internal/ws"
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "配置文件路径")
	listenOverride := flag.String("listen", "", "覆盖配置中的监听地址（调试用）")
	flag.Parse()

	// 面板自身不写任何日志文件（D9）；stderr 交给 systemd/journald。
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("litepanel: ")

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
		fmt.Fprintf(os.Stderr, "litepanel: 一次性初始密码（首次登录后请立即修改）：%s\n", pwd)
	}

	addr := resolveListen(cfg, *listenOverride)
	hub := ws.NewHub()
	deps := api.AuthDeps{
		DB:           db,
		Hub:          hub,
		Sessions:     auth.NewSessionStore(db, time.Now, 7*24*time.Hour),
		Limiter:      auth.NewLoginLimiter(time.Now, 5, 10*time.Minute),
		Clock:        time.Now,
		SecureCookie: cfg.TLS.Enabled,
	}

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
	log.Printf("已启动，监听 %s", addr)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx) // TODO(M4-T5): 此处遍历停止所有托管服务进程组。
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
			fmt.Fprintln(os.Stderr, "litepanel: 未探测到 tailscale 地址，回落绑定 127.0.0.1")
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
	// 发布构建下唯一保留的输出通道：启动失败必须可见（设计 R4）。
	fmt.Fprintf(os.Stderr, "litepanel: "+format+"\n", args...)
	os.Exit(1)
}
