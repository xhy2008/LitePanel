//go:build ignore

// 端到端冒烟：对着**正在运行的面板**跑一遍浏览器会做的事。
// 登录 → 建会话 → WS 订阅 → 发按键 → 看输出 → 第二台设备补历史。
// 与 Go 测试的区别：这里连的是编译出来的二进制，走真实的
// 配置加载 / 路由装配 / 静态资源 / tmux 桥接全链路。
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// base 可用 PANEL_URL 覆盖：写死端口的话，改了配置就静默打到
// 另一个（也许是旧的）服务上，于是一路"通过"其实测的不是这个二进制。
var base = envOr("PANEL_URL", "http://127.0.0.1:18080")

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func must(err error, what string) {
	if err != nil {
		fmt.Printf("FAIL %s: %v\n", what, err)
		os.Exit(1)
	}
}

// 强制改密之后的固定密码：面板首次登录后必须改密（后端的 403 闸门），
// 所以冒烟脚本第一轮把一次性密码换成这个，之后各轮直接用它登录。
const fixedPass = "Smoke-Test-2026!"

// changePassword 换掉初始密码。改密会吊销全部会话（设计 M7-T5：密码换了
// 别人的旧 token 不能再有效），所以换完必须重新登录拿新 cookie。
func changePassword(c *http.Client, old, neu string) {
	body, _ := json.Marshal(map[string]string{"old": old, "new": neu})
	req, _ := http.NewRequest("POST", base+"/api/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "litepanel")
	res, err := c.Do(req)
	must(err, "改密请求")
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		must(fmt.Errorf("改密 %d %s", res.StatusCode, b), "改密")
	}
}

func mustChange(c *http.Client) bool {
	code, b := get(c, "/api/me")
	must(err404(code, "/api/me"), "GET /api/me")
	var v struct {
		Must bool `json:"must_change_password"`
	}
	must(json.Unmarshal([]byte(b), &v), "解析 /api/me")
	return v.Must
}

func err404(code int, path string) error {
	if code/100 != 2 {
		return fmt.Errorf("%s -> %d", path, code)
	}
	return nil
}

func login(pass string) *http.Client {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	body, _ := json.Marshal(map[string]string{"password": pass})
	req, _ := http.NewRequest("POST", base+"/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "litepanel")
	res, err := c.Do(req)
	must(err, "登录请求")
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		fmt.Printf("FAIL 登录 %d %s\n", res.StatusCode, b)
		fmt.Println("（debug 构建的一次性密码在面板启动日志里）")
		os.Exit(1)
	}
	// 把 cookie 显式带上：httptest 之外我们只关心这一个值
	var ck string
	for _, c := range res.Cookies() {
		ck = c.Value
	}
	u, _ := url.Parse(base)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: "lp_session", Value: ck, Path: "/"}})
	return c
}

func cookieOf(c *http.Client) string {
	u, _ := url.Parse(base)
	for _, k := range c.Jar.Cookies(u) {
		if k.Name == "lp_session" {
			return k.Value
		}
	}
	return ""
}

func get(c *http.Client, path string) (int, string) {
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("X-Requested-With", "litepanel")
	res, err := c.Do(req)
	must(err, "GET "+path)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func post(c *http.Client, path, body string) (int, string) {
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "litepanel")
	res, err := c.Do(req)
	must(err, "POST "+path)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func dial(token string) *websocket.Conn {
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws"
	d := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	// 认证沿用浏览器行为：WS 握手带同名 cookie（同源自动带上）。
	c, _, err := d.Dial(wsURL, http.Header{
		"Cookie": {"lp_session=" + token},
	})
	if err != nil {
		// 握手失败时把状态码打出来：401 = cookie 没带上/会话无效，
		// 400 = 路径或协议不对，两者的排查方向完全不同。
		// gorilla 把细节藏在私有类型里，能拿到的只有状态码。
		var he interface{ ErrorCode() int }
		if errors.As(err, &he) {
			fmt.Printf("FAIL WS 握手 HTTP %d（401=cookie 没带上/会话无效，400=路径或协议不对）\n", he.ErrorCode())
		}
		must(err, "WS 握手")
	}
	return c
}

func sub(c *websocket.Conn, ch string) {
	must(c.WriteMessage(websocket.TextMessage,
		[]byte(`{"t":"sub","ch":"`+ch+`"}`)), "sub")
}

// 浏览器格式的上行帧：[1B 频道名长度][频道名]['k'+按键]
func keys(c *websocket.Conn, ch, text string) {
	frame := append([]byte{byte(len(ch))}, []byte(ch)...)
	frame = append(frame, 'k')
	frame = append(frame, []byte(text)...)
	must(c.WriteMessage(websocket.BinaryMessage, frame), "按键")
}

// 读到出现 want 为止。出错立即返回：连接已死时继续读会让 gorilla
// 在累计 1000 次失败后主动 panic。
func readUntil(c *websocket.Conn, want string, budget time.Duration) (string, error) {
	deadline := time.Now().Add(budget)
	var all bytes.Buffer
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(budget))
		mt, data, err := c.ReadMessage()
		if err != nil {
			return all.String(), err
		}
		if mt != websocket.BinaryMessage {
			continue // 文本帧：sub_ok / 心跳
		}
		n := int(data[0])
		all.Write(data[1+n:])
		if strings.Contains(all.String(), want) {
			return all.String(), nil
		}
	}
	return all.String(), fmt.Errorf("超时未等到 %q", want)
}

