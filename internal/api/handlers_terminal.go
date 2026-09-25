package api

import (
	"net/http"

	"litepanel/internal/terminal"
)

// TermProber 是 HTTP 层需要的全部终端健康能力。
//
// 只有 Health() 一个方法，和 MetricsSource 只暴露 OnDemand() 是同一个理由：
// "tmux 到底能不能用"这件事的判定（版本下限、输出格式、保守方向）属于
// internal/terminal，HTTP 层不参与判断，只负责把它翻译成 JSON。
//
// 实现者应当**缓存**探测结果：`tmux -V` 虽然瞬时，但每次请求 fork 一个子进程
// 仍然没必要（tmux 不会在面板运行期间自己装上或升级）。有测试钉住"一次请求
// 只读一次"。
type TermProber interface {
	Health() terminal.Health
}

// handleTermHealth 返回 tmux 可用性（设计 7.3）。
//
// 注意状态码的取舍：tmux **不可用也回 200**。"没装 tmux"是面板运行环境的
// 一种正常状态，不是本次请求的失败；前端要据 available=false 渲染安装引导，
// 而 4xx/5xx 会走统一错误体那条路径，引导框就永远出不来。
// 对照 handleMetricsSnapshot 回 500 的理由：采集器报错是**意外**故障，
// 用 200 空壳会渲染成"各项 0%"的假数据；而 tmux 缺失有专门的 UI 承接。
func handleTermHealth(p TermProber) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, p.Health())
	}
}
