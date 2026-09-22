package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"litepanel/internal/config"
	"litepanel/internal/service"
	"litepanel/internal/store"
	"litepanel/internal/ws"
)

// 服务事件的装配验收：supervisor 的事件出口必须真的接到 hub。
//
// 与 TestHubSubscriptionDrivesCollector 同一个理由 —— 接线最容易"两边都写对、
// 中间忘了接"：service 包有自己的事件测试，ws 包有自己的广播测试，但 main 里
// 漏接的话两边各自全绿，实际表现是服务自己崩了 UI 仍显示"运行中"，
// 用户以为一切正常。
func TestServiceEventReachesWS(t *testing.T) {
	db := openTestDB(t)
	const pwd = "svc-wiring-pass-1"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	hub := ws.NewHub()
	// 用生产装配函数：测试里绝不出 OnEvent/OnLog，否则生产漏接也测不出来。
	sup := wireServices(db, hub)

	deps := buildDeps(db, config.Config{}, hub, false, nil)
	deps.Services = sup

	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c := dialWS(t, wsURL, token)
	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"ch":"services","t":"sub"}`)); err != nil {
		t.Fatal(err)
	}

	svc, err := service.Create(db, service.ServiceInput{
		Name: "wired", Kind: service.KindCommand, StartCmd: "sleep 3000",
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := sup.Start(svc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sup.Shutdown(ctx)
	})

	type frame struct {
		Ch string `json:"ch"`
		T  string `json:"t"`
		D  struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
			PID   int    `json:"pid"`
		} `json:"d"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		var f frame
		if err := c.ReadJSON(&f); err != nil {
			break
		}
		if f.T != "data" || f.Ch != "services" {
			continue
		}
		if f.D.ID == svc.ID && f.D.State == service.StateRunning && f.D.PID == st.PID {
			return // 收到了：说明事件确实从 supervisor 流到了浏览器这一侧
		}
	}
	t.Fatalf("订阅 services 后 3 秒内没收到启动事件（接线漏了？）")
}

// 启动顺序是硬要求：先对账收孤儿，再 autostart。
// 颠倒会让上次遗留的进程和新拉起的同时跑（两个 nginx 抢 80 端口）。
func TestBootServicesReconcilesThenAutostarts(t *testing.T) {
	db := openTestDB(t)
	sup := wireServices(db, ws.NewHub())

	// 上次遗留：库里声称在跑，实际进程早没了。
	stale, err := service.Create(db, service.ServiceInput{
		Name: "stale", Kind: service.KindCommand, StartCmd: "sleep 3000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SaveState(db, stale.ID, service.State{
		State: service.StateRunning, PID: 999998, PGID: 999998, StartedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	auto, err := service.Create(db, service.ServiceInput{
		Name: "auto", Kind: service.KindCommand, StartCmd: "sleep 3000", Autostart: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 没勾自启动的绝不该被拉起来。
	manual, err := service.Create(db, service.ServiceInput{
		Name: "manual", Kind: service.KindCommand, StartCmd: "sleep 3000",
	})
	if err != nil {
		t.Fatal(err)
	}
	// systemd 单元由 systemd 自己管，面板不碰。
	unit, err := service.Create(db, service.ServiceInput{
		Name: "unit", Kind: service.KindSystemd, Unit: "nginx.service", Autostart: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := bootServices(ctx, sup, db); err != nil {
		t.Fatalf("bootServices: %v", err)
	}
	t.Cleanup(func() {
		c, cn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cn()
		_ = sup.Shutdown(c)
	})

	if st, _ := service.GetState(db, auto.ID); st.State != service.StateRunning || st.PID <= 0 {
		t.Fatalf("autostart 服务没被拉起: %+v", st)
	}
	if st, _ := service.GetState(db, manual.ID); st.State != service.StateStopped {
		t.Fatalf("未勾选自启动的服务被拉起了: %+v", st)
	}
	if st, _ := service.GetState(db, unit.ID); st.State != service.StateStopped {
		t.Fatalf("systemd 单元不该被面板拉起: %+v", st)
	}
	if st, _ := service.GetState(db, stale.ID); st.State != service.StateStopped || st.PID != 0 {
		t.Fatalf("遗留状态没被对账清掉: %+v", st)
	}
}

// 自启动失败不该拖垮整个面板：记下来越好，UI 上如实显示 stopped。
func TestBootServicesSurvivesBadCommand(t *testing.T) {
	db := svcWiringDB(t)
	sup := wireServices(db, ws.NewHub())
	if _, err := service.Create(db, service.ServiceInput{
		Name: "broken", Kind: service.KindCommand,
		StartCmd: "/nonexistent/litepanel-binary", Autostart: true,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bootServices(ctx, sup, db); err != nil {
		t.Fatalf("单个服务起不来不该让整个启动流程报错: %v", err)
	}
}

func svcWiringDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "svc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