func main() {
	pass := os.Args[1]
	c := login(pass)
	if mustChange(c) {
		changePassword(c, pass, fixedPass)
		c = login(fixedPass)
		fmt.Println("OK 改掉初始密码并重新登录")
	}
	token := cookieOf(c)
	if token == "" {
		fmt.Println("FAIL 没拿到会话 cookie")
		os.Exit(1)
	}
	fmt.Println("OK 登录")

	if code, b := get(c, "/api/term/health"); code != 200 {
		fmt.Printf("FAIL 健康 %d %s\n", code, b)
		os.Exit(1)
	}
	fmt.Println("OK 终端健康")

	code, b := post(c, "/api/term/sessions", `{"title":"冒烟"}`)
	if code != 201 {
		fmt.Printf("FAIL 建会话 %d %s\n", code, b)
		os.Exit(1)
	}
	var created struct {
		Session struct {
			ID       int64  `json:"id"`
			TmuxName string `json:"tmux_name"`
		} `json:"session"`
	}
	must(json.Unmarshal([]byte(b), &created), "解析建会话响应")
	ch := fmt.Sprintf("term:%d", created.Session.ID)
	fmt.Printf("OK 建会话 id=%d tmux=%s\n", created.Session.ID, created.Session.TmuxName)

	// 设备 A：订阅 → 打命令
	a := dial(token)
	defer a.Close()
	sub(a, ch)
	time.Sleep(300 * time.Millisecond)
	marker := fmt.Sprintf("SMOKE-%d", time.Now().UnixNano())
	// 先赋值再打印：直接 echo marker 会数到两次（shell 会回显键入的命令本身）
	keys(a, ch, fmt.Sprintf("m=%s; printf 'X%%sY\\n' \"$m\"\r", marker))
	out, err := readUntil(a, "X"+marker+"Y", 15*time.Second)
	if err != nil {
		fmt.Printf("FAIL 设备 A 没看到输出: %v\n----\n%s\n", err, out)
		os.Exit(1)
	}
	fmt.Println("OK 设备 A 打字 → tmux → 输出")

	// 中文与颜色
	keys(a, ch, "printf '中文测试\\n'; ls --color=always -l / | head -3\r")
	if out, err := readUntil(a, "中文测试", 10*time.Second); err != nil {
		fmt.Printf("FAIL 中文: %v\n%s\n", err, out)
		os.Exit(1)
	}
	fmt.Println("OK 中文输出")

	// 设备 B：中途接入，必须看到 A 之前的历史行（重放）
	bb := dial(token)
	defer bb.Close()
	sub(bb, ch)
	if out, err := readUntil(bb, "X"+marker+"Y", 15*time.Second); err != nil {
		fmt.Printf("FAIL 设备 B 没补到历史: %v\n----\n%s\n", err, out)
		os.Exit(1)
	}
	fmt.Println("OK 设备 B 中途接入补到历史")

	// B 接入之后 A 的新输出，两边都要继续收到
	marker2 := fmt.Sprintf("LIVE-%d", time.Now().UnixNano())
	keys(a, ch, fmt.Sprintf("printf 'A%%sB\\n' '%s'\r", marker2))
	for _, tc := range []struct {
		name string
		conn *websocket.Conn
	}{{"A", a}, {"B", bb}} {
		if _, err := readUntil(tc.conn, "A"+marker2+"B", 15*time.Second); err != nil {
			fmt.Printf("FAIL 设备 %s 没收到后续输出: %v\n", tc.name, err)
			os.Exit(1)
		}
	}
	fmt.Println("OK 两个设备同时收到后续输出")

	// 删除：确认参数是服务端强制的
	if code, _ := get(c, "/api/term/sessions"); code != 200 {
		fmt.Println("FAIL 列表")
		os.Exit(1)
	}
	fmt.Println("OK 会话列表")
	fmt.Println("全部通过")
}
