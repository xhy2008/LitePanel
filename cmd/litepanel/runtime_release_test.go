//go:build !debug

package main

import (
	"os"
	"runtime/debug"
	"testing"
)

// 发布构建的运行时资源默认值 —— 用户 2026-07 增补要求：
// release = 运行时资源占用最少的优化构建。
//
// GOMEMLIMIT 是保险丝：设计 20 节内存上限 40MB，64MiB 留 1.6 倍余量，
// 万一后续里程碑把内存跑飞，GC 会疯狂回收而不是把 12GB 的宿主机拖死。
// GOGC=50 压低稳态堆（实测收益小，但代价为零，且在 M6 的 10 万文件
// 目录页场景优于默认值时的堆翻倍行为）。
//
// 关于"环境变量优先"的可测边界：GOGC/GOMEMLIMIT 由 Go 运行时在启动时
// 原生消费，本包的契约只是 **环境变量存在时不去覆盖**。t.Setenv 无法
// 让运行时重新走一遍启动逻辑，所以"env 真的生效"属于运行时职责，
// 这里测的是 tuneRuntime 的另一半：看见了就住手。
//
// （另一个坑：debug.SetGCPercent(-1) 不是纯读取 —— 它会把 GC 关掉再返回
// 旧值。所有读取都必须把值放回去，否则本文件第一个用例之后整个测试
// 进程 GC 就停了，后面的用例在完全失真的环境里跑。）

func TestReleaseRuntimeTuningDefaults(t *testing.T) {
	osUnsetForTest(t, "GOGC")
	osUnsetForTest(t, "GOMEMLIMIT")

	tuneRuntime()

	if got := gcPercent(); got != 50 {
		t.Errorf("默认 GOGC 应为 50, got %d", got)
	}
	if got := memLimit(); got != 64<<20 {
		t.Errorf("默认 GOMEMLIMIT 应为 64MiB, got %d", got)
	}
}

// 环境变量存在（哪怕值与默认完全不同）时 tuneRuntime 必须不动运行时，
// 让 systemd 单元里的调参不被二进制写死的值覆盖。
func TestReleaseRuntimeTuningLeavesEnvAlone(t *testing.T) {
	t.Setenv("GOGC", "99")
	t.Setenv("GOMEMLIMIT", "99MiB")

	// 模拟"运行时已按环境变量配置过"的状态：直接写进运行时，
	// 然后要求 tuneRuntime 看见环境变量后住手。
	oldGC := debug.SetGCPercent(77)
	oldLim := debug.SetMemoryLimit(99 << 20)
	t.Cleanup(func() { debug.SetGCPercent(oldGC); debug.SetMemoryLimit(oldLim) })

	tuneRuntime()

	if got := gcPercent(); got != 77 {
		t.Errorf("GOGC 环境变量存在时不应覆盖运行时: %d → %d", 77, got)
	}
	if got := memLimit(); got != 99<<20 {
		t.Errorf("GOMEMLIMIT 环境变量存在时不应覆盖运行时: %d → %d", 99<<20, got)
	}
}

// osUnsetForTest 真正删除环境变量并在用例结束后复原
// （t.Setenv("") 只是设成空串，删除需要自己动手）。
func osUnsetForTest(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
