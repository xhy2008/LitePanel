package terminal

// pane 状态查询（快捷命令选会话用，设计 5.3 的"忙"判定）。
//
// 为什么放在 terminal 包：这是 tmux 的交互知识——哪个 format 给什么、
// pane_pid 到底指向谁。quickcmd 只需要"这个会话现在能不能安全注入"这个
// 结论，不该自己去拼 tmux format（前端的 GET /api/commands/busy 也要读它，
// 拼两遍就会漂开）。

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PaneState 是一个会话主 pane 的可观测状态。
type PaneState struct {
	TmuxName   string
	ShellName  string    // pane_pid 那个进程的名字，通常就是 bash/zsh
	Foreground string    // tmux 眼里的前台命令名
	LastOutput time.Time // 这个 window 最后一次有输出的时刻
}

// Busy 判断现在往里注入会不会打断正在跑的东西。
//
// 两条判据，缺一不可：
//
//  1. 前台命令不是 shell 本身 → 忙。这条覆盖 sleep/编译/下载这类
//     **一个字节都不输出**的前台进程：光看"最近有没有输出"对 sleep 60
//     完全不成立，照输出判 would 把命令喂给 sleep，用户看到的是点了没反应、
//     sleep 结束后命令行里多了句奇怪的话（验收用例就是 sleep）。
//     基准取 pane_pid 那个进程的 comm，而不是把 "bash" 写死：写死之后
//     换成 zsh/fish 的机器会永远判成忙 —— 每次点快捷命令都另开一个会话，
//     而用户完全不知道为什么。
//  2. 最近 quiet 秒内有输出 → 忙。这条覆盖纯 shell 循环：echo 是 builtin，
//     pane_current_command 一直是 bash，判据 1 看不见它。
//
// quiet<=0 时只剩判据 1（配置项，见设置页"判定忙的时间窗口"）。
func (s PaneState) Busy(quiet int, now time.Time) bool {
	if s.Foreground != "" && s.ShellName != "" && s.Foreground != s.ShellName {
		return true
	}
	if quiet > 0 && !s.LastOutput.IsZero() {
		d := now.Sub(s.LastOutput)
		// d>=0 不能省：输出发生在 now 之后不算“刚刚输出过”。
		// 不卡这个方呎的话 now.Sub(last) 是个负数，对任何上限都成立，
		// 于是时间基准一偏就永久判忙。
		return d >= 0 && d <= time.Duration(quiet)*time.Second
	}
	return false
}

// paneFormat 一次把三个字段取回来。
//
// 用 list-panes 而不是 display-message：后者拿不到退出码，会话不存在时
// 只会打印一行错误文本，得靠匹配错误文案分辨"没有"和"问失败了"。
const paneFormat = "#{pane_pid}\t#{pane_current_command}\t#{window_activity}"

// PaneStateOf 读会话主 pane 的状态。会话不存在时返回 error（上层据此
// 把它当"不在线"，而不是当成空闲 —— 后者会让注入对着空气成功）。
func PaneStateOf(bin, name string) (PaneState, error) {
	out, err := exec.Command(bin, "list-panes", "-t", "="+name, "-F", paneFormat).
		CombinedOutput()
	if err != nil {
		return PaneState{}, fmt.Errorf("读 pane 状态 %s: %v (%s)",
			name, err, strings.TrimSpace(string(out)))
	}
	// 只取第一个 pane：面板创建的会话都是单 pane。用户自己分屏后，
	// 注入的目标本来就是主 pane，用别的 pane 的状态替它作决定反而更糟。
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	fields := strings.Split(line, "\t")
	if len(fields) != 3 {
		return PaneState{}, fmt.Errorf("读 pane 状态 %s: 解析不了 %q", name, line)
	}
	st := PaneState{TmuxName: name, Foreground: strings.TrimSpace(fields[1])}
	if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
		st.ShellName = procComm(pid)
	}
	if act, err := strconv.ParseInt(fields[2], 10, 64); err == nil && act > 0 {
		st.LastOutput = time.Unix(act, 0)
	}
	return st, nil
}

// procComm 读 /proc/<pid>/comm。读不到返回 ""（上层退化成只看输出窗口）。
func procComm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
