package ws

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待条件超时（2s）")
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fmtSeq(xs []int) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x)
	}
	return strings.Join(out, ",")
}

// mustRawClose 关掉底层 TCP 而不发 WS Close 帧，模拟浏览器进程被杀。
func mustRawClose(t *testing.T, c *websocket.Conn) {
	t.Helper()
	// UnderlyingConn 拿到裸 TCP 连接后直接 Close。
	if err := c.UnderlyingConn().Close(); err != nil {
		t.Fatalf("裸关闭失败: %v", err)
	}
}
