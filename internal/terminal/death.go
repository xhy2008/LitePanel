package terminal

// 死因判定（用户 2026-09 裁决的执行基础）。
//
// 为什么必须靠"尸检"而不是事件流：实测（dev/deathprobe，tmux 3.7c）
// 三种死法 —— shell 自己 exit、外力 kill-session、整个 kill-server ——
// control 连接上看到的事件完全相同（%sessions-changed → %exit，
// 参数为空），事后 list-sessions 也只说"名字不在"。tmux 只在
// remain-on-exit on 的会话上留下尸体：pane_dead=1 + pane_dead_status
// （退出码）。所以"正常退出"的唯一可观测证据就是这具尸体，CreateSession
// 必须替每个会话把这个选项打开（见 session.go）。
//
// 判定表（写死在这里，别处不复述）：
//   pane_dead=1, status=0   正常退出      → 上层杀尸体 + 删行，不留痕迹
//   pane_dead=1, status!=0  异常退出      → 保留、显示遗言、手动删
//   名字凭空消失（server 在）异常/被外力杀 → 保留
//   server 连不上            未知          → 什么都不动

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// PaneStatus 是一个会话的存活与死因。
type PaneStatus struct {
	// Dead：所有 pane 都已退出（尸体）。有任何一个 pane 活着就不算死。
	Dead bool
	// ExitStatus：尸体里**最大**的退出码。取最大值是有方向的取舍：
	// 一个窗口 exit 0 不许抹掉另一个窗口的异常 3 —— 误判成"正常"会把
	// 要人看的现场自动删掉，误判成"异常"只是多一个要手动关的标签。
	// 会话活着时恒为 0。
	ExitStatus int
}

// ErrNoServer 表示"tmux server 根本连不上"。
//
// 必须是可判别的错误而不是"回空 map"：本包有过一次教训（见 noServerSays
// 的注释），把"没 server"咽成"没有会话"，让 TMUX_TMPDIR 指错目录被读成
// "会话全没了"。死因判定对这条更敏感 —— 空 map 会被上层当作
// "这些会话的名字都不在了"，正是最不该发生的连锁。
var ErrNoServer = errNoServer("tmux: 没有运行中的 server")

type errNoServer string

func (e errNoServer) Error() string { return string(e) }

// paneStatusFormat 每 pane 一行：会话名 | pane_dead | 退出码。
// 名字里不许有 |（面板生成的 lp-<数字> 满足；用户手建的怪名字段
// 对不上就整行跳过，宁可当"不知道"也不猜）。
const paneStatusFormat = "#{session_name}|#{pane_dead}|#{pane_dead_status}"

// ListPaneStatuses 一次调用拿到 prefix 下所有会话的 存活+死因。
//
// 用 list-panes -a 而不是逐会话查：N 个会话 N 次进程创建，而这东西每个
// 轮询周期都跑；一次 -a 全量拿回再本地分组，成本与 `tmux ls` 同量级。
// 没有 server 时报 ErrNoServer（见上）；server 活着但没有任何会话时
// 回空 map + nil error —— 这两种情况必须不同。
func ListPaneStatuses(bin, prefix string) (map[string]PaneStatus, error) {
	out, err := exec.Command(bin, "list-panes", "-a", "-F", paneStatusFormat).
		CombinedOutput()
	if err != nil {
		if noServerSays(strings.TrimSpace(string(out))) {
			return nil, fmt.Errorf("%w: %s", ErrNoServer, strings.TrimSpace(string(out)))
		}
		return nil, fmt.Errorf("list-panes -a: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return parsePaneStatuses(string(out), prefix), nil
}

// parsePaneStatuses 按会话聚合每 pane 一行。解析不出来的行整行跳过：
// 对不上格式就说明我们对这行没有把握，而"没把握"在上层是合法的第三种
// 状态（保持原样），"猜错"不是。
func parsePaneStatuses(out, prefix string) map[string]PaneStatus {
	res := make(map[string]PaneStatus)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, deadRaw, statusRaw, ok := splitPaneLine(line)
		if !ok || prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		dead, err := strconv.Atoi(deadRaw)
		if err != nil {
			continue
		}
		cur, seen := res[name]
		if dead == 0 {
			// 有任何活着的 pane：会话活着。退出码清零重计 ——
			// 死窗口的码不能污染一个还活着的会话。
			res[name] = PaneStatus{}
			continue
		}
		code, err := strconv.Atoi(statusRaw)
		if err != nil {
			// 尸体但读不出码：按异常处理（错的保守方向，见 PaneStatus 注释）。
			code = 1
		}
		next := PaneStatus{Dead: true, ExitStatus: cur.ExitStatus}
		if !seen || !cur.Dead || code > next.ExitStatus {
			next.ExitStatus = code
		}
		res[name] = next
	}
	return res
}

func splitPaneLine(line string) (name, dead, status string, ok bool) {
	parts := strings.Split(line, "|")
	if len(parts) != 3 || parts[0] == "" {
		return "", "", "", false
	}
	return parts[0], strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2]), true
}
