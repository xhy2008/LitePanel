package download

import (
	"context"
	"errors"
	"time"
)

// Health 是 aria2 的可达性快照。下载页靠它决定显示什么：
// 可达 → 任务列表；不可达 → 安装引导（命令、启用服务的按钮）。
//
// 这个类型存在的唯一理由是**降级要能判定**。aria2 是可选依赖（D16 里它
// 由 systemd 独立托管），面板绝不能因为它没起来就整个下载页报错甚至让
// 请求挂住 —— 但也不能显示一个空列表假装"没有任务"，那等于告诉用户
// "下载功能坏了且不知道原因"。
type Health struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version,omitempty"`
	Message   string `json:"message,omitempty"` // 不可达时给用户看的一句话
	CheckedAt int64  `json:"checked_at"`
}

// prober 让 HealthChecker 能注入假实现（测试用），也让未来接别的下载后端
// 时不必改 handler。
type prober interface {
	GetVersion(ctx context.Context) (string, error)
}

// GetVersion 用 aria2.getVersion 做探活：它最轻、无副作用、且只有"真的是
// aria2 在听"才会成功 —— 拿 getGlobalStat 探活的话，端口上恰好跑了别的
// JSON-RPC 服务也会被判成健康。
func (c *Client) GetVersion(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.callWithAuth(ctx, "aria2.getVersion", []any{}, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// HealthChecker 带缓存地探活。
//
// 缓存不是为了省 CPU 而是为了**不放大故障**：下载页每 2 秒刷新一次，如果
// 每次探活都真去连一个没起来的 aria2（TCP 握手要等到超时），面板就会持续
// 出站连一个死端口，并把每个请求都拖慢一个超时时间。缓存 TTL 内的失败直接
// 回缓存结果，恢复探测最快 TTL 一次。
type HealthChecker struct {
	p   prober
	ttl time.Duration

	cached *Health
	at     time.Time
}

// NewHealthChecker 建探活器。ttl<=0 时取默认 30s —— 太短会退化成轮询死端口，
// 太长则 aria2 起来后用户要等很久才看到列表回来；30s 与"重启 aria2 服务"
// 这个动作的实际耗时同量级。
func NewHealthChecker(p prober, ttl time.Duration) *HealthChecker {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &HealthChecker{p: p, ttl: ttl}
}

// Check 返回当前健康状态。
//
// 探活本身失败**不返回 error**：调用方是 HTTP handler，而"aria2 没起来"是
// 一个正常业务状态（200 + ok=false），不是服务器错误。把它做成 error 会
// 诱导 handler 回 5xx，前端于是只显示"请求失败"，安装引导永远出不来。
func (h *HealthChecker) Check(ctx context.Context) *Health {
	if h.cached != nil && time.Since(h.at) < h.ttl {
		return h.cached
	}
	hh := &Health{CheckedAt: time.Now().Unix()}
	ver, err := h.p.GetVersion(ctx)
	switch {
	case err == nil:
		hh.OK = true
		hh.Version = ver
	case IsUnavailable(err):
		// 没连上：最常见的原因是 aria2 没装或没起，给的是**下一步动作**。
		// 文案里带上 aria2 与"未安装/未启动"这些词：前端直接显示这句话，
		// 而绝大多数用户的实际情况就是没装。只回 "connection refused" 等于
		// 让用户自己去猜该装什么包。
		hh.Message = "aria2 不可达：可能未安装 aria2、服务未启动，或 RPC 地址/端口配置不对"
	default:
		// 连上了但 RPC 报错：地址可能是别的 JSON-RPC 服务，或密钥不对。
		// 保留原文 —— "Unauthorized" 与 "connection refused" 的修法完全不同。
		hh.Message = err.Error()
	}
	// 成功的结果与失败的结果都要缓存（理由见上面的故障放大）；但 ctx 取消
	// 不缓存 —— 那是调用方走的，不代表 aria2 的状态，缓存它会把"用户关了
	// 页面"错记成"aria2 坏了"整整一个 TTL。
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		h.cached, h.at = hh, time.Now()
	}
	return hh
}

// Forget 让"设置页刚改了 aria2 地址"这类操作能立刻重探。
func (h *HealthChecker) Forget() { h.cached, h.at = nil, time.Time{} }
