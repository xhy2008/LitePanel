package main

import (
	"litepanel/internal/ws"
)

// termHub 把 *ws.Hub 适配成 termws.Hub。
//
// 为什么不直接让 *ws.Hub 实现 termws.Hub：那会让 ws 包为了一个桥接接口
// 把整条链路上的订阅者类型都改成 any，丢掉编译期类型检查换来的却是
// "桥接少一个 import"。窄接口 + 一处适配是更划算的交易。
//
// 适配器是生产代码，会被用错（最常见的是把频道名丢掉 —— 前缀登记下
// 一个回调对应多个会话，丢了就没法分派），所以有独立测试。
type termHub struct{ h *ws.Hub }

func (t termHub) BroadcastBin(ch string, payload []byte) { t.h.BroadcastBin(ch, payload) }

func (t termHub) SendBinTo(sub any, ch string, payload []byte) {
	if c, ok := sub.(*ws.Client); ok {
		t.h.SendBinTo(c, ch, payload)
	}
}

func (t termHub) OnJoin(prefix string, fn func(sub any, ch string)) {
	t.h.OnJoin(prefix, func(c *ws.Client, ch string) { fn(c, ch) })
}

func (t termHub) OnBinary(prefix string, fn func(sub any, ch string, payload []byte)) {
	t.h.OnBinary(prefix, func(c *ws.Client, ch string, p []byte) { fn(c, ch, p) })
}
