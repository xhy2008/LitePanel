package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// Session 是一个 tmux 会话加一条 control mode 连接，可多 goroutine 并发调用。
//
// 为什么不是裸 unix socket：`tmux -CC attach` 的客户端进协议之前就要 tty
// （tcgetattr failed），而服务端 socket 不接受我们自造的 identify 握手
// （实测被直接关闭）。所以面板自己开 PTY 把 tmux 客户端挂上去 —— PTY
// 只是协议载体，两端说的都是 control mode，写裸文本进去会被当命令解析。
//
// 一次只允许一条等响应的命令：control mode 的 %begin/%end 里命令编号由
// tmux 自己定（实测客户端自带 "%1 " 前缀是 parse error），没法自带关联
// ID，并发命令的响应分不开。

// SessionOpts 是建会话时可配的几项。
type SessionOpts struct {
	Cols, Rows   int
	Cwd          string // 空则用 tmux 默认
	Shell        string // 空则用 default-shell
	HistoryLimit int    // 0 = 不动 tmux 设置
}

// eventBuf 是事件缓冲深度。
//
// tmux 3.7c 没有流速控制（实测 refresh-client -A 各种写法全被拒），浏览器
// 侧停止读取时只能自己兜：缓冲区满就丢帧并计数。为什么敢丢而不是阻塞：
// 卡住 readLoop 会让所有命令响应超时、并把管道一路堵回 pane，整条终端
// 僵死；丢输出只是花屏一下，下一次重绘就好了。
const eventBuf = 4096

// ErrExited 表示 control 连接已结束（会话被杀或 shell 退出）。
var ErrExited = errors.New("tmux control 连接已结束")

// ErrBusy 表示上一条等响应的命令还没结束。
var ErrBusy = errors.New("上一条命令还没结束（control 连接一次只能跑一条）")

type Session struct {
	name string
	bin  string

	ptmx  *os.File
	slave *os.File
	cmd   *exec.Cmd

	p      Parser
	events chan Event

	// ready 在 attach 握手块结束时关闭。没握手就发命令，"下一个 %begin
	// 就是我的" 这个前提不成立（见 corr）。
	ready   chan struct{}
	readyMu sync.Mutex
	handled bool

	exited     chan struct{}
	exitOnce   sync.Once
	exitReason string

	cmdMu   sync.Mutex
	pending *corr

	dropped atomic.Int64

	closeOnce sync.Once
}

// CreateSession 建会话并 attach 一条 control 连接。
//
// history-limit 用「建完之后 set -t」而不是 set -g：后者会改掉这台机器
// 上所有未来会话的缺省，面板不该动别人的默认值。实测建完再设同样生效。
func CreateSession(bin, name string, o SessionOpts) (*Session, error) {
	args := []string{"new-session", "-d", "-s", name}
	if o.Cols > 0 && o.Rows > 0 {
		args = append(args, "-x", strconv.Itoa(o.Cols), "-y", strconv.Itoa(o.Rows))
	}
	if o.Cwd != "" {
		args = append(args, "-c", o.Cwd)
	}
	if o.Shell != "" {
		args = append(args, o.Shell)
	}
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("new-session %s: %v (%s)", name, err, bytes.TrimSpace(out))
	}
	// 从这里开始，tmux 里那个叫 name 的会话是**我们刚造的**，后面任何一步
	// 失败都得把它收掉 —— 否则它是个没人登记过的残：死因清理扫不到它
	//（靠库里的行反推），名字又把它占死：SQLite 的 INTEGER PRIMARY KEY 会
	// 复用刚删掉的 rowid（实测），下一次建会话正好撞上 duplicate session。
	//
	// handoff 不是多余的保险，它圈出两件事的分界：
	//  1. 上面那次 new-session **失败**时也常常 tmux 里有个同名会话
	//     （duplicate）。那个是别人的（手工建的/收编来的），无条件
	//     defer-kill 就会把人家的会话杀了 —— 比留残严重得多。
	//  2. 成功路径必须在 return 之前置位 —— defer 在 return 表达式
	//     求值**之后**才跑，写成 `return Attach(...)` 的话，刚建好的
	//     会话会被自己的清理杀掉。
	handoff := false
	defer func() {
		if !handoff {
			_ = KillSession(bin, name)
		}
	}()

	// remain-on-exit：整条"退出码=死因"规则（见 death.go）的地基。
	// 不开这个选项，shell 退出时连会话一起消失，"正常退出"和
	// "被杀/重启"就永远分不出来 —— 实测三种死法的事件流一模一样。
	if err := ArmCorpse(context.Background(), bin, name); err != nil {
		return nil, err
	}
	if o.HistoryLimit > 0 {
		// 失败不致命：会话已建起来，限流没生效顶多历史短点
		_, _ = exec.Command(bin, "set-option", "-t", name,
			"history-limit", strconv.Itoa(o.HistoryLimit)).CombinedOutput()
	}
	sess, err := Attach(bin, name)
	if err != nil {
		return nil, err
	}
	handoff = true
	return sess, nil
}

