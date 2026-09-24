package terminal

import (
	"fmt"
	"testing"
)

// 解码器：tmux 的 %output 值把「非可打印字符与反斜杠」编成八进制 \ooo，
// 其余字节原样。转义表不是猜的，是拿 0x01..0xff 逐字节喂给真 tmux 3.7c
// 反推的（见 dev/ctlprobe 探针）：
//
//	0x01..0x1f 变 \001..\037；0x5c 变 \134；0x7f 与 >=0x80 **原样**
//	（所以 UTF-8 中文是原始字节，不是八进制）；'%' 原样。

func TestDecode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"纯文本", "hello", "hello"},
		{"空", "", ""},
		// 控制字符：实测全部八进制
		{"SOH", `\001`, "\x01"},
		{"BEL", `\007`, "\a"},
		{"BS", `\010`, "\b"},
		{"TAB", `\011`, "\t"},
		{"LF", `\012`, "\n"},
		{"VT", `\013`, "\v"},
		{"FF", `\014`, "\f"},
		{"CR", `\015`, "\r"},
		{"US", `\037`, "\x1f"},
		{"ESC", `\033`, "\x1b"},
		// 反斜杠自己也编（否则 "\134033" 这种串会有歧义）
		{"反斜杠", `\134`, `\`},
		// 原样字节
		{"DEL 原样", "\x7f", "\x7f"},
		{"高位原样", "\x80\xff", "\x80\xff"},
		{"中文原样", "中文😀", "中文😀"},
		{"百分号原样", "%output %0 fake", "%output %0 fake"},
		{"可打印 ASCII 原样", "abc XYZ 0123456789 !@#$^&*()_+-=[]{};:',.<>/?", "abc XYZ 0123456789 !@#$^&*()_+-=[]{};:',.<>/?"},
		// 颜色：ANSI 的 ESC 是 \033，方括号与数字原样
		{"颜色序列", `\033[31mRED\033[0m`, "\x1b[31mRED\x1b[0m"},
		// 光标定位：R3 里 vim/htop 的命脉
		{"光标定位", `\033[H\033[2J\033[3;40H`, "\x1b[H\x1b[2J\x1b[3;40H"},
		{"保存恢复光标", `\0337\0338`, "\x1b7\x1b8"},
		{"备用屏切换", `\033[?1049h\033[?1049l`, "\x1b[?1049h\x1b[?1049l"},
		{"括号粘贴模式", `\033[?2004h`, "\x1b[?2004h"},
		// 混合：转义与原样交错
		{"混合", `A\015\012中文\033[1mb\134n`, "A\r\n中文\x1b[1mb\\n"},
		// 三八进制数字，值可达 377(=255)。0xFF 实测原样，但若 tmux 哪天
		// 编了也要能解
		{"三位高位八进制", `\377`, "\xff"},
		{"\000", `\000`, "\x00"},

		// ---- 非法/畸形输入：不 panic、不吞数据 ----
		{"孤立反斜杠结尾", `abc\`, "abc\\"},
		{"两位残缺八进制", `a\12b`, "a\\12b"},
		{"含非八进制数字", `a\18b`, "a\\18b"},
		{"反斜杠后是字母", `\n`, "\\n"},
		{"行尾单反斜杠不误吞下一段", `\033\`, "\x1b\\"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecodeOutput(c.in)
			if got != c.want {
				t.Fatalf("DecodeOutput(%q) = %q, 要 %q", c.in, got, c.want)
			}
		})
	}
}

// 全字节往返：0x01..0xff 里凡是 tmux 会编的，编码→解码必须回到原字节；
// 原样 passing 的也一样。这条比上面的表更狠 —— 它扫遍整个字节域。
func TestDecodeRoundTripsEveryByte(t *testing.T) {
	for b := 1; b <= 255; b++ {
		if b == 0x0a || b == 0x0d {
			// LF/CR 在协议里是行分隔，永远不会以原字节出现在值里
			continue
		}
		enc := escapeForTest(byte(b))
		got := DecodeOutput(enc)
		if got != string(rune(b)) && got != string([]byte{byte(b)}) {
			t.Fatalf("字节 %#02x 编成 %q 解回 %q", b, enc, got)
		}
	}
}

// escapeForTest 按探针确认的规则编码单字节：非可打印与反斜杠走八进制。
func escapeForTest(b byte) string {
	if b < 0x20 || b == 0x5c || b == 0x7f {
		return escapeOctal(b)
	}
	return string([]byte{b})
}

func TestDecodeOctalUpTo255(t *testing.T) {
	for v := 0; v <= 255; v++ {
		in := escapeOctal(byte(v))
		if got := DecodeOutput(in); len(got) != 1 || got[0] != byte(v) {
			t.Fatalf("%s 应解成 %#02x, got %q", in, v, got)
		}
	}
}

// escapeOctal 是测试侧的编码器：产品代码只需要解码。
func escapeOctal(b byte) string {
	return fmt.Sprintf("\\%03o", b)
}
