package download

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 面板与 aria2 之间只有 HTTP POST /jsonrpc 一条通道。这层契约很薄，但写错
// 任何一条在真机上都表现为"下载页永远空白"且原因极难定位：
//   - 密钥的位置：ARIA2 要求 `token:xxx` 作为 **params[0]**，不是 HTTP
//     header。写成 header 时 aria2 仍回 HTTP 200 + JSON-RPC error，浏览器
//     控制台什么都看不出来，只表现为"添加任务失败"。
//   - method 拼写必须逐字符对上（aria2 不报"不认识"，只回 -32601）。
//   - aria2 的业务错误在 error 字段（code<0），result 是空的。
//
// 所以下面每条断言钉的都是"我发出去的 JSON 是不是 aria2 认的那个"，而不是
// 函数返回值 —— 后者用 mock 也能测，前者必须过真 HTTP。

func TestAddUriSendsTokenAsFirstParam(t *testing.T) {
	c, rec := newTestClient(t, func(m string, p []any) any { return "0x1" }, "s3cret")
	gid, err := c.AddURI(context.Background(), []string{"https://example.com/a.iso"}, Options{
		Dir: "/DISK/downloads", Split: 16, Out: "a.iso",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gid != "0x1" {
		t.Errorf("gid 应取 result, got %q", gid)
	}
	req, _ := rec.last()
	if req.JSONRPC != "2.0" {
		t.Errorf(`jsonrpc 必须是 "2.0", got %q`, req.JSONRPC)
	}
	if req.Method != "aria2.addUri" {
		t.Errorf("method 拼写不对: %q", req.Method)
	}
	if len(req.Params) < 3 {
		t.Fatalf("addUri 需要 [token, uris, options], got %d 个 param", len(req.Params))
	}
	if tok, _ := req.Params[0].(string); tok != "token:s3cret" {
		t.Errorf("密钥必须是 params[0] 的 token:xxx, got %#v", req.Params[0])
	}
	uris, ok := req.Params[1].([]any)
	if !ok || len(uris) != 1 || uris[0] != "https://example.com/a.iso" {
		t.Errorf("uris 应是第 1 个 param 的数组, got %#v", req.Params[1])
	}
	opts, ok := req.Params[2].(map[string]any)
	if !ok {
		t.Fatalf("options 应是第 2 个 param 的对象, got %#v", req.Params[2])
	}
	if opts["dir"] != "/DISK/downloads" || opts["split"] != "16" || opts["out"] != "a.iso" {
		t.Errorf("options 不对: %#v", opts)
	}
}

// 数值在 aria2 的 option 表里是**字符串**（它按 shell 参数语义解析）。发
// 数字过去 aria2 会当成非法值，表现同样是"添加成功但 split 没生效"。
func TestOptionNumbersAreSentAsStrings(t *testing.T) {
	c, rec := newTestClient(t, func(string, []any) any { return "0x2" }, "")
	if _, err := c.AddURI(context.Background(), []string{"u"}, Options{Split: 8}); err != nil {
		t.Fatal(err)
	}
	req, _ := rec.last()
	// params[0] 仍是 uris —— 空密钥必须整个不发出，而不是发个 "token:"。
	if first, _ := req.Params[0].(string); strings.HasPrefix(first, "token:") {
		t.Errorf("没配密钥时不该发 token 参数（空 token: 会被 aria2 当授权失败）: %#v", req.Params[0])
	}
	if len(req.Params) != 2 {
		t.Fatalf("无密钥时 addUri 只有 [uris, options], got %d 个: %#v", len(req.Params), req.Params)
	}
	opts, _ := req.Params[1].(map[string]any)
	if opts["split"] != "8" {
		t.Errorf("split 必须是字符串, got %#v", opts["split"])
	}
}

// 只发填了的字段。空 Options 也要带一个 {} 的 options 对象（aria2 允许
// 省略第三个参数，但带上空对象更省事），且绝不能塞 dir:"" 这种空值 ——
// aria2 会把空字符串当有效值，结果是文件落到一个名为 "" 的相对目录里。
func TestEmptyOptionsOmitKeys(t *testing.T) {
	c, rec := newTestClient(t, func(string, []any) any { return "0x3" }, "")
	if _, err := c.AddURI(context.Background(), []string{"u"}, Options{}); err != nil {
		t.Fatal(err)
	}
	req, _ := rec.last()
	opts, _ := req.Params[1].(map[string]any)
	for k, v := range opts {
		if s, _ := v.(string); s == "" {
			t.Errorf("option %s 值为空，aria2 会当真处理: %#v", k, v)
		}
	}
	// 零值同样不该发：split=0 在 aria2 里是"关掉分片下载"，而默认值是 5，
	// 用户没填分片数时把 0 发出去等于偷偷改了 aria2 的默认行为。
	if _, ok := opts["split"]; ok {
		t.Errorf("Split 为 0 时不该发 split 键, got %#v", opts["split"])
	}
}

// aria2 的业务错误要翻成人话。"Resource not found" 这种 aria2 自己的短句
// 保留原文（它比任何转述都准），但必须包上"哪一步失败"的上下文，否则用户
// 在设置页看到一行孤零零的 "No JSON could be parsed" 完全无从下手。
func TestRPCErrorBecomesReadable(t *testing.T) {
	c, _ := newTestClient(t, func(string, []any) any {
		return rpcError{code: 22, msg: "Resource not found."}
	}, "")
	_, err := c.AddURI(context.Background(), []string{"magnet:?xt=x"}, Options{})
	if err == nil {
		t.Fatal("RPC error 必须变成 Go error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Resource not found") {
		t.Errorf("aria2 原文要留着: %s", msg)
	}
	if !strings.Contains(msg, "添加") && !strings.Contains(msg, "addUri") {
		t.Errorf("要说清是哪一步失败: %s", msg)
	}
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("错误要可判类型（handler 据此区分 4xx/5xx）, got %T", err)
	}
	if ae.Code != 22 {
		t.Errorf("code 要透传, got %d", ae.Code)
	}
}

// 传输层失败（connect refused）与 aria2 的业务失败必须能区分：前者意味着
// "aria2 没装/没起"，下载页要显示安装引导；后者是这条 URL 本身有问题。
// 两者混成一个 error 就没法给出正确的下一步提示。
func TestUnreachableIsNotAnRPCError(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close() // 立刻关掉，端口就是"拒绝连接"
	c, err := NewClient(url, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.TellActive(context.Background())
	if err == nil {
		t.Fatal("连不上必须报错")
	}
	var ae *Error
	if errors.As(err, &ae) {
		t.Errorf("连不上不是 aria2 的业务错误: %#v", ae)
	}
	if !IsUnavailable(err) {
		t.Errorf("要能用 IsUnavailable 判出「aria2 不可达」，handler 才能回 503: %v", err)
	}
}

// 实测（aria2 1.37.0）：`"params": null` 被拒（-32602 Invalid params.），
// `"params": []` 与不带该字段都正常。Go 侧 nil slice marshal 出来正是 null，
// 于是所有无参方法（getVersion / getGlobalStat）都会失败 —— 而 getVersion
// 是探活用的一次调用，它失败就等于"aria2 永远不可达"，下载页会显示"没装"
// 而它其实跑得好好的。所以这条断言直接看**发出去的 JSON 里 params 是不是
// null**：编码之后 nil 与空 slice 分不开，必须在这一刻拦住。
func TestNilParamsMarshalsAsEmptyArray(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"version":"1.37.0"}}`))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.GetVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 注意判断串用拼接而不是反引号原文：这里是"字面量里含双引号"，
	// 用反引号会被下面那句 t.Errorf 的格式串搅浑，改用字符串拼接最省事。
	if strings.Contains(seen, "params"+`":null`) {
		t.Errorf("aria2 会拒绝 params 为 null（实测 -32602）, 实际发出: %s", seen)
	}
	if !strings.Contains(seen, "params"+`":[]`) {
		t.Errorf("无参调用应发空数组, 实际发出: %s", seen)
	}
}