// Attach 对已存在的会话开 control 连接。
func Attach(bin, name string) (*Session, error) {
	if out, err := exec.Command(bin, "has-session", "-t", name).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("会话 %s 不存在: %v (%s)", name, err, bytes.TrimSpace(out))
	}
	s := &Session{
		name:   name,
		bin:    bin,
		events: make(chan Event, eventBuf),
		ready:  make(chan struct{}),
		exited: make(chan struct{}),
	}
	if err := s.start(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Session) start() error {
	ptmx, slave, err := openPty()
	if err != nil {
		return fmt.Errorf("开 PTY: %w", err)
	}
	s.ptmx, s.slave = ptmx, slave

	cmd := exec.Command(s.bin, "-CC", "attach", "-t", s.name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// Setsid+Setctty：让 tmux 客户端把这个 PTY 当控制终端。不设
	// Pdeathsig —— 面板被 kill -9 时子进程随面板死，而 tmux 侧只是少一个
	// client，会话照跑（实测正是 D5 要的行为）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		ptmx.Close()
		slave.Close()
		return fmt.Errorf("attach %s: %w", s.name, err)
	}
	s.cmd = cmd

	go s.readLoop()
	go func() {
		// 退出只能靠子进程：面板自己持有 slave fd，写端不会全关，master
		// 永远读不到 EOF（实测踩过）。
		_ = cmd.Wait()
		s.markExited("control 子进程结束")
	}()
	return nil
}

func (s *Session) markExited(reason string) {
	s.exitOnce.Do(func() {
		if reason != "" {
			s.exitReason = reason
		}
		close(s.exited)
		s.markReady() // 别让等握手的调用方永远等下去
	})
}

func (s *Session) markReady() {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if !s.handled {
		s.handled = true
		close(s.ready)
	}
}

// Events 是解码后的事件流。单消费者模型：消费不过来会丢帧并计数。
func (s *Session) Events() <-chan Event { return s.events }

// Ready 在 attach 握手块结束时关闭：此刻起发出的命令才谈得上响应关联，
// 此刻起的输出才是"实时流"（control attach 不回放历史，回放只能靠 Capture）。
// 连接若直接失败，Ready 也会关闭 —— 调用方随后从 Exited 分辨原因。
func (s *Session) Ready() <-chan struct{} { return s.ready }

// Exited 在 control 连接结束时关闭（shell 退出、会话被杀、连接被拆）。
func (s *Session) Exited() <-chan struct{} { return s.exited }

// ExitReason 是退出原因。
func (s *Session) ExitReason() string { return s.exitReason }

// Name 是 tmux 会话名。
func (s *Session) Name() string { return s.name }

// Dropped 返回因下游跟不上而丢掉的输出帧数。
func (s *Session) Dropped() int64 { return s.dropped.Load() }

// readLoop 是唯一读 PTY 的 goroutine：解析、分发、喂命令响应。
func (s *Session) readLoop() {
	defer func() {
		for _, e := range s.p.Flush() {
			s.dispatch(e)
		}
		s.failAllPending()
		s.markExited("连接已关闭")
		s.ptmx.Close()
		s.slave.Close()
	}()

	buf := make([]byte, 1<<16)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			for _, e := range s.p.Feed(string(buf[:n])) {
				s.dispatch(e)
			}
		}
		if err != nil {
			return // EOF 或 fd 被 Close
		}
	}
}

