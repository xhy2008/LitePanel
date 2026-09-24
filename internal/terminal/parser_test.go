package terminal

import (
	"strings"
	"testing"
)

// 分帧器：把 PTY 读到的任意字节流切成事件。
//
// 两个必须同时成立的约束（探针实测）：
//  1. PTY 读块会在任意位置切断，一帧可能跨多次读，也可能一次读到多帧；
//  2. %begin/%end 之间的**数据**里可能出现以 % 开头的行（capture-pane
//     抓到的屏幕内容就有），那段必须当数据，不能当通知解析 —— man 明说
//     "A notification will never occur inside an output block"。

// collect 把若干读块喂进去，返回全部事件。
func collect(t *testing.T, chunks ...string) []Event {
	t.Helper()
	var p Parser
	var out []Event
	for _, c := range chunks {
		out = append(out, p.Feed(c)...)
	}
	out = append(out, p.Flush()...)
	return out
}

func summarize(evs []Event) string {
	var b strings.Builder
	for _, e := range evs {
		switch e.Kind {
		case EvOutput:
			b.WriteString("out(" + e.Pane + "," + e.Data + ") ")
		case EvBlockData:
			b.WriteString("data(" + strings.ReplaceAll(e.Data, "\n", "|") + ") ")
		case EvBegin:
			b.WriteString("begin ")
		case EvEnd:
			b.WriteString("end ")
		case EvError:
			b.WriteString("err ")
		case EvNotify:
			b.WriteString("notify(" + e.Name + " " + e.Arg + ") ")
		case EvUnknown:
			b.WriteString("unk(" + e.Raw + ") ")
		}
	}
	return b.String()
}

func TestSingleOutputNotification(t *testing.T) {
	evs := collect(t, "%output %0 hello\\015\\012\r\n")
	want := []Event{
		{Kind: EvOutput, Pane: "%0", Data: "hello\r\n", Raw: `%output %0 hello\015\012`},
	}
	if len(evs) != 1 || evs[0].Kind != want[0].Kind || evs[0].Data != want[0].Data {
		t.Fatalf("got %s", summarize(evs))
	}
	if evs[0].Pane != "%0" {
		t.Fatalf("pane 应为 %%0, got %q", evs[0].Pane)
	}
}

func TestBlockBeginDataEnd(t *testing.T) {
	evs := collect(t, "%begin 1363006971 2 1\r\n0: bash* (1 panes) [80x24]\r\n%end 1363006971 2 1\r\n")
	got := summarize(evs)
	// 块内行的分隔符按 \r\n 补回（回放给终端时才是完整的行结束），
	// 所以 summarize 里看到的是 "\r|"。
	if !strings.HasPrefix(got, "begin data(0: bash* (1 panes) [80x24]\r|) end") {
		t.Fatalf("事件序列不对: %s", got)
	}
}

func TestBlockError(t *testing.T) {
	evs := collect(t, "%begin 1 2 1\r\nparse error: unknown command: x\r\n%error 1 2 1\r\n")
	var sawErr, sawData bool
	for _, e := range evs {
		if e.Kind == EvError {
			sawErr = true
		}
		if e.Kind == EvBlockData && strings.Contains(e.Data, "parse error") {
			sawData = true
		}
	}
	if !sawErr || !sawData {
		t.Fatalf("要同时看到 error 与数据: %s", summarize(evs))
	}
}

// 块内数据以 % 开头 —— 这是"按行首 % 判协议"的经典翻车点。
// capture-pane 抓到的屏幕内容里就有这种行。
func TestPercentInsideBlockIsData(t *testing.T) {
	// 块内出现的假 %end 编号(9)必须与真 %begin 的编号(2)不同：这才是
	// 编号比对救命的场景 —— 屏幕上完全可能显示着 "%end 9 9 9" 这行字，
	// 只按前缀认就会让块提前结束，真 %end 之后的输出全被当协议行丢掉。
	in := "%begin 1 2 1\r\n%output %0 FAKE\r\n%begin 1 2 3 FAKE2\r\n%end 9 9 9\r\nAFTER-FAKE-END\r\n%end 1 2 1\r\n"
	evs := collect(t, in)
	var data string
	nEnd := 0
	for _, e := range evs {
		if e.Kind == EvBlockData {
			data += e.Data
		}
		if e.Kind == EvEnd {
			nEnd++
		}
	}
	for _, want := range []string{"%output %0 FAKE", "FAKE2", "%end 9 9 9", "AFTER-FAKE-END"} {
		if !strings.Contains(data, want) {
			t.Fatalf("块内数据缺 %q（%% 开头的行与假 %%end 之后的行都必须留着）: %s", want, summarize(evs))
		}
	}
	if nEnd != 1 {
		t.Fatalf("只应有 1 个真 %%end, got %d: %s", nEnd, summarize(evs))
	}
}

