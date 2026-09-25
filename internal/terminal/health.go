package terminal

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// 启动时探测 tmux 可用性（设计 7.3）。
//
// 判定方向刻意保守：**认不出来就当不可用**。理由是两种误判的代价不对称——
// 误判"可用"会让用户点开终端页看到一片空白且没有任何解释；误判"不可用"
// 只是多显示一行安装引导。终端不可用是个可以带着用的面板，终端假装可用
// 不是。
//
// 不做静默降级到自研 pty：那会让"面板重启后终端还在"这个承诺（D5）悄悄
// 失效，用户需要知道真相。

// DefaultBin 是默认探测的 tmux 路径。
const DefaultBin = "tmux"

// MinVersion 是本实现要求的最低 tmux 版本。
//
// 设计 7.3 早期写的是 2.1，这里收紧到 3.2，因为实现真正依赖的能力比那晚：
// `%begin/%end` 携带命令编号（corr.go 靠它关联响应，见 parser.go）与
// window-size 的 latest 语义都是 3.x 的行为。环境探测（R8）确认目标机是
// tmux 3.6，所以这个下限不会挡住部署。
const MinVersion = "3.2"

// Health 是探测结果。字段直接给前端渲染引导文案用，故把原因写成中文句子
// 而不是错误码——这个接口唯一的消费者就是那个引导框。
type Health struct {
	Available  bool   `json:"available"`
	Version    string `json:"version"`
	MinVersion string `json:"min_version"`
	Bin        string `json:"bin"`
	Reason     string `json:"reason,omitempty"`
}

// versionRe 抓 x.y；`next-3.4`、`3.2a`、`3.7c` 都能落到同一个 (major,minor)。
var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// Probe 执行 `<bin> -V` 并判定可用性。bin 为空时用 PATH 里的 tmux。
func Probe(bin string) Health {
	if bin == "" {
		bin = DefaultBin
	}
	h := Health{Bin: bin, MinVersion: MinVersion}

	out, err := exec.Command(bin, "-V").CombinedOutput()
	text := strings.TrimSpace(string(out))
	switch {
	case errors.Is(err, exec.ErrNotFound):
		h.Reason = fmt.Sprintf("找不到 %s：请安装 tmux（apt install tmux）后重启面板", bin)
		return h
	case err != nil:
		// 退出码非 0：权限/socket 问题，或那个路径根本不是 tmux。
		// 把 tmux 的原文带进 reason，否则现场无从下手。
		h.Reason = fmt.Sprintf("%s -V 执行失败：%v（输出 %q）", bin, err, head(text, 120))
		return h
	}

	// 必须同时出现 "tmux" 字样和一个版本号：只看版本号会让任何自称
	// "xxx 9.9" 的程序通过探测，那样终端页会以错误的前提启动。
	if !strings.Contains(strings.ToLower(text), "tmux") {
		h.Reason = fmt.Sprintf("%s -V 的输出不像 tmux：%q", bin, head(text, 120))
		return h
	}
	m := versionRe.FindStringSubmatch(text)
	if m == nil {
		h.Reason = fmt.Sprintf("无法从 %q 里读出版本号", head(text, 120))
		return h
	}
	h.Version = versionToken(text, m[0])

	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	want := strings.Split(MinVersion, ".")
	wantMajor, _ := strconv.Atoi(want[0])
	wantMinor, _ := strconv.Atoi(want[1])

	// 后缀字母（3.2a / 3.7c）不参与比较：它们只是同一 minor 的发布修订，
	// 只会更不容易有 bug，不会更低。
	if major < wantMajor || (major == wantMajor && minor < wantMinor) {
		h.Reason = fmt.Sprintf("tmux %s 太旧，需要 %s 以上", h.Version, MinVersion)
		return h
	}
	h.Available = true
	return h
}

// versionToken 从输出里挑出**完整**的版本词（"3.7c" 而不是正则吃到的 "3.7"），
// 纯粹为了引导文案里给用户看到他自己机器上的真实版本号。
func versionToken(text, match string) string {
	for _, f := range strings.Fields(text) {
		if strings.Contains(f, match) {
			return f
		}
	}
	return match
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