func (s *Session) dispatch(e Event) {
	// 握手块（attach 后 tmux 自发的那一个）结束 == 可以发命令了
	if !s.isReady() {
		switch e.Kind {
		case EvEnd, EvError:
			s.markReady()
			return // 握手块不外抛
		case EvBegin, EvBlockData:
			return
		}
	}

	// 命令响应优先；被认领的事件不外抛
	s.cmdMu.Lock()
	p := s.pending
	s.cmdMu.Unlock()
	if p != nil && p.accept(e) {
		if p.resolvedNow() {
			s.cmdMu.Lock()
			if s.pending == p {
				s.pending = nil
			}
			s.cmdMu.Unlock()
		}
		return
	}

	if e.Kind == EvNotify && e.Name == "%exit" {
		// 实测 %exit 不带原因参数
		s.markExited(strings.TrimSpace(e.Arg))
	}
	s.emit(e)
}

func (s *Session) isReady() bool {
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

func (s *Session) emit(e Event) {
	select {
	case s.events <- e:
	default:
		s.dropped.Add(1)
	}
}

func (s *Session) failAllPending() {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	if s.pending != nil {
		s.pending.abort(ErrExited)
		s.pending = nil
	}
}

// Run 发一条 control 命令并等它的响应块。只有需要输出的命令（capture-pane
// 等）才用它；发键、改尺寸走 sendOneWay，不等响应。
//
// 参数一律引用：control mode 的命令要过 tmux 自己的命令行分词，而
// "#{...}" 会被当格式串展开 —— 会话名里带 "#{ 就会出事。
//
// 例外：send-keys -H 的 hex 串**不能整套引用**（实测 `send-keys -l -H
// '68 65 6c'` 被静默吞掉，不报错也不发），所以那边按空格拆成多个参数。
func (s *Session) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) == 0 || args[0] == "" {
		return "", errors.New("空命令")
	}
	if err := s.waitReady(ctx); err != nil {
		return "", err
	}
	p := &corr{done: make(chan struct{})}
	s.cmdMu.Lock()
	if s.pending != nil {
		s.cmdMu.Unlock()
		return "", ErrBusy
	}
	s.pending = p
	s.cmdMu.Unlock()

	if err := s.writeLine(quoteAll(args)); err != nil {
		s.dropPending(p)
		return "", err
	}
	select {
	case <-p.done:
		s.dropPending(p)
		return string(p.data), p.err
	case <-ctx.Done():
		// 必须能取消：capture 卡住（会话被人 kill-pane）时不能把调用方
		// 的连接永久占住，也不能让整条 readLoop 关联到不存在的响应上
		s.dropPending(p)
		p.abort(ctx.Err())
		return "", ctx.Err()
	case <-s.exited:
		s.dropPending(p)
		return "", ErrExited
	}
}

func (s *Session) dropPending(p *corr) {
	s.cmdMu.Lock()
	if s.pending == p {
		s.pending = nil
	}
	s.cmdMu.Unlock()
}

