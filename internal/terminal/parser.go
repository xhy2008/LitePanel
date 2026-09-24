package terminal

import (
	"bytes"
	"strings"
)

// Kind 是一帧的角色。
type Kind int

const (
	EvOutput    Kind = iota // %output %N <解码后的字节>，直接喂终端
	EvBegin                 // %begin：命令响应块开始
	EvBlockData             // 块内原始字节（capture-pane 等命令的 stdout）
	EvEnd                   // %end：块成功结束
	EvError                 // %error：块以错误结束，内容仍是命令的 stderr
	EvNotify                // 其他认识的通知（含 %exit：会话没了）
	EvUnknown               // 不认识的行/通知：原样带上层，由上层决定
)

// Event 是一条解析结果。
type Event struct {
	Kind Kind
	Name string // 通知名，如 "%output"、"%session-changed"
	Pane string // %output 的 pane id（形如 "%0"）
	Data string // EvOutput：解码后的字节；EvBlockData：原始块内容
	Arg  string // 通知的剩余参数
	Raw  string // 原始行（未解码、未去尾部 \r），排错用
}

// dcsPreamble 是 tmux 在 control mode 流最前面写的 DCS 串。实测它**不带
// 换行**，直接贴在第一个 %begin 前面："\x1bP1000p%begin ... \r\n"。要按
// 行首判协议就得先摘掉它，否则首帧（通常是 attach 后的空块）会被当垃圾。
const dcsPreamble = "\x1bP1000p"

// knownNotifications 抄自 tmux(1) 的 CONTROL MODE 章节。认不出的 %xxx
// 走 EvUnknown 而不是当数据 —— 新版本 tmux 新增通知时，不该被误灌进终端。
var knownNotifications = map[string]bool{
	"%client-detached":         true,
	"%client-session-changed":  true,
	"%config-error":            true,
	"%continue":                true,
	"%exit":                    true,
	"%extended-output":         true,
	"%layout-change":           true,
	"%message":                 true,
	"%output":                  true,
	"%pane-mode-changed":       true,
	"%paste-buffer-changed":    true,
	"%paste-buffer-deleted":    true,
	"%pause":                   true,
	"%session-changed":         true,
	"%session-renamed":         true,
	"%session-window-changed":  true,
	"%sessions-changed":        true,
	"%subscription-changed":    true,
	"%unlinked-window-add":     true,
	"%unlinked-window-close":   true,
	"%unlinked-window-renamed": true,
	"%window-add":              true,
	"%window-close":            true,
	"%window-pane-changed":     true,
	"%window-renamed":          true,
}

// Parser 把 PTY 读到的任意切块切成事件。不是并发安全的，单 goroutine 喂。
//
// 三条来自实测（tmux 3.7c）的硬约束：
//  1. 读块在任意位置切断：一帧可能跨多次读，一次读也可能含多帧；
//  2. 协议行以 \r\n 结尾，且 %output 的值里 CR/LF 一定是 \015/\012 转义
//     —— 所以按行切分是安全的；
//  3. %begin/%end 块内的数据行**可以以 % 开头**（capture-pane 抓到的屏幕
//     内容里就有 "%output %0 ..." 这样的行）。man 明说通知不会出现在块内，
//     所以块内一律按原始数据处理；块的结束只认「命令编号与 %begin 相同」
//     的 %end/%error，否则屏幕上是可能正好显示着 "%end 1 2 3" 的。
type Parser struct {
	pending  []byte
	inBlock  bool
	blockNum string // 打开当前块的命令编号
	block    strings.Builder
	started  bool
}

// Feed 吃进一段原始字节，返回此刻能定案的事件。
func (p *Parser) Feed(chunk string) []Event {
	if !p.started {
		p.started = true
		chunk = strings.TrimPrefix(chunk, dcsPreamble)
	}
	p.pending = append(p.pending, chunk...)

	var out []Event
	for {
		i := bytes.IndexByte(p.pending, '\n')
		if i < 0 {
			break
		}
		line := string(p.pending[:i])
		p.pending = p.pending[i+1:]
		out = p.consume(strings.TrimSuffix(line, "\r"), out)
	}
	return out
}

