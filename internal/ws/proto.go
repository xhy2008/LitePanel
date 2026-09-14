package ws

import (
	"errors"
	"fmt"
)

// 二进制数据帧格式：[1B 频道名长度][频道名 UTF-8][原始字节]（设计 15 节）。
const maxChannelNameLen = 200

var (
	ErrShortFrame   = errors.New("ws: 帧长度不足")
	ErrBadChanLen   = errors.New("ws: 频道名长度字段与实际不符")
	ErrChanNameLong = fmt.Errorf("ws: 频道名超过 %d 字节", maxChannelNameLen)
)

// EncodeTermFrame 编码二进制数据帧。
func EncodeTermFrame(channel string, payload []byte) []byte {
	n := len(channel)
	if n > maxChannelNameLen {
		n = maxChannelNameLen
	}
	raw := make([]byte, 0, 1+n+len(payload))
	raw = append(raw, byte(n))
	raw = append(raw, channel[:n]...)
	raw = append(raw, payload...)
	return raw
}

// DecodeTermFrame 解析二进制数据帧。
func DecodeTermFrame(raw []byte) (channel string, payload []byte, err error) {
	if len(raw) < 1 {
		return "", nil, ErrShortFrame
	}
	n := int(raw[0])
	if len(raw) < 1+n {
		return "", nil, ErrBadChanLen
	}
	return string(raw[1 : 1+n]), raw[1+n:], nil
}