func (s *Session) waitReady(ctx context.Context) error {
	select {
	case <-s.ready:
		select {
		case <-s.exited:
			return ErrExited
		default:
			return nil
		}
	case <-s.exited:
		return ErrExited
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendOneWay 发一条不等响应的命令（send-keys / refresh-client）。
func (s *Session) sendOneWay(ctx context.Context, args ...string) error {
	if err := s.waitReady(ctx); err != nil {
		return err
	}
	return s.writeLine(quoteAll(args))
}

func (s *Session) writeLine(cmd string) error {
	select {
	case <-s.exited:
		return ErrExited
	default:
	}
	// 命令裸写 + \n。不要给命令行加 "%1 " 这类前缀（实测 parse error：
	// 编号是 tmux 自己加的，不是客户端给的）。
	if _, err := s.ptmx.Write(append([]byte(cmd), '\n')); err != nil {
		return fmt.Errorf("写 control 命令: %w", err)
	}
	return nil
}

// SendText 把任意文本原样送进 pane；末尾的换行会变成回车执行。
//
// 走 send-keys -H 的十六进制而不是字面量：用户命令里什么都有（引号、$、
// 反斜杠、分号、中文），任何一层多做解释都会变形 —— 这是快捷命令注入
// （D20）的生命线。实测 `-l -H` 与只给 `-H` 等效，取带 -l 的写法，多一层
// 「这是字面按键、不是键名」的保护。
func (s *Session) SendText(ctx context.Context, text string) error {
	body := text
	trailing := false
	for len(body) > 0 && (body[len(body)-1] == '\n' || body[len(body)-1] == '\r') {
		body = body[:len(body)-1]
		trailing = true
	}
	if body != "" {
		// 每个字节码独立成词（实测整体引用会被静默丢弃）
		args := append([]string{"send-keys", "-l", "-H"},
			strings.Fields(EncodeKeysHex([]byte(body)))...)
		if err := s.sendOneWay(ctx, args...); err != nil {
			return err
		}
	}
	if trailing {
		return s.SendEnter(ctx)
	}
	return nil
}

// SendEnter 发回车（执行命令）。
func (s *Session) SendEnter(ctx context.Context) error {
	return s.sendOneWay(ctx, "send-keys", "Enter")
}

// Interrupt 送 Ctrl-C（中断前台程序）。
//
// 键盘上的 Ctrl-C 是**字节 0x03**，只能以原始控制字节送进去；送字面文本
// "^C" 只会打出两个可见字符（有专门的测试钉住这点）。用 -H 03 而不是键名
// 形式 C-c：键名要查 tmux 的键表，而我们的输入路径本来就绕不开 hex。
//
// 为什么"只支持普通命令"这个简化里它不能跟着砍：apt update / ping / make
// 跑歪了都要能停，这是普通命令的一部分，不是 TUI 的一部分。
func (s *Session) Interrupt(ctx context.Context) error {
	return s.sendOneWay(ctx, "send-keys", "-H", "03")
}

// Resize 上报前端尺寸。
//
// 用 refresh-client -C 而不是 resize-window：控制模式客户端的尺寸是它自己
// 报的，窗口尺寸取所有 client 的最小值。实测改 PTY 尺寸对窗口毫无影响，
// 而 refresh-client -C 立刻生效，并让 pane 里的全屏程序收到 SIGWINCH。
func (s *Session) Resize(ctx context.Context, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("非法尺寸 %dx%d", cols, rows)
	}
	return s.sendOneWay(ctx, "refresh-client", "-C", fmt.Sprintf("%dx%d", cols, rows))
}

// CaptureAll 取回整个 scrollback（含 ANSI），用于重连回放。
//
// `-S -` 才是"从历史开头"。踩过的坑：`-S -1` 只给历史上 1 行，实测截回
// 来的正好是一屏，看起来"回放正常"其实历史全丢了。
//
// 块内是**原样字节**（真实 ESC 而非 \033 转义文本），所以不解码，直接
// 喂给前端终端。
func (s *Session) CaptureAll(ctx context.Context) (string, error) {
	return s.Run(ctx, "capture-pane", "-p", "-e", "-S", "-", "-t", s.name)
}

// CaptureBack 取回最近 lines 行历史（含可见区）。给"只看最近 N 行"之类
// 的场景用，避免把几 MB scrollback 灌给一个刚打开的页面。
func (s *Session) CaptureBack(ctx context.Context, lines int) (string, error) {
	if lines <= 0 {
		return s.CaptureAll(ctx)
	}
	return s.Run(ctx, "capture-pane", "-p", "-e", "-S", "-"+strconv.Itoa(lines), "-t", s.name)
}

// ReplayBytes 生成"喂给一个刚打开的前端终端"的完整字节串。
//
// 三个部分，缺一不可，都是实测出来的：
//
//  1. 先清屏归零：客户端可能有掉线前的残留内容，不清会和新内容叠成花屏。
//  2. 再写 capture-pane -e 的整段历史（原样 ANSI，见 CaptureAll）。
//  3. 最后必须补一条 CUP 把光标放到 tmux 认为的位置。**这一步是"重连后
//     能看不能打字"的根源**：capture 出来的每行都以换行结尾，喂完之后
//     客户端光标停在最后一行的下一行行首，而 tmux 里光标其实停在提示符
//     中间（用户还没回车）。差一个字，之后每个字符都错一行。
//
// 还要**去掉正文末尾那个换行**：视口只有一屏，多打一个换行会让终端往上
// 滚一行，于是第一行被推进客户端 scrollback，可见区整体错位一行。
//
// 前提：窗口尺寸必须已经按这个客户端 Resize 过（行/列不一致时，换行的
// 折行位置和 CUP 的行号都对不上）。 newcom 的顺序是 Resize -> ReplayBytes -> 实时流。
func (s *Session) ReplayBytes(ctx context.Context) (string, error) {
	info, err := s.paneInfo(ctx)
	if err != nil {
		return "", err
	}
	text, err := s.CaptureAll(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString(strings.TrimSuffix(text, "\r\n"))
	// CUP 是 1 基坐标，tmux 的 cursor_x/y 是 0 基
	fmt.Fprintf(&b, "\x1b[%d;%dH", info.cy+1, info.cx+1)
	_ = info.cols
	return b.String(), nil
}

// paneInfo 取回放需要的几何信息（尺寸 + 光标位置，均 0 基）。
func (s *Session) paneInfo(ctx context.Context) (paneInfo, error) {
	out, err := s.Run(ctx, "display-message", "-t", s.name, "-p",
		"#{pane_width} #{pane_height} #{cursor_x} #{cursor_y}")
	if err != nil {
		return paneInfo{}, err
	}
	var pi paneInfo
	fs := strings.Fields(out)
	if len(fs) != 4 {
		return paneInfo{}, fmt.Errorf("display-message 返回 %d 个字段，要 4 个: %q", len(fs), out)
	}
	for i, v := range fs {
		n, err := strconv.Atoi(v)
		if err != nil {
			return paneInfo{}, fmt.Errorf("display-message 第 %d 个字段 %q 不是数字", i, v)
		}
		switch i {
		case 0:
			pi.cols = n
		case 1:
			pi.rows = n
		case 2:
			pi.cx = n
		case 3:
			pi.cy = n
		}
	}
	if pi.cols <= 0 || pi.rows <= 0 {
		return paneInfo{}, fmt.Errorf("pane 几何非法: %+v", pi)
	}
	return pi, nil
}

type paneInfo struct {
	cols, rows, cx, cy int
}

// Close 只拆这条 control 连接，不动 tmux 会话 —— D5 的全部重量压在这条上：
// 关浏览器、面板重启、WS 掉线都不该杀掉用户正在跑的任务。
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			// 杀客户端等价于 detach：tmux 会给同会话的其他 client 发
			// %sessions-changed，会话本身继续活着（实测）
			_ = s.cmd.Process.Kill()
		}
		_ = s.ptmx.Close() // 让 readLoop 立刻返回
	})
	return nil
}

