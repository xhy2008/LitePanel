package main

// 测试主机：用真实的 Supervisor 拉起一个服务，然后把两个 pid 写盘。
// 它必须走生产代码路径，否则这个测试只能证明测试自己没写错。

import (
	"fmt"
	"os"

	"litepanel/internal/service"
	"litepanel/internal/store"
)

func main() {
	dir := os.Args[1]
	db, err := store.Open(dir + "/db.sqlite")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	svc, err := service.Create(db, service.ServiceInput{
		Name: "victim", Kind: service.KindCommand, StartCmd: "sleep 3000",
	})
	if err != nil {
		panic(err)
	}
	sup := service.NewSupervisor(db)
	st, err := sup.Start(svc)
	if err != nil {
		panic(err)
	}

	must(os.WriteFile(dir+"/self", []byte(fmt.Sprint(os.Getpid())), 0o600))
	must(os.WriteFile(dir+"/child", []byte(fmt.Sprint(st.PID)), 0o600))

	// 什么都不做，等着被 kill -9。
	select {}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
