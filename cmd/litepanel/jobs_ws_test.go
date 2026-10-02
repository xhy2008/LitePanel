package main

// fsjobs 频道的端到端接线。
//
// 只调用生产侧装配（buildDeps + newTestServer + 真 WS 连接），测试里绝不
// 出现 filemgr 的内部注入口或 hub.Broadcast 的手工调用。判据是"从 HTTP
// 提交一条删除任务后，订阅了 fsjobs 的连接能收到它的状态帧" —— 这一条
// 同时覆盖三件事：newFileService 把 BroadcastJob 接上了 jobNotifier、
// 接的是 deps.Files 那**一个**实例（另 new 一个就推不到）、频道名是
// fsjobs（写错频道名订阅方一条都收不到）。少任何一件都收不到帧。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/config"
	"litepanel/internal/filemgr"
	"litepanel/internal/ws"

	"github.com/gorilla/websocket"
)

func TestWSFsJobsPushesOnDelete(t *testing.T) {
	db := openTestDB(t)
	hub := ws.NewHub()
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(t.TempDir(), "p.db")
	deps := buildDeps(db, cfg, hub, false, nil)

	// 让删除只作用在一个临时目录里的一个文件上，不碰真实盘。
	dir := t.TempDir()
	target := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := wiredFileService(t, &deps)

	const pwd = "wstest-pass-1234"
	if err := seedPasswordNoChange(db, pwd); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, deps)
	token := loginForToken(t, srv, pwd)

	c := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", token)
	defer c.Close()
	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"ch":"fsjobs","t":"sub"}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return hub.SubscriberCount("fsjobs") == 1 }, "订阅 fsjobs 未生效")

	ctx := context.Background()
	j, err := svc.CreateJob(ctx, filemgr.JobInput{Op: filemgr.OpDelete, Src: []string{target}, Permanent: true})
	if err != nil {
		t.Fatal(err)
	}

	// 读帧直到看到这条任务的一个 fsjobs data 帧。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := c.ReadMessage()
		if err != nil {
			continue // 读超时，继续等下一条
		}
		var f struct {
			Ch string `json:"ch"`
			T  string `json:"t"`
			D  struct {
				ID    int64  `json:"id"`
				State string `json:"state"`
			} `json:"d"`
		}
		if json.Unmarshal(msg, &f) != nil {
			continue
		}
		if f.Ch == "fsjobs" && f.D.ID == j.ID {
			return // 端到端通了
		}
	}
	t.Fatalf("5s 内没从 fsjobs 频道收到任务 %d 的推送（接线缺失或频道名写错）", j.ID)
}

// 装配层必须把 BroadcastJob 接到订阅了 fsjobs 的 hub 上；hub 为 nil 时
// 不接（BroadcastJob(nil) 回一个空函数，不能 panic）。
func TestBroadcastJobNilHubSafe(t *testing.T) {
	api.BroadcastJob(nil)(filemgr.JobProgress{ID: 1})
}
