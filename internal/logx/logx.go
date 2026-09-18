// Package logx 是面板唯一的运行日志出口（设计方案 §12.1 / D9）。
//
// 两种构建标签提供两套实现：
//
//	go build -tags debug   → 结构化日志输出到 stderr，级别可调
//	go build               → 所有调用编译为空操作（不是运行时判断）
//
// release 侧的 const Enabled = false 让调用点内联成空函数，
// 挂载点（如访问日志中间件）整段被编译器消除 —— 变参求值、
// 字符串拼接、接口装箱都不会发生在发布产物里。
//
// 例外（R4，刻意保留在 logx 之外）：初始密码、启动失败、panic
// 仍由 main 直接写 stderr —— 那些行不是运行日志，缺了面板就没法
// 首次登录 / systemd 下启动失败会一片空白。
package logx

// Level 是 debug 构建里的日志级别；release 构建中没有任何东西读它们，
// 常量仍导出以便业务代码不分叉地调用 SetLevel。
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)