// KillSession 真的删掉 tmux 会话（只有面板里点「删除会话」才走这里）。
//
// "=名字" 不能省：tmux 的 -t 只在**存在精确匹配**时优先精确。名字已经不
// 存在、而恰好有个前缀邻居时，`kill-session -t lp-2` 会打到 lp-20 上。
// 这条组合完全可达：库里的行还在（用户直接在 tmux 里 exit 了会话），
// 面板上点删除 —— 于是删掉了别人的会话，且不可逆。
// 上层 Delete 靠 HasSession（同样精确）先挡一道，但这里也必须自守：
// 否则任何新调用点都自带这个坑。
func KillSession(bin, name string) error {
	if out, err := exec.Command(bin, "kill-session", "-t", "="+name).CombinedOutput(); err != nil {
		return fmt.Errorf("kill-session %s: %v (%s)", name, err, bytes.TrimSpace(out))
	}
	return nil
}

// HasSession 判断 tmux 里是否存在该会话（精确名）。
//
// 必须用 "=名字"：tmux 的 -t 默认是**前缀匹配**，lp-1 会命中 lp-10
// （实测过会话共存的场景）。删错会话比删不掉严重得多。
// server 整个没起时回 (false, nil)：那是"会话不存在"，不是查询失败。
func HasSession(bin, name string) (bool, error) {
	out, err := exec.Command(bin, "has-session", "-t", "="+name).CombinedOutput()
	if err == nil {
		return true, nil
	}
	msg := strings.TrimSpace(string(out))
	if noServerSays(msg) {
		return false, nil
	}
	return false, fmt.Errorf("has-session %s: %v (%s)", name, err, msg)
}

