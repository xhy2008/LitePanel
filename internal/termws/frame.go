package termws

import (
	"encoding/binary"
	"errors"
)

// 上行二进制帧的负载格式（外层已由 ws 的 [1B 频道名长][频道名][负载] 定位）：
//
//	'k' | 原始按键字节...
//	'r' | cols(2B 大端) | rows(2B 大端)
//
// 按键一律按原始字节传：前端 xterm.js 的 onData 已经把 Enter/方向键/Ctrl-C
// 序列化成字节，后端不再维护键位表。

const (
	frameKeys   byte = 'k'
	frameResize byte = 'r'
)

var (
	ErrShortFrame = errors.New("termws: 上行帧过短")
	ErrBadFrame   = errors.New("termws: 上行帧类型未知")
)

// KeysFrame 打包按键。
func KeysFrame(b []byte) []byte {
	out := make([]byte, 0, 1+len(b))
	out = append(out, frameKeys)
	return append(out, b...)
}

// ResizeFrame 打包尺寸上报。
func ResizeFrame(cols, rows int) []byte {
	out := []byte{frameResize, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(out[1:3], uint16(cols))
	binary.BigEndian.PutUint16(out[3:5], uint16(rows))
	return out
}

// Inbound 是解析后的上行帧。
type Inbound struct {
	IsResize   bool
	Keys       []byte
	Cols, Rows int
}

// ParseInbound 解析上行帧负载。
func ParseInbound(p []byte) (Inbound, error) {
	if len(p) < 1 {
		return Inbound{}, ErrShortFrame
	}
	switch p[0] {
	case frameKeys:
		return Inbound{Keys: p[1:]}, nil
	case frameResize:
		if len(p) < 5 {
			return Inbound{}, ErrShortFrame
		}
		return Inbound{
			IsResize: true,
			Cols:     int(binary.BigEndian.Uint16(p[1:3])),
			Rows:     int(binary.BigEndian.Uint16(p[3:5])),
		}, nil
	default:
		return Inbound{}, ErrBadFrame
	}
}