// UnavailableClient 的所有方法都必须是"不可达"而不是 panic 或假成功。
//
// 它是装配层在地址非法时塞进来的替代品，存在的唯一意义是"让下载变成 503"。
// 任何一个方法漏了这道拦截（尤其是走 callWithAuth 之外的路径的），后果都是
// 对上层伪装成正常客户端 —— 比报错难查得多。
func TestUnavailableClientFailsEveryMethod(t *testing.T) {
	c := UnavailableClient(errors.New("地址不合法"))
	ctx := context.Background()
	_, err := c.GetVersion(ctx)
	if !IsUnavailable(err) {
		t.Errorf("GetVersion 要报不可达（健康检查靠它才显示未授权/未安装）: %v", err)
	}
	if _, err := c.AddURI(ctx, []string{"https://x/a"}, Options{}); !IsUnavailable(err) {
		t.Errorf("AddURI: %v", err)
	}
	if _, err := c.TellActive(ctx); !IsUnavailable(err) {
		t.Errorf("TellActive: %v", err)
	}
	if _, err := c.TellStatus(ctx, "0x1"); !IsUnavailable(err) {
		t.Errorf("TellStatus: %v", err)
	}
	if err := c.Pause(ctx, "0x1"); !IsUnavailable(err) {
		t.Errorf("Pause: %v", err)
	}
	if _, err := c.GetGlobalStat(ctx); !IsUnavailable(err) {
		t.Errorf("GetGlobalStat: %v", err)
	}
	// 原因要带上：503 的 detail 里只有"连不上"而没有一个真实原因，运维会
	// 去查 aria2 而问题在自己的配置文件里。
	if !strings.Contains(err.Error(), "地址不合法") {
		t.Errorf("原始原因要能透出来: %v", err)
	}
}

