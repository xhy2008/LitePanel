// Command litepanel 是轻量服务器管理面板的单二进制入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/webdist"
)

// 发布构建（-tags release）下日志不写入任何文件；
// 启动失败与 panic 仍输出一行到 stderr（设计 R4）。
var logOut io.Writer = io.Discard

func main() {
	var addr = flag.String("listen", "127.0.0.1:9530", "监听地址")
	flag.Parse()

	sub, err := webdist.Dist()
	if err != nil {
		fmt.Fprintf(os.Stderr, "litepanel: 前端产物不可用: %v\n", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.NewRouter(sub),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "litepanel: 监听 %s 失败: %v\n", *addr, err)
			os.Exit(1)
		}
	}()
	log.SetOutput(logOut)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx) // TODO(M4-T5): 此处遍历停止所有托管服务进程组。
}