func TestExitNotification(t *testing.T) {
	for _, in := range []string{"%exit\r\n", "%exit no sessions\r\n"} {
		evs := collect(t, in)
		if len(evs) != 1 || evs[0].Kind != EvNotify || evs[0].Name != "%exit" {
			t.Fatalf("%q => %s", in, summarize(evs))
		}
	}
}

func TestSessionChangedArg(t *testing.T) {
	evs := collect(t, "%session-changed $0 probe\r\n")
	if len(evs) != 1 || evs[0].Name != "%session-changed" || evs[0].Arg != "$0 probe" {
		t.Fatalf("got %s", summarize(evs))
	}
}

// 关键：任意切块。逐字节喂必须与一次喂入得到相同事件。
func TestByteByByteFeeding(t *testing.T) {
	full := "%begin 1 2 1\r\nline one\r\n%output %0 x\033[31mred\033[0m\r\n%end 1 2 1\r\n%output %0 tail\r\n"
	whole := collect(t, full)

	var p Parser
	var oneAt []Event
	for i := 0; i < len(full); i++ {
		oneAt = append(oneAt, p.Feed(full[i:i+1])...)
	}
	oneAt = append(oneAt, p.Flush()...)

	if summarize(whole) != summarize(oneAt) {
		t.Fatalf("逐字节喂与整体喂不一致:\n整体: %s\n逐字: %s", summarize(whole), summarize(oneAt))
	}
}

// 一次读入含多帧（高速输出时常见）
func TestMultipleFramesInOneChunk(t *testing.T) {
	in := "%output %0 a\r\n%output %0 b\r\n%output %0 c\r\n"
	evs := collect(t, in)
	if len(evs) != 3 {
		t.Fatalf("要 3 个输出事件, got %d: %s", len(evs), summarize(evs))
	}
	if evs[0].Data != "a" || evs[2].Data != "c" {
		t.Fatalf("顺序错了: %s", summarize(evs))
	}
}

// 块被切断在中间：Flush 前不得把残块当完整事件吐出。
func TestIncompleteBlockNotEmittedEarly(t *testing.T) {
	var p Parser
	evs := p.Feed("%begin 1 2 1\r\nhalf")
	for _, e := range evs {
		if e.Kind == EvEnd || e.Kind == EvError {
			t.Fatalf("块还没结束就出现了终止事件: %s", summarize(evs))
		}
	}
	evs = append(evs, p.Feed("-line\r\n%end 1 2 1\r\n")...)
	if !strings.Contains(summarize(evs), "half-line") {
		t.Fatalf("跨读的数据没拼起来: %s", summarize(evs))
	}
}

// 值里含空（例如 "%output %0 " 后面什么都没有）
func TestEmptyOutputValue(t *testing.T) {
	evs := collect(t, "%output %0 \r\n")
	if len(evs) != 1 || evs[0].Data != "" {
		t.Fatalf("got %s", summarize(evs))
	}
	// 完全没有值
	evs = collect(t, "%output %0\r\n")
	if len(evs) != 1 || evs[0].Data != "" {
		t.Fatalf("无值形态: %s", summarize(evs))
	}
}

// 超长帧：200KB 单行（探针实测 tmux 会分多帧，但单帧也可能很大）
func TestVeryLongLine(t *testing.T) {
	big := strings.Repeat("X", 300*1024)
	evs := collect(t, "%output %0 "+big+"\r\n")
	if len(evs) != 1 || len(evs[0].Data) != len(big) {
		t.Fatalf("超长帧处理不对: %d 事件, 首帧 %d 字节", len(evs), len(evs[0].Data))
	}
}