// Flush 在连接结束时收尾：不这样做，最后一行没带 \n 的通知会静默消失。
func (p *Parser) Flush() []Event {
	var out []Event
	if len(p.pending) > 0 {
		line := string(p.pending)
		p.pending = nil
		out = p.consume(strings.TrimSuffix(line, "\r"), out)
	}
	if p.inBlock {
		// 块没收尾就断线：把手里的半块交出去，并用 EvError 让状态机复位，
		// 否则调用方会一直等一个永远不来的 %end。
		data := p.block.String()
		p.block.Reset()
		p.inBlock = false
		p.blockNum = ""
		if data != "" {
			out = append(out, Event{Kind: EvBlockData, Data: data})
		}
		out = append(out, Event{Kind: EvError, Name: "%error", Raw: "<连接中断>"})
	}
	return out
}

func (p *Parser) consume(line string, out []Event) []Event {
	if p.inBlock {
		if term, arg, ok := p.terminator(line); ok {
			data := p.block.String()
			p.block.Reset()
			p.inBlock = false
			p.blockNum = ""
			if data != "" {
				out = append(out, Event{Kind: EvBlockData, Data: data})
			}
			kind := EvEnd
			if term == "%error" {
				kind = EvError
			}
			return append(out, Event{Kind: kind, Name: term, Arg: arg, Raw: line})
		}
		// 块内：原样收，连 % 开头的行也不看。分隔补回 \r\n，让
		// capture-pane 的回放文本仍是终端能直接吃的行结束。
		p.block.WriteString(line)
		p.block.WriteString("\r\n")
		return out
	}

	if line == "" {
		return out
	}
	if line[0] != '%' {
		return append(out, Event{Kind: EvUnknown, Raw: line})
	}

	// line[0] 必是 '%'，所以 name 自带前缀，直接和 "%begin" 比。
	name, rest := cut(line)
	switch name {
	case "%begin":
		p.inBlock = true
		p.block.Reset()
		p.blockNum = nthField(rest, 1) // "<时间> <编号> <标志>"
		return append(out, Event{Kind: EvBegin, Name: name, Arg: rest, Raw: line})

	case "%end", "%error":
		// 块外的 %end：协议上不该有，容错成通知而不是丢掉
		kind := EvEnd
		if name == "%error" {
			kind = EvError
		}
		return append(out, Event{Kind: kind, Name: name, Arg: rest, Raw: line})

	case "%output":
		pane, val := cut(rest)
		return append(out, Event{
			Kind: EvOutput, Name: name, Pane: pane,
			Data: DecodeOutput(val), Raw: line,
		})

	case "%extended-output":
		// "%extended-output %0 123 : <值>"。':' 之前的字段是留给未来的，
		// 按 man 的说法直接忽略。tmux 3.7c 只在开了 pause-after 时才发，
		// 而该功能在这个版本不可用（探针 round9），解出来只是防将来。
		pane, tail := cut(rest)
		if _, after, ok := strings.Cut(tail, ":"); ok {
			tail = strings.TrimPrefix(after, " ")
		}
		return append(out, Event{
			Kind: EvOutput, Name: name, Pane: pane,
			Data: DecodeOutput(tail), Raw: line,
		})

	default:
		kind := EvNotify
		if !knownNotifications[name] {
			kind = EvUnknown
		}
		return append(out, Event{Kind: kind, Name: name, Arg: rest, Raw: line})
	}
}

// terminator 判断这行是不是当前块的真结尾。
//
// 必须比对命令编号：capture 到的屏幕内容里可能出现 "%end 1 2 3" 这样的
// 行（探针 round4 P1 实测到块内同时有假 %output/%begin/%end 三行），只按
// 前缀判会让块提前结束，之后的数据全被当成块外协议行。
func (p *Parser) terminator(line string) (name, arg string, ok bool) {
	for _, name = range []string{"%end", "%error"} {
		if line != name && !strings.HasPrefix(line, name+" ") {
			continue
		}
		arg = strings.TrimPrefix(strings.TrimPrefix(line, name), " ")
		if p.blockNum == "" {
			return name, arg, true // %begin 参数残缺，退化成按前缀认
		}
		if nthField(arg, 1) == p.blockNum {
			return name, arg, true
		}
	}
	return "", "", false
}

// cut 取第一个空格前的词与剩余部分（剩余保留原样，可含空格）。
func cut(s string) (string, string) {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// nthField 取第 n 个空格分隔字段（0 起）。
func nthField(s string, n int) string {
	for i := 0; i < n; i++ {
		_, rest := cut(s)
		s = rest
	}
	head, _ := cut(s)
	return head
}
