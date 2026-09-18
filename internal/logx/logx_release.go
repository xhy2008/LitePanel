//go:build !debug

package logx

import "io"

// Enabled 为 false 时，下面的函数都是空函数体：调用点内联后什么都不剩，
// 连变参的接口装箱都不会发生（实参只在函数体内被使用才会装箱）。
// 这就是 §12.1 说的"零开销，不是运行时判断"。
const Enabled = false

// Init/SetLevel 保留可调用（业务代码与测试不分叉），但不产生任何效果。
func Init(io.Writer) {}
func SetLevel(Level) {}

func Debug(string, ...any) {}
func Info(string, ...any)  {}
func Warn(string, ...any)  {}
func Error(string, ...any) {}
