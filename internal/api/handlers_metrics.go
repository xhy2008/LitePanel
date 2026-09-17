package api

import (
	"net/http"

	"litepanel/internal/metrics"
)

// MetricsSource 是 HTTP 层需要的全部指标能力。
//
// 只暴露 OnDemand 一个方法是刻意的：节流、"有实时快照就别再采"这些
// 判断都依赖采集器的内部状态（interval、当前快照、订阅数），在 HTTP 层
// 重做一遍必然与它漂移。依赖面越小，漂移面越小。
// Collector 天然满足这个接口。
type MetricsSource interface {
	OnDemand() (*metrics.Snapshot, error)
}

// handleMetricsSnapshot 返回当前指标快照（首屏用，之后走 WS）。
//
// 出错时必须回 500，绝不能回 200 + 空壳：空壳会被前端渲染成"各项 0%"，
// 看起来像机器空闲 —— 一个明确的错误远好过一个看起来正常的假数据。
func handleMetricsSnapshot(src MetricsSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap, err := src.OnDemand()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "metrics_unavailable",
				"读取系统指标失败: "+err.Error())
			return
		}
		// 响应体与 WS 帧的 d 字段结构完全一致（同一个类型、同一套 tag），
		// 前端只需一套解析逻辑。
		writeJSON(w, http.StatusOK, snap)
	}
}
