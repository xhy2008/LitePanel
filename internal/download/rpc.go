// Package download 是 aria2 的对接层（设计 §9 / D16）。
//
// 分工：aria2c 由 systemd 常驻，面板只做两件事 —— 用 HTTP JSON-RPC 下命令
// （本文件），以及用 WebSocket 收事件（events.go）。**面板自己不得罪轮询
// 进度**：aria2 的 onDownloadProgress 默认每秒推，面板把它转成前端帧即可；
// 一旦改成轮询，面板就变成"每秒问一次所有任务"的额外负载源，而这正是 D16
// 选 systemd + RPC 而不是内置下载器的理由。
package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Error 是 aria2 **业务**错误的包装。
//
// 为什么非要一个可判类型的错误：handler 必须把"aria2 没装/没起"（503 +
// 安装引导）和"这条 URL aria2 不收"（400 + 原文）分开发。这两种情况在
// HTTP 层面完全一样（aria2 都回 200 + JSON-RPC error），只有到这一层才
// 分得开，所以分类也必须在这一层做完。
type Error struct {
	Code    int    // aria2 的错误码（负数是 JSON-RPC 协议错误，正数是 aria2 自己的）
	Message string // aria2 原文，保留：它通常比任何转述都准
	Method  string // 哪个方法失败的
}

func (e *Error) Error() string {
	if e.Method == "" {
		return fmt.Sprintf("aria2: %s (code %d)", e.Message, e.Code)
	}
	return fmt.Sprintf("aria2 %s 失败: %s (code %d)", e.Method, e.Message, e.Code)
}

// unavailableError 标记"根本没连上"，与 aria2 的业务拒绝区分开。
type unavailableError struct{ err error }

func (e *unavailableError) Error() string { return "aria2 不可达: " + e.err.Error() }
func (e *unavailableError) Unwrap() error { return e.err }

// IsUnavailable 判断错误是否"aria2 没装/没起/端口不对"。
// handler 据此回 503，前端显示安装引导而不是空列表或一句"失败"。
func IsUnavailable(err error) bool {
	var ue *unavailableError
	return errors.As(err, &ue)
}

