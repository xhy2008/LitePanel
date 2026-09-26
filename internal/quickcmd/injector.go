package quickcmd

// 注入器：把命令投进一个 tmux 终端会话（D20）。
//
// 发送走 `tmux send-keys -t <pane> -l -H <hex…>`，与 terminal.Session.SendText
// 同一套 hex 写法。为什么不复用 SendText（control mode）：control 连接只在
// 浏览器订阅时才存在，而"点快捷命令时没有任何设备打开着终端"是常态 ——
// 为发一行字去 attach 一条 control 连接、再把它的输出泵起来，代价花在
// 没人看的地方。CLI 的 send-keys 落到同一个 tty 上，hex 一个字节都不差。
//
// hex 而不是字面量的原因和 SendText 一样：命令里什么都有（引号、$、
// 反斜杠、分号、中文），任何一层多做解释都会变形 —— 而变形的命令是
// 会执行错的命令。hex 串必须按空格拆成多个参数（实测整体引用被静默吞掉）。

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"litepanel/internal/terminal"
)

// DefaultBusyWindow 是判定空闲所需的默认静默时长（秒，设计 5.3）。
// 设置页可改（“判定忙的时间窗口”）。
const DefaultBusyWindow = 2

// Injector 把命令注入到一个终端会话里。
type Injector struct {
	svc   *terminal.Service
	quiet int // 静默窗口（秒），见 terminal.PaneState.Busy
	now   func() time.Time
}

// NewInjector 组装注入器。quiet<=0 时判忙只看前台命令（不看输出窗口）。
func NewInjector(svc *terminal.Service, quiet int) *Injector {
	return &Injector{svc: svc, quiet: quiet, now: time.Now}
}

// Result 是一次注入的结果。
type Result struct {
	SessionID    int64  `json:"session_id"`
	IsNewSession bool   `json:"is_new_session"`
	Title        string `json:"title"`
}

// Run 把 c 投进一个终端会话：优先复用最久没动静的空闲会话，全都忙或
// 一个都没有时新建。返回的 session_id 供前端切到终端页选中标签。
//
// 为什么复用后还要留 is_new_session：前端的提示不一样 —— 复用是"在你的
// 会话里跑了"，新建是"你的会话正忙，另开了一个"。后者不说明就会让人
// 以为点错了。
func (in *Injector) Run(ctx context.Context, c Command) (Result, error) {
	if _, err := c.normalized(); err != nil {
		return Result{}, err
	}
	sessions, err := in.pick(ctx)
	if err != nil {
		return Result{}, err
	}
	// 从"最久没动静"的那个开始试：正在被人盯着敲的会话最不该被插一脚。
	for _, s := range sessions {
		st, err := terminal.PaneStateOf(terminal.DefaultBin, s.TmuxName)
		if err != nil {
			continue // 会话已经没了：List 与这里之间隔着一次用户点击的距离
		}
		// 死主 pane 的会话在 List 里仍可能 alive（别的 pane 活着），但
		// send-keys 的目标 =name:0.0 是死的：实测回 exit 0、命令蒸发，
		// 界面报 success 而屏幕一个字节都没变。忙不忙都轮不到它。
		if st.Dead {
			continue
		}
		if !st.Busy(in.quiet, in.now()) {
			if err := injectTo(s.TmuxName, c); err != nil {
				return Result{}, err
			}
			return Result{SessionID: s.ID, Title: s.Title}, nil
		}
	}

	meta, err := in.svc.Create(ctx, terminal.SessionInput{
		Title: "快捷命令 · " + c.Name,
	})
	if err != nil {
		return Result{}, fmt.Errorf("为快捷命令新建会话: %w", err)
	}
	// 新会话刚建，shell 可能还没画完第一个提示符；等它，否则命令会打进
	// new-session 与 shell 启动之间的空档（实测过：整行丢失且不报错）。
	waitPrompt(meta.TmuxName)
	if err := injectTo(meta.TmuxName, c); err != nil {
		return Result{}, err
	}
	return Result{SessionID: meta.ID, IsNewSession: true, Title: meta.Title}, nil
}

// pick 返回候选会话，按"最久没动的在前"排。alive 已由 Service.List
// 对着真实 tmux 刷过一遍。
func (in *Injector) pick(ctx context.Context) ([]terminal.SessionMeta, error) {
	items, err := in.svc.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]terminal.SessionMeta, 0, len(items))
	for _, it := range items {
		if it.Alive {
			out = append(out, it)
		}
	}
	// 插入排序就够：会话数量级是个位数到几十。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].LastAttachedAt < out[j-1].LastAttachedAt; j++ {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// injectTo 把可选的 cd、然后命令本体写进会话主 pane。