// 非法/未知行：不能 panic，也不能吞掉后续帧。
func TestUnknownLineTolerated(t *testing.T) {
	evs := collect(t, "这是一行垃圾\r\n%output %0 ok\r\n")
	var ok bool
	for _, e := range evs {
		if e.Kind == EvOutput && e.Data == "ok" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("垃圾行后的正常帧丢了: %s", summarize(evs))
	}
	// 以 % 开头但不认识的：当未知通知，不是数据
	evs = collect(t, "%made-up-thing arg\r\n%output %0 after\r\n")
	var sawUnknown, sawOut bool
	for _, e := range evs {
		if e.Kind == EvUnknown {
			sawUnknown = true
		}
		if e.Kind == EvOutput {
			sawOut = true
		}
	}
	if !sawUnknown || !sawOut {
		t.Fatalf("未知通知处理不对: %s", summarize(evs))
	}
}

// % 开头的行恰好出现在块外的数据位置（不该发生，但要安全）
func TestBareLineOutsideBlock(t *testing.T) {
	// 首帧前的 DCS "\x1bP1000p"（探针实测 tmux 会在最前面发这个）
	evs := collect(t, "\x1bP1000p%begin 1 2 0\r\n%end 1 2 0\r\n")
	if !strings.Contains(summarize(evs), "begin end") {
		t.Fatalf("开头 DCS 字节应被容忍: %s", summarize(evs))
	}
}

func TestFlushDiscardsNothingUseful(t *testing.T) {
	// 连接被杀时可能留下没有 \r\n 结尾的残行；Flush 不该 panic，
	// 且残行如果是完整通知应被认出。
	evs := collect(t, "%exit")
	if len(evs) != 0 {
		// 允许两种合理行为：要么当不完整丢弃，要么 Flush 时补全识别
		t.Logf("Flush 行为: %s", summarize(evs))
	}
}

func TestPaneIDExtraction(t *testing.T) {
	for _, tc := range []struct{ in, pane string }{
		{"%output %0 data", "%0"},
		{"%output %1234 data", "%1234"},
		{"%output %0 ", "%0"},
	} {
		evs := collect(t, tc.in+"\r\n")
		if len(evs) != 1 || evs[0].Pane != tc.pane {
			t.Fatalf("%q => pane %q", tc.in, summarize(evs))
		}
	}
}

// 值里含 \012 之类转义时必须解码后才是喂给 xterm 的字节
func TestOutputIsDecoded(t *testing.T) {
	evs := collect(t, `%output %0 \033[31mRED\033[0m\015\012`+"\r\n")
	if len(evs) != 1 {
		t.Fatalf("got %s", summarize(evs))
	}
	want := "\x1b[31mRED\x1b[0m\r\n"
	if evs[0].Data != want {
		t.Fatalf("Data = %q, 要 %q", evs[0].Data, want)
	}
}

// TestEndCarriesCommandNumber 钉住一个集成测试踩出来的坑：块内的 %end /
// %error 必须把命令编号放进 Event.Arg。上层靠它把响应块和发出去的命令
// 对上号（attach 握手块也会发一个 %end，编号不同）；少了这个字段，等待
// 者永远等不到自己的 %end —— Capture 直接挂死。
func TestEndCarriesCommandNumber(t *testing.T) {
	evs := collect(t, "%begin 111 42 1\r\nhello\r\n%end 111 42 1\r\n")
	if len(evs) != 3 {
		t.Fatalf("要 3 个事件: %s", summarize(evs))
	}
	begin, end := evs[0], evs[2]
	if begin.Kind != EvBegin || end.Kind != EvEnd {
		t.Fatalf("形状不对: %s", summarize(evs))
	}
	if got := nthField(begin.Arg, 1); got != "42" {
		t.Fatalf("%%begin 编号解析失败: Arg=%q -> %q", begin.Arg, got)
	}
	if got := nthField(end.Arg, 1); got != "42" {
		t.Fatalf("%%end 必须带编号供上层匹配: Arg=%q -> %q", end.Arg, got)
	}

	evs = collect(t, "%begin 111 43 1\r\nbad\r\n%error 111 43 1\r\n")
	if len(evs) != 3 || evs[2].Kind != EvError {
		t.Fatalf("%%error 形状不对: %s", summarize(evs))
	}
	if got := nthField(evs[2].Arg, 1); got != "43" {
		t.Fatalf("%%error 必须带编号: Arg=%q -> %q", evs[2].Arg, got)
	}
}