// changeGlobalOption 是设置页"下载"分组的热生效通道。
//
// 参数形状必须是 [token, options]：aria2 对 params 的个数与顺序都敏感，
// 而它的报错只有 -32602 "Invalid params"，看不出是哪个位置错了。
func TestChangeGlobalOptionShape(t *testing.T) {
	c, rec := newTestClient(t, func(m string, p []any) any { return "OK" }, "sec")
	err := c.ChangeGlobalOption(context.Background(), map[string]string{
		"max-concurrent-downloads": "7",
		"split":                    "9",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := rec.last()
	if req.Method != "aria2.changeGlobalOption" {
		t.Errorf("method 拼写不对: %q", req.Method)
	}
	if len(req.Params) != 2 {
		t.Fatalf("params 应是 [token, options]，得 %d 个", len(req.Params))
	}
	if tok, _ := req.Params[0].(string); tok != "token:sec" {
		t.Errorf("密钥位置不对: %#v", req.Params[0])
	}
	opts, ok := req.Params[1].(map[string]any)
	if !ok {
		t.Fatalf("options 应是对象，得 %#v", req.Params[1])
	}
	if opts["max-concurrent-downloads"] != "7" || opts["split"] != "9" {
		t.Errorf("选项没原样发出: %#v", opts)
	}
}

// 空 map 不发请求。
// 装配层常常只有部分项要推（用户这次只改了回收站天数），为一次空推送
// 走一趟 RPC 没有意义，而且 aria2 不可达时会凭空冒出一个错误。
func TestChangeGlobalOptionEmptyIsNoop(t *testing.T) {
	var hits int
	c, _ := newTestClient(t, func(m string, p []any) any { hits++; return "OK" }, "sec")
	if err := c.ChangeGlobalOption(context.Background(), map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := c.ChangeGlobalOption(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Errorf("空选项不该发请求，发了 %d 次", hits)
	}
}

// 无密钥时 params 只有 options（不能塞一个空的 "token:"）。
// aria2 在未配 rpc-secret 时会把 params[0] 当 gid 去解析，回一个
// "Invalid GID" 而不是"不需要密钥"。
func TestChangeGlobalOptionWithoutSecret(t *testing.T) {
	c, rec := newTestClient(t, func(m string, p []any) any { return "OK" }, "")
	if err := c.ChangeGlobalOption(context.Background(), map[string]string{"split": "4"}); err != nil {
		t.Fatal(err)
	}
	req, _ := rec.last()
	if len(req.Params) != 1 {
		t.Fatalf("无密钥时 params 应只有 options，得 %d 个: %#v", len(req.Params), req.Params)
	}
}
