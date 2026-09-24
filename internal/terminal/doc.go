// Package terminal 提供面板的终端能力：把 tmux 会话桥接到浏览器 xterm.js。
//
// 架构是 D5 定的：走 tmux 的 control mode（`tmux -CC attach`），不是普通
// attach。理由值得留档，因为普通 attach 看起来"简单得多"：
//
//   - 多客户端同屏（实测）：control mode 下每个连接都能独立收到 pane 的
//     %output，所以 PC 上敲的命令，手机同时能看到并且各自按自己的尺寸渲染。
//     普通 attach 实测只有最后一个活动客户端能收到输出，其余连接拿到的是
//     空流 —— 换设备就看不到正在跑的命令，这条是硬需求，所以必须 control mode。
//   - 不干扰用户：control mode 的输入是显式 send-keys，不会被 tmux 的
//     prefix（C-b）抢走按键；普通 attach 实测会把 C-b d 当成 detach。
//
// 代价是这个包里要自己还原 tmux 的渲染状态：
//   - %output 的八进制转义要解码成原始字节（decode.go）；
//   - 行式协议要分帧，且 %begin/%end 块内的数据可能长得像协议行（parser.go）；
//   - 命令与响应块靠 tmux 自己编的命令编号关联，客户端无法自带 ID（corr.go）；
//   - control attach **不会**画已有屏幕（实测首块 0 字节），所以重连回放要
//     自己 capture-pane 拼，还要补 CUP 修光标（session.go 的 ReplayBytes）。
//
// 本包所有行为约定来自对 tmux 3.7c 的实测（探针脚本见仓库 dev/ 下的
// ctlprobe/paintprobe/xtermcheck，dev/ 不进版本库）以及 tmux(1) 的
// CONTROL MODE 章节 —— 没有一处是凭记忆写的。
package terminal
