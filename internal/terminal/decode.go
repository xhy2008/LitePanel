package terminal

import (
	"strconv"
	"strings"
)

// DecodeOutput 还原 %output 值里的八进制转义。
//
// tmux(1) 的原话："value escapes non-printable characters and backslash
// as octal \xxx"。实测（探针 round7，逐字节 0x01..0xff）：
//
//	0x01..0x1f 与 0x5c 被编成 \001..\037、\134；
//	0x7f 与 0x80..0xff 原样（所以 UTF-8 中文是原始字节）；
//	可打印 ASCII 与 '%' 原样。
//
// 解码按文法来：'\' + 恰好三位八进制 → 对应字节；凑不齐三位就把反斜杠
// 当普通字符输出（tmux 不会产出这种输入，但坏输入不该 panic 或吞数据）。
func DecodeOutput(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s // 常见快路径：纯可打印
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) {
			o1, ok1 := octDigit(s[i+1])
			o2, ok2 := octDigit(s[i+2])
			o3, ok3 := octDigit(s[i+3])
			if ok1 && ok2 && ok3 {
				b.WriteByte(byte(o1*64 + o2*8 + o3))
				i += 4
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func octDigit(c byte) (int, bool) {
	if c >= '0' && c <= '7' {
		return int(c - '0'), true
	}
	return 0, false
}

// EncodeKeysHex 把任意字节编成 send-keys -H 的参数串。
//
// 为什么走 hex 而不是 send-keys -l 的字面量：字面量会经过 tmux 自己的
// 命令行解析（引号、$、分号都是它的），而用户的命令里全可能有这些。
// hex 是唯一能让 "echo 'a $b' ; rm -rf /" 原样落进 pane 的写法。
// 实测（探针 C/P3）：-H 与 -l -H 行为一致，取 -l -H（多一层"这是字面
// 按键不是键名"的保护）。
func EncodeKeysHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*3-1)
	for i, c := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}

// EscapePercent 无害存在（% 在 %output 值里原样），无需转义；
// 这里只留一个 strconv 依赖的说明位，避免未用导入。
var _ = strconv.Itoa