// RPCRequest 是发出去的 JSON-RPC 2.0 请求。导出仅为了测试能直接断言它的
// 形状（协议正确性只有看到最终 JSON 才测得到）。
type RPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
	ID      int    `json:"id"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Options 对应 aria2.addUri 的第三个参数。
//
// 全部是字符串而不是数字：aria2 的 option 表按 shell 参数语义解析，发数字
// 过去它当非法值处理 —— 而失败方式是"添加成功但选项没生效"，用户只会觉得
// "多线程是假的"，比直接报错更难查。
type Options struct {
	Dir string // 保存目录
	Out string // 文件名
	// Split 是分片数。**这里收 int，发出去时转成字符串** —— 转换必须发生
	// 在这一层而不是让每个调用方 strconv.Itoa：一旦漏掉，症状是"多线程
	// 是假的"（aria2 静默忽略非法值），比直接报错难查一个数量级。
	Split int
}

// m 转成 aria2 的 options 对象。**空值必须整个键都不发**：aria2 会把空
// 字符串当有效值，dir:"" 的后果是文件落到一个名为 "" 的相对目录。
func (o Options) m() map[string]string {
	m := map[string]string{}
	if o.Dir != "" {
		m["dir"] = o.Dir
	}
	if o.Out != "" {
		m["out"] = o.Out
	}
	if o.Split > 0 {
		m["split"] = strconv.Itoa(o.Split)
	}
	return m
}

// Client 是 aria2 JSON-RPC 客户端。可并发使用（底层 http.Client 本身安全）。
type Client struct {
	url    string
	secret string
	http   *http.Client
}

// NewClient 连接 aria2 的 RPC 端点。rpcURL 形如 http://127.0.0.1:6800/jsonrpc。
//
// 超时是**必须**有的：aria2 卡住时（磁盘满、正在做 DNS）没有超时的调用会
// 一直挂着，而它挂在 HTTP handler 里 —— 后果是下载页一直转圈且登录限流器
// 那边数不上失败，直到把 server 的连接耗尽。
func NewClient(rpcURL, secret string) (*Client, error) {
	if rpcURL == "" {
		return nil, errors.New("aria2 RPC 地址未配置")
	}
	if _, err := url.Parse(rpcURL); err != nil {
		return nil, fmt.Errorf("aria2 RPC 地址不合法: %w", err)
	}
	return &Client{
		url:    rpcURL,
		secret: secret,
		// 不用默认 transport 的 0 超时语义：单独一个 10s 的 Client，
		// 与面板其他出站请求隔离。
		http: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Close 释放空闲连接。面板生命期内一般不调，留给测试与热重载。
func (c *Client) Close() { c.http.CloseIdleConnections() }

// call 发一次 JSON-RPC。result 为 nil 时丢弃返回值。
func (c *Client) call(ctx context.Context, method string, params []any, result any) error {
	// params 为 nil 时 Go 会 marshal 成 JSON null，而 aria2 对 "params": null
	// 回 -32602 Invalid params（实测 1.37.0：null 被拒，[] 或不带该字段都正常）。
	// 不规范化 nil 的后果是"无参方法全都调不通"，包括 getVersion —— 那会让
	// 探活永远报不可达，下载页显示"aria2 没装"，而它其实跑得好好的。
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(RPCRequest{JSONRPC: "2.0", Method: method, Params: params, ID: 1})
	if err != nil {
		return fmt.Errorf("编码 aria2 请求: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造 aria2 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// 传输层失败包成 unavailable：调用方要能把它和"aria2 拒绝了这个
		// 请求"区分开（见 IsUnavailable）。ctx 主动取消不算不可达 —— 那是
		// 用户关了页面，不是 aria2 出了问题。
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &unavailableError{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return &unavailableError{err: fmt.Errorf("读取 aria2 响应: %w", err)}
	}
	// 非 200 通常是端口连上了别的服务（比如把 RPC 地址写成面板自己的 8080，
	// 会拿到一个 HTML）。不特判的话下面那个 JSON 错误会显得莫名其妙。
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("aria2 RPC 返回 HTTP %d（这个端口上跑的是 aria2 吗？）", resp.StatusCode)
	}
	var rr rpcResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return fmt.Errorf("aria2 响应不是 JSON（检查 RPC 地址与协议）: %w", err)
	}
	if rr.Error != nil {
		return &Error{Code: rr.Error.Code, Message: rr.Error.Message, Method: method}
	}
	if result != nil && len(rr.Result) > 0 {
		if err := json.Unmarshal(rr.Result, result); err != nil {
			return fmt.Errorf("解析 %s 结果: %w", method, err)
		}
	}
	return nil
}

// callWithAuth 把密钥放到 params[0]（ARIA2 的约定），没配密钥就一个参数
// 都不加 —— 发个空的 "token:" 过去 aria2 会按资源篮处理。
func (c *Client) callWithAuth(ctx context.Context, method string, params []any, result any) error {
	p := params
	if c.secret != "" {
		p = append([]any{"token:" + c.secret}, params...)
	}
	return c.call(ctx, method, p, result)
}

// AddURI 添加下载任务，返回 gid。uris 多个时 aria2 视作同一文件的镜像。
func (c *Client) AddURI(ctx context.Context, uris []string, o Options) (string, error) {
	if len(uris) == 0 {
		return "", errors.New("下载地址不能为空")
	}
	list := make([]any, 0, len(uris))
	for _, u := range uris {
		if strings.TrimSpace(u) == "" {
			continue
		}
		list = append(list, u)
	}
	if len(list) == 0 {
		return "", errors.New("下载地址不能为空")
	}
	var gid string
	if err := c.callWithAuth(ctx, "aria2.addUri", []any{list, o.m()}, &gid); err != nil {
		return "", err
	}
	return gid, nil
}

// Status 是 aria2.tellStatus 的返回（字段名照 aria2 的 key）。
type Status struct {
	GID             string   `json:"gid"`
	Status          string   `json:"status"`
	TotalLength     string   `json:"totalLength"`
	CompletedLength string   `json:"completedLength"`
	DownloadSpeed   string   `json:"downloadSpeed"`
	Connections     string   `json:"connections"`
	ErrorCode       string   `json:"errorCode"`
	ErrorMessage    string   `json:"errorMessage"`
	FollowedBy      []string `json:"followedBy"`
	BelongsTo       string   `json:"belongsTo"`
}

func (c *Client) tell(ctx context.Context, method string, result *[]Status, extra ...any) error {
	return c.callWithAuth(ctx, method, extra, result)
}

// TellStatus 查单个任务。
func (c *Client) TellStatus(ctx context.Context, gid string) (*Status, error) {
	// 字段名逐个点名：aria2 默认只回一小部分，速度/连接数/错误码要显式要。
	var one Status
	if err := c.callWithAuth(ctx, "aria2.tellStatus", []any{gid, []any{
		"status", "totalLength", "completedLength", "downloadSpeed",
		"connections", "errorCode", "errorMessage", "belongsTo", "followedBy",
	}}, &one); err != nil {
		return nil, err
	}
	return &one, nil
}

// TellActive 当前下载中的任务。
func (c *Client) TellActive(ctx context.Context) ([]Status, error) {
	var out []Status
	err := c.tell(ctx, "aria2.tellActive", &out, []any{statusFields()})
	return out, err
}

// TellWaiting 排队等待的任务。
func (c *Client) TellWaiting(ctx context.Context) ([]Status, error) {
	var out []Status
	err := c.tell(ctx, "aria2.tellWaiting", &out, 0, 1000, []any{statusFields()})
	return out, err
}

// TellStopped 已完成/出错/被移除的历史（aria2 只保留有限条，见 tasks.go）。
func (c *Client) TellStopped(ctx context.Context) ([]Status, error) {
	var out []Status
	err := c.tell(ctx, "aria2.tellStopped", &out, 0, 1000, []any{statusFields()})
	return out, err
}

func statusFields() []any {
	return []any{"gid", "status", "totalLength", "completedLength",
		"downloadSpeed", "connections", "errorCode", "errorMessage", "belongsTo", "followedBy"}
}

// Pause 暂停；aria2 对已暂停的任务再暂停会报错，调用方按需忽略。
func (c *Client) Pause(ctx context.Context, gid string) error {
	return c.callWithAuth(ctx, "aria2.pause", []any{gid}, nil)
}

// PauseDownload 暂停某个会话组（种子/金属链接派生的子任务一起停）。
func (c *Client) PauseDownload(ctx context.Context, gid string) error {
	return c.callWithAuth(ctx, "aria2.pauseDownload", []any{gid}, nil)
}

// Resume 继续。aria2 的 unpause 对不存在的 gid 报"不存在"，对已下载的
// 任务报"已经完成" —— 都算失败，由 handler 翻成用户能懂的话。
func (c *Client) Resume(ctx context.Context, gid string) error {
	return c.callWithAuth(ctx, "aria2.unpause", []any{gid}, nil)
}

// Remove 移除任务（保留已下载的文件）。
func (c *Client) Remove(ctx context.Context, gid string) error {
	return c.callWithAuth(ctx, "aria2.remove", []any{gid}, nil)
}

// ForceRemove 强删：正在写的文件会留下 .aria2 控制文件，这是 aria2 的行为，
// 面板不额外清理（清理会误删用户正在别的任务里复用的文件）。
func (c *Client) ForceRemove(ctx context.Context, gid string) error {
	return c.callWithAuth(ctx, "aria2.forceRemove", []any{gid}, nil)
}

// RemoveDownloadResult 清掉一条已完成/已失败的历史记录。
func (c *Client) RemoveDownloadResult(ctx context.Context, gid string) error {
	return c.callWithAuth(ctx, "aria2.removeDownloadResult", []any{gid}, nil)
}

// GlobalStat 是 getGlobalStat 的返回。
type GlobalStat struct {
	DownloadSpeed     string `json:"downloadSpeed"`
	UploadSpeed       string `json:"uploadSpeed"`
	NumActive         string `json:"numActive"`
	NumWaiting        string `json:"numWaiting"`
	NumStopped        string `json:"numStopped"`
	NumWaitingSeeders string `json:"numWaitingSeeders"`
}

// GetGlobalStat 全局汇总（下载页顶部那条）。
func (c *Client) GetGlobalStat(ctx context.Context) (*GlobalStat, error) {
	var g GlobalStat
	if err := c.callWithAuth(ctx, "aria2.getGlobalStat", []any{}, &g); err != nil {
		return nil, err
	}
	return &g, nil
}
