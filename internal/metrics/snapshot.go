package metrics

// ChannelMetrics 是指标频道名。hub 的订阅计数用它驱动采集器启停（D6）。
const ChannelMetrics = "metrics"

// Snapshot 是一次采集的完整快照，直接作为 WS data 帧的 d 与
// GET /api/metrics/snapshot 的响应体。字段命名走 snake_case 与
// 设计 15 节的帧格式一致。
//
// 指针字段表示"这一项本轮没取到"，前端渲染成 --；
// 不用 0 表示未知，因为 0 会被读成"完全空闲"。
type Snapshot struct {
	Seq     uint64 `json:"seq"`
	TS      int64  `json:"ts"` // Unix 秒
	Warming bool   `json:"warming"`

	CPU   *CPUStat   `json:"cpu"`
	Mem   *MemStat   `json:"mem"`
	Disks []DiskStat `json:"disks,omitempty"`

	// GPU/显存留给 M3。留指针而非删字段：前端可以现在就固定四个环的布局，
	// M3 落地时不需要改协议。
	GPU  *GPUStat `json:"gpu"`
	VRAM *GPUStat `json:"vram"`
}

type CPUStat struct {
	Percent    *float64  `json:"percent"` // 差分不可用时为 null
	Cores      []float64 `json:"cores,omitempty"`
	Load1      float64   `json:"load1"`
	Load5      float64   `json:"load5"`
	Load15     float64   `json:"load15"`
	CoresTotal int       `json:"cores_total"`
}

type MemStat struct {
	Total     uint64  `json:"total"`
	Available uint64  `json:"available"`
	Used      uint64  `json:"used"`
	Percent   float64 `json:"percent"`

	SwapTotal uint64  `json:"swap_total"`
	SwapUsed  uint64  `json:"swap_used"`
	SwapPct   float64 `json:"swap_percent"`
}

type DiskStat struct {
	Mountpoint string  `json:"mountpoint"`
	Device     string  `json:"device"`
	FSType     string  `json:"fstype"`
	Total      uint64  `json:"total"`
	Used       uint64  `json:"used"`
	Free       uint64  `json:"free"`
	Percent    float64 `json:"percent"`
}

// GPUStat 是利用率或显存占用（两者形状相同，语义不同故分开两个字段）。
// Available=false 时仪表显示灰色不可用态（设计 5.4 的 NVML 降级路径）。
type GPUStat struct {
	Available bool     `json:"available"`
	Percent   *float64 `json:"percent"`
	Used      uint64   `json:"used,omitempty"`
	Total     uint64   `json:"total,omitempty"`
	Name      string   `json:"name,omitempty"`
	Reason    string   `json:"reason,omitempty"` // 不可用原因，人类可读
}