//
// cd 失败不拦命令：cwd 是"顺便在那儿跑"的便利项，不是安全边界（D14 明确
// 不做目录限制）。cd 报错会原样显示在终端里，命令随后在旧目录执行 ——
// 用户看得见发生了什么，比面板替它决定"那就不跑了"更好解释。
func injectTo(name string, c Command) error {
	if c.Cwd != "" {
		if err := send(name, "cd "+quoteShell(c.Cwd), true); err != nil {
			return err
		}
	}
	return send(name, c.Command, true)
}

func send(name, text string, enter bool) error {
	pane := name + ":0.0"
	if text != "" {
		args := append([]string{"send-keys", "-t", pane, "-l", "-H"}, hexSplit(text)...)
		if out, err := exec.Command(terminal.DefaultBin, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("注入 %s: %v (%s)", name, err, out)
		}
	}
	if enter {
		if out, err := exec.Command(terminal.DefaultBin, "send-keys", "-t", pane,
			"Enter").CombinedOutput(); err != nil {
			return fmt.Errorf("回车 %s: %v (%s)", name, err, out)
		}
	}
	return nil
}

// hexSplit 把每个字节写成独立的两位十六进制词。
//
// 必须是"多个参数"而不是一个长串：实测 `send-keys -l -H '68 65 6c'`
// 整体引用会被 tmux 静默吞掉（不报错也不发）。
func hexSplit(s string) []string {
	out := make([]string, 0, len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, fmt.Sprintf("%02x", s[i]))
	}
	return out
}

// quoteShell 单引号包住，内部单引号按 '\” 展开。
// cwd 里出现空格或引号不应该改变"cd 到哪里"这件事。
func quoteShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// waitPrompt 等新会话的 shell 画出第一个提示符（前台命令回到 shell 本身）。
//
// 等不到也照样发：超时说明环境异常，但命令发出去大概率还是对的；
// 直接报错反而把一个能用的会话变成"点了一下没反应"。
func waitPrompt(name string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := terminal.PaneStateOf(terminal.DefaultBin, name); err == nil &&
			st.Foreground != "" && st.Foreground == st.ShellName {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// BusyInfo 是"这个会话现在能不能投"的结论。
//
// 为什么不直接把 terminal.PaneState 丢给上层：PaneState.Busy 是个需要
// (quiet, now) 两个参数的**方法**，上层一旦拿到 PaneState 就得自己决定
// 传什么 —— quiet 窗口于是有了第二个主人。设置页把窗口从 2 秒调到 10 秒，
// 注入侧读配置、展示侧写死 2 秒，结果是四个标签都显示"空闲"而点下去每次
// 都被另开新会话，面板上一处报错都没有。结论必须只算一次。
type BusyInfo struct {
	Busy       bool   `json:"busy"`
	Foreground string `json:"foreground"`
	ShellName  string `json:"shell_name"`
	// Dead：主 pane 是尸体。不把 dead 折算成 busy=true —— 标签写
	// "忙"是谎报，用户会等一个永远不会来的"忙完"。dead 单独成字段，
	// 前端把标签标成"已退出"；点进来 Run 会另开新会话并明说。
	Dead bool `json:"dead"`
}

// Busy 给出一批会话现在能不能投。
//
// 判据与 Run 完全同源：同一个 quiet、同一个 now、同一个 PaneStateOf。
// 前端据此把标签标成"可投/忙"，如果这里的窗口与 Run 用得不一致，标签
// 说的和实际发生的就会相反（详见 BusyInfo）。
//
// 查不到的会话（库里活着但 tmux 里已 exit，或 id 本来就是编的）**跳过**，
// 不编造状态：前端拿不到的标签就不标，比标错好。但一个都问不到时报错 ——
// 空表会被画成"全都空闲"，而真原因可能是 tmux 刚被关掉，用户投进去的
// 命令会对着空气"成功"。
func (in *Injector) Busy(ctx context.Context, ids []int64) (map[int64]BusyInfo, error) {
	out := make(map[int64]BusyInfo, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	want := make(map[int64]string, len(ids))
	items, err := in.svc.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		for _, id := range ids {
			if it.ID == id && it.Alive {
				want[it.ID] = it.TmuxName
			}
		}
	}
	now := in.now()
	for id, name := range want {
		st, err := terminal.PaneStateOf(terminal.DefaultBin, name)
		if err != nil {
			continue // 刚被用户 exit：与 ids 里没有它等价
		}
		out[id] = BusyInfo{
			Busy:       st.Busy(in.quiet, now),
			Foreground: st.Foreground,
			ShellName:  st.ShellName,
			Dead:       st.Dead,
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("问不到任何会话的状态（tmux 不可用，或会话都已退出）")
	}
	return out, nil
}
