package main

import "runtime/debug"

// 无副作用地读取 GC 参数，供两种构建标签的测试共用。
//
// 坑：debug.SetGCPercent(-1) 不是纯读取 —— 它把 GC 关掉并返回旧值。
// 直接拿它当 getter，第一个用例之后整个测试进程的 GC 就停了，
// 后面的用例在完全失真的环境里跑。读必须写成"读+放回"。

func gcPercent() int {
	prev := debug.SetGCPercent(-1)
	debug.SetGCPercent(prev)
	return prev
}

// SetMemoryLimit(-1) 是官方文档认可的无副作用查询。
func memLimit() int64 { return debug.SetMemoryLimit(-1) }
