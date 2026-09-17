// D6 人工验收辅助工具：订阅 metrics 频道一段时间，
// 用于对照面板进程在「有人看」与「没人看」两种状态下的 CPU 开销。
//
//	go run ./tools/d6probe -url http://127.0.0.1:9530 -pass 密码 -for 60s
//
// 为什么留在仓库里：M2-T4 的人工验收项要量"有人看 / 没人看"两种状态下
// 面板自身的 CPU，而这必须走真实 WS 订阅（HTTP 轮询不会驱动 D6）。
// 后续里程碑的开销基线（设计 20 节）也用它。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	base := flag.String("url", "http://127.0.0.1:9530", "面板地址")
	pass := flag.String("pass", "", "登录密码")
	dur := flag.Duration("for", 20*time.Second, "订阅保持时长")
	ch := flag.String("ch", "metrics", "订阅频道")
	flag.Parse()

	if *pass == "" {
		fmt.Fprintln(os.Stderr, "必须给 -pass")
		os.Exit(2)
	}

	cookie, err := login(*base, *pass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "登录失败:", err)
		os.Exit(1)
	}

	wsURL := strings.Replace(*base, "http://", "ws://", 1) + "/ws"
	head := http.Header{"Cookie": {cookie}}
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, head)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WS 握手失败:", err, resp)
		os.Exit(1)
	}
	defer c.Close()

	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"ch":"`+*ch+`","t":"sub"}`)); err != nil {
		fmt.Fprintln(os.Stderr, "订阅失败:", err)
		os.Exit(1)
	}

	frames := 0
	seqs := []float64{}
	deadline := time.Now().Add(*dur)
	_ = c.SetReadDeadline(deadline)
	fmt.Printf("已订阅 %s，观察 %v\n", *ch, *dur)
	for time.Now().Before(deadline) {
		_, msg, err := c.ReadMessage()
		if err != nil {
			break
		}
		var f struct {
			T   string          `json:"t"`
			Ch  string          `json:"ch"`
			Seq float64         `json:"seq"`
			D   json.RawMessage `json:"d"`
		}
		if json.Unmarshal(msg, &f) != nil {
			continue
		}
		if f.T == "data" && f.Ch == *ch {
			frames++
			seqs = append(seqs, f.Seq)
			if frames == 1 {
				var snap map[string]any
				_ = json.Unmarshal(f.D, &snap)
				cpu := "-"
				if m, ok := snap["cpu"].(map[string]any); ok {
					cpu = fmt.Sprint(m["percent"])
				}
				memv := "-"
				if m, ok := snap["mem"].(map[string]any); ok {
					memv = fmt.Sprint(m["percent"])
				}
				fmt.Printf("首帧 seq=%.0f warming=%v cpu=%v mem=%v%%\n",
					f.Seq, snap["warming"], cpu, memv)
			}
		}
	}
	fmt.Printf("共收到 %d 帧", frames)
	if frames > 1 {
		fmt.Printf("（seq %v→%v）", seqs[0], seqs[len(seqs)-1])
	}
	fmt.Println()
	if frames == 0 {
		os.Exit(1)
	}
}

func login(base, pass string) (string, error) {
	body, _ := json.Marshal(map[string]string{"password": pass})
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = "/api/login"
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Requested-With", "litepanel")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "lp_session" && ck.Value != "" {
			return ck.Name + "=" + ck.Value, nil
		}
	}
	return "", fmt.Errorf("响应里没有 lp_session cookie")
}