// noServerSays 判断 tmux 的报错是否属于“根本没有 server / 根本没有会话”。
//
// 必须归到一起判，而不是到处散着 strings.Contains("no server running")：
// tmux 在这几种情况下说的话完全不同，而它们都是合法状态，不是故障：
//
//   - server 起着、会话已被清空：       “no sessions”
//   - server 起着、只查不到那个名字： “can't find session”
//   - socket 都不存在（装着 tmux 但从没起过 server，全新机器/
//     重启后的常态）：“error connecting to <path> (No such file or directory)”
//
// 最后一条本包原先不认，后果是面板第一次启动就是 500 —— 而那次启动
// 恰恰是用户第一次打开面板（实测 tmux 3.7c）。真故障（权限不足、
// socket 属主不对）不匹配这些文案，仍然会报错。
func noServerSays(msg string) bool {
	low := strings.ToLower(msg)
	for _, s := range []string{
		"no server running",
		"no sessions",
		"can't find session",
	} {
		if strings.Contains(low, s) {
			return true
		}
	}
	// socket 不存在单独判：connect 失败会说 “error connecting to <path> (原因)”，
	// 而括号里的原因才是关键。Permission denied 必须继续报错（那可能是别人
	// 的 tmux server，把它当成“没会话”会谎报状态），只有 ENOENT 才是“没起过”。
	return strings.Contains(low, "error connecting to") &&
		strings.Contains(low, "no such file or directory")
}

// ListSessions 列出带指定前缀的会话名，供面板启动对账。
//
// 走 CLI 而不是 control 的 list-sessions：控制连接分不到命令编号，把列表
// 从通知里剥出来更脆；CLI 有干净的退出码。
func ListSessions(bin, prefix string) ([]string, error) {
	out, err := exec.Command(bin, "list-sessions", "-F", "#{session_name}").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if noServerSays(msg) {
			return nil, nil
		}
		return nil, fmt.Errorf("list-sessions: %v (%s)", err, msg)
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || (prefix != "" && !strings.HasPrefix(l, prefix)) {
			continue
		}
		names = append(names, l)
	}
	return names, nil
}

// quoteAll 把参数拼成一条 control 命令，逐个按需引用。
func quoteAll(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = QuoteArg(a)
	}
	return strings.Join(parts, " ")
}

// QuoteArg 按 POSIX shell 风格引用一个参数。
//
// tmux 的 control 命令行走自己的 shell-like 分词：空格、引号、反斜杠、
// "#{...}" 都会改变含义。保守做法 —— 只要不是纯安全字符就整段单引号包住。
func QuoteArg(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			strings.IndexByte("-_.,:/=%+", c) >= 0 {
			continue
		}
		safe = false
		break
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\\''`) + "'"
}

// ArmCorpse 给一个已存在的会话开启 remain-on-exit（尸检的前提）。
//
// 新建与收编都必须调用：收编来的手工会话没开这个选项的话，正常
// 退出会连尸体一起消失，死因判定只能把它归为"异常消失"。
//
// 必须独立成命令：create-session 的 -t remain-on-exit 格式串只作用
// 于新建窗口，不会变成会话级选项（实测）。设不上就直接报错，不
// 要像早先那样用 `_ = exec...` 吞掉 —— 静默漏设会让所有会话重新
// 变回"死因看不出来"，而这是没有错误信号的故障。
func ArmCorpse(ctx context.Context, bin, name string) error {
	if out, err := exec.CommandContext(ctx, bin, "set-option", "-t", name,
		"remain-on-exit", "on").CombinedOutput(); err != nil {
		return fmt.Errorf("set-option remain-on-exit %s: %v (%s)",
			name, err, bytes.TrimSpace(out))
	}
	return nil
}
