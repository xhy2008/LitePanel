package metrics

import (
	"strings"
	"testing"
)

// 内核 3.14+ 的现代 /proc/meminfo 片段。
const meminfoModern = `MemTotal:        7664732 kB
MemFree:          820612 kB
MemAvailable:    3155276 kB
Buffers:            2180 kB
Cached:          2363512 kB
SwapCached:        26836 kB
SwapTotal:       6291452 kB
SwapFree:        3987520 kB
Dirty:               120 kB
`

// 老内核（<3.14）没有 MemAvailable。
const meminfoLegacy = `MemTotal:        3932160 kB
MemFree:          512000 kB
Buffers:          204800 kB
Cached:          1024000 kB
SwapTotal:             0 kB
SwapFree:              0 kB
`

// 设计 5.1：used = MemTotal - MemAvailable（比 MemFree 更符合直觉）。
// 这台机器上：7664732 - 3155276 = 4509456 kB。
func TestMemUsedIsTotalMinusAvailable(t *testing.T) {
	m, err := parseMemInfo(strings.NewReader(meminfoModern))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.Total, uint64(7664732*1024); got != want {
		t.Errorf("Total = %d, want %d", got, want)
	}
	if got, want := m.Used, uint64((7664732-3155276)*1024); got != want {
		t.Errorf("Used = %d, want %d", got, want)
	}
	// 2026-09-16 与 free -k 对照：free 报 used=4510860，本算法 4509456，
	// 差异来自 SReclaimable 的归类口径；面板按设计口径显示，可接受。
	wantPct := float64(7664732-3155276) / 7664732 * 100
	if diff(m.UsedPercent(), wantPct) > 1e-9 {
		t.Errorf("UsedPercent = %v, want %v", m.UsedPercent(), wantPct)
	}
}

// swap 也按同一口径：used = SwapTotal - SwapFree。
func TestMemSwap(t *testing.T) {
	m, err := parseMemInfo(strings.NewReader(meminfoModern))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.SwapTotal, uint64(6291452*1024); got != want {
		t.Errorf("SwapTotal = %d, want %d", got, want)
	}
	if got, want := m.SwapUsed, uint64((6291452-3987520)*1024); got != want {
		t.Errorf("SwapUsed = %d, want %d", got, want)
	}
}

// 老内核没有 MemAvailable：回落到 MemFree + Buffers + Cached。
// 这是 procps 的老口径，虽然偏乐观，但比直接把 MemFree 当可用强得多。
func TestMemFallsBackWithoutMemAvailable(t *testing.T) {
	m, err := parseMemInfo(strings.NewReader(meminfoLegacy))
	if err != nil {
		t.Fatal(err)
	}
	avail := uint64((512000 + 204800 + 1024000) * 1024)
	if got, want := m.Available, avail; got != want {
		t.Errorf("Available = %d, want %d (MemFree+Buffers+Cached)", got, want)
	}
	if got, want := m.Used, uint64(3932160*1024)-avail; got != want {
		t.Errorf("Used = %d, want %d", got, want)
	}
}

// swap 总量为 0 是常态（很多 VPS 与本机容器都不开 swap）。
// 除零会产出 NaN，NaN 传进 SVG 会让渲染链无声崩掉，必须挡住。
func TestMemSwapZeroTotalNoDivByZero(t *testing.T) {
	m, err := parseMemInfo(strings.NewReader(meminfoLegacy))
	if err != nil {
		t.Fatal(err)
	}
	if m.SwapTotal != 0 {
		t.Fatalf("夹具应描述无 swap, got %d", m.SwapTotal)
	}
	p := m.SwapPercent()
	if p != p {
		t.Error("SwapPercent 出现 NaN")
	}
	if p != 0 {
		t.Errorf("无 swap 时应为 0, got %v", p)
	}
}

// 主内存总量为 0（读到了被截断的 meminfo）时同样不许出 NaN。
func TestMemTotalZeroNoDivByZero(t *testing.T) {
	m, err := parseMemInfo(strings.NewReader("MemTotal: 0 kB\nMemFree: 0 kB\nMemAvailable: 0 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p := m.UsedPercent(); p != p || p != 0 {
		t.Errorf("MemTotal=0 时应为 0, got %v", p)
	}
}

// 百分比必须夹在 0–100。
// MemAvailable 理论上不会大于 MemTotal，但内核/容器 cgroup 场景下
// 见过越界值；越界值直接进 CSS width 会得到负宽度或超宽进度条。
func TestMemPercentClamped(t *testing.T) {
	m, err := parseMemInfo(strings.NewReader(`MemTotal:  1000 kB
MemFree:   100 kB
MemAvailable: 9000 kB
SwapTotal: 0 kB
SwapFree:  0 kB
`))
	if err != nil {
		t.Fatal(err)
	}
	if p := m.UsedPercent(); p < 0 || p > 100 {
		t.Errorf("UsedPercent 越界: %v", p)
	}
}

// MemAvailable > MemTotal（容器 / cgroup limit 场景真见过）时，
// used = total - available 的无符号减法会回绕成天文数字，
// 界面会显示「已用 18446744073 GB」。必须在减法处就夹住，
// 只夹百分比不够 —— used 还会作为字节数被单独显示。
func TestMemNoUnderflowWhenAvailableExceedsTotal(t *testing.T) {
	m, err := parseMemInfo(strings.NewReader(`MemTotal:  1000 kB
MemFree:      0 kB
MemAvailable: 5000 kB
SwapTotal:    0 kB
SwapFree:     0 kB
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Used > m.Total {
		t.Errorf("Used 回绕了: %d > Total %d", m.Used, m.Total)
	}
	if got := m.UsedPercent(); got != 0 {
		t.Errorf("available 超过 total 时应视为全空闲(0%%), got %v", got)
	}
}

// 缺 MemTotal 是致命畸形：必须报错而不是静默算出半个样本。
func TestMemRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"MemFree: 100 kB\n",       // 缺 MemTotal
		"MemTotal: abc kB\n",      // 非数字
		"MemTotal:\n",             // 只有字段名
		"Garbage line no colon\n", // 完全畸形
	} {
		if _, err := parseMemInfo(strings.NewReader(in)); err == nil {
			t.Errorf("%q 应报错", in)
		}
	}
}

// 单位：meminfo 的 kB 实际是 KiB（1024）。这里显式锁死，
// 防止有人日后按 1000 换算导致 12GB 机器显示成 11.4GB。
func TestMemUsesKiB(t *testing.T) {
	m, err := parseMemInfo(strings.NewReader("MemTotal: 1 kB\nMemFree: 0 kB\nMemAvailable: 0 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 1024 {
		t.Errorf("1 kB 应为 1024 字节, got %d", m.Total)
	}
}

// 真实读取入口（本机 /proc/meminfo 可读，可跑通）。
func TestReadMemFromDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "meminfo", meminfoModern)
	m, err := ReadMem(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Total == 0 {
		t.Error("Total 为 0")
	}
	if _, err := ReadMem(t.TempDir()); err == nil {
		t.Error("缺文件应报错")
	}
}
