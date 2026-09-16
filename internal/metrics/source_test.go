package metrics

import (
	"math"
	"testing"
	"time"
)

// fakeProc 造一个假 /proc：只放采集器真正会读的那几个文件。
func fakeProc(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		writeFile(t, dir, name, content)
	}
	return dir
}

// 两帧 /proc/stat：第二次采样才有差分基线，所以首轮必须 warming。
const statFrame1 = `cpu  100 0 100 800 0 0 0 0 0 0
cpu0 50 0 50 400 0 0 0 0 0 0
cpu1 50 0 50 400 0 0 0 0 0 0
intr 0
`

const statFrame2 = `cpu  150 0 150 1700 0 0 0 0 0 0
cpu0 75 0 75 850 0 0 0 0 0 0
cpu1 75 0 75 850 0 0 0 0 0 0
intr 0
`

func TestSystemSourceFirstFrameWarming(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"stat":      statFrame1,
		"loadavg":   "0.50 0.40 0.30 1/100 1234\n",
		"meminfo":   "MemTotal: 1000000 kB\nMemAvailable: 500000 kB\n",
		"mounts":    "/dev/sda2 / ext4 rw 0 0\n",
		"diskstats": "",
	})
	src := NewSystemSource(dir)

	snap, warming, err := src.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !warming {
		t.Error("第一帧无 CPU 差分基线，应标 warming")
	}
	// 关键：warming 时 CPU.percent 必须是 null，不能是 0。
	// 0 会被前端画成"系统完全空闲"，比 -- 更误导。
	if snap.CPU == nil {
		t.Fatal("CPU 段不能为 nil")
	}
	if snap.CPU.Percent != nil {
		t.Errorf("无基线时 percent 应为 null, got %v", *snap.CPU.Percent)
	}
	// 但内存/磁盘是全量读数，第一帧就该有值。
	if snap.Mem == nil || snap.Mem.Total != 1000000*1024 {
		t.Errorf("内存应第一帧就有值, got %+v", snap.Mem)
	}
}

func TestSystemSourceCPUDiffSecondFrame(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"stat": statFrame1, "loadavg": "0.5 0.4 0.3 1/1 1\n",
		"meminfo": "MemTotal: 1000 kB\nMemAvailable: 500 kB\n",
		"mounts":  "/dev/sda2 / ext4 rw 0 0\n",
	})
	src := NewSystemSource(dir)

	if _, w, err := src.Sample(); err != nil || !w {
		t.Fatalf("first: warming=%v err=%v", w, err)
	}
	writeFile(t, dir, "stat", statFrame2)
	snap, warming, err := src.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if warming {
		t.Error("第二帧不应再 warming")
	}
	// 手工算（与 cpu_test.go 用的是同一组夹具）：
	// 帧1 total=1000 idle=800；帧2 total=2000 idle=1700
	// => (Δtotal-Δidle)/Δtotal = (1000-900)/1000 = 10%
	if snap.CPU.Percent == nil {
		t.Fatal("第二帧应有 percent")
	}
	if d := diff(*snap.CPU.Percent, 10.0); d > 0.001 {
		t.Errorf("CPU = %v, want 10 (差 %v)", *snap.CPU.Percent, d)
	}
	if len(snap.CPU.Cores) != 2 {
		t.Errorf("应 2 个核心, got %d", len(snap.CPU.Cores))
	}
	if snap.CPU.CoresTotal != 2 {
		t.Errorf("cores_total = %d, want 2", snap.CPU.CoresTotal)
	}
	// 核心数必须与 cores 数组一致，否则前端按 cores_total 循环会越界。
	if len(snap.CPU.Cores) != snap.CPU.CoresTotal {
		t.Errorf("cores(%d) 与 cores_total(%d) 不一致",
			len(snap.CPU.Cores), snap.CPU.CoresTotal)
	}
	if snap.CPU.Load1 != 0.5 {
		t.Errorf("load1 = %v, want 0.5", snap.CPU.Load1)
	}
}

// Reset 后必须重新回到 warming：
// 否则恢复采样的第一帧会拿"停表期间"的累计差分，
// 算出一个跨了很久的平均值，读数会离谱地偏高或偏低。
func TestSystemSourceResetClearsBaseline(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"stat": statFrame1, "loadavg": "0 0 0 0 0\n",
		"meminfo": "MemTotal: 1000 kB\nMemAvailable: 500 kB\n",
		"mounts":  "",
	})
	src := NewSystemSource(dir)
	src.Sample()
	writeFile(t, dir, "stat", statFrame2)
	src.Sample()

	src.Reset()
	_, warming, err := src.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !warming {
		t.Error("Reset 后第一帧应重新 warming")
	}
}

// CPU 读不到时整轮不能失败：loadavg 权限、stat 权限在 Android/容器里
// 都会拒绝。此时仍要出内存和磁盘，面板才不至于整块空白。
func TestSystemSourcePartialFailure(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"meminfo": "MemTotal: 2000 kB\nMemAvailable: 1000 kB\n",
		"mounts":  "/dev/sda2 / ext4 rw 0 0\n",
		// 故意不放 stat / loadavg
	})
	src := NewSystemSource(dir)
	snap, _, err := src.Sample()
	if err != nil {
		t.Fatalf("CPU 缺失不应让整轮失败: %v", err)
	}
	if snap.CPU == nil || snap.CPU.Percent != nil {
		t.Errorf("CPU 应降级为 null, got %+v", snap.CPU)
	}
	if snap.Mem == nil {
		t.Error("内存仍应有值")
	}
}

func TestSystemSourceImplementsResetter(t *testing.T) {
	var src Source = NewSystemSource(".")
	if _, ok := src.(OptionalResetter); !ok {
		t.Error("SystemSource 必须实现 Reset，否则停表后基线会污染读数")
	}
}

// 磁盘条数量必须等于过滤后的挂载点数，且各字段齐全。
func TestSystemSourceDisksPopulated(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"stat": statFrame1, "loadavg": "0 0 0 0 0\n",
		"meminfo": "MemTotal: 1000 kB\nMemAvailable: 500 kB\n",
		// /proc/mounts 里的路径在测试机上不存在也没关系：
		// statfs 失败的条目应被跳过而不是报错。
		"mounts": "/dev/sda2 / ext4 rw 0 0\n/dev/sdb1 /DISK ext4 rw 0 0\n",
	})
	src := NewSystemSource(dir)
	snap, _, err := src.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Disks) == 0 {
		t.Fatal("至少应有根分区一条")
	}
	for _, d := range snap.Disks {
		if d.Mountpoint == "" || d.Device == "" {
			t.Errorf("磁盘条目字段缺失: %+v", d)
		}
		if d.Total == 0 {
			t.Errorf("%s Total=0", d.Mountpoint)
		}
		if math.IsNaN(d.Percent) || math.IsInf(d.Percent, 0) {
			t.Errorf("%s Percent 非有限值: %v", d.Mountpoint, d.Percent)
		}
		if d.Percent < 0 || d.Percent > 100 {
			t.Errorf("%s Percent 越界: %v", d.Mountpoint, d.Percent)
		}
	}
	// 根分区一定在（statfs 一定能成功）。
	found := false
	for _, d := range snap.Disks {
		if d.Mountpoint == "/" {
			found = true
		}
	}
	if !found {
		t.Error("根分区必须出现在磁盘列表里")
	}
}

// D6 的真实验证：不 mock，直接把 SystemSource 接到 /proc，
// 数出 0 订阅期间到底发生了多少次真实系统读取。
// 期望恰好为 0 —— 一次读取都没有，才说明这台机器真的没在替
// 没有人看的仪表干活。
func TestD6ZeroReadsWithNoSubscribers(t *testing.T) {
	var probes int
	counter := countSource{
		inner: NewSystemSource(DefaultProcDir),
		count: func() { probes++ },
	}
	c := NewCollector(counter, &fakeBC{}, 5*time.Millisecond)
	defer c.Stop()

	time.Sleep(80 * time.Millisecond)
	if probes != 0 {
		t.Fatalf("0 订阅期间真实读取 /proc %d 次，应为 0", probes)
	}

	// 真订阅之后必须真的能读出这一台机器的真实数据。
	c.SetSubscribers(1)
	deadline := time.After(3 * time.Second)
	for {
		if snap := c.Latest(); snap != nil {
			if snap.Mem == nil || snap.Mem.Total == 0 {
				t.Fatalf("真实 /proc 内存读不出来: %+v", snap.Mem)
			}
			t.Logf("真实快照: 内存 %.0f%% 磁盘%d项 CPU=%v",
				snap.Mem.Percent, len(snap.Disks), snap.CPU.Percent)
			break
		}
		select {
		case <-deadline:
			t.Fatal("订阅 3 秒后仍无快照")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// 停表后真实读取次数必须不再增长。
	c.SetSubscribers(0)
	settled := probes
	time.Sleep(80 * time.Millisecond)
	if probes != settled {
		t.Fatalf("退订后仍在读 /proc: %d → %d", settled, probes)
	}
	t.Logf("全程真实采样次数 = %d（订阅期间）", probes)
}

// countSource 给内层 Source 的调用计数，用来验证 D6 在真实实现上成立。
type countSource struct {
	inner Source
	count func()
}

func (s countSource) Sample() (Snapshot, bool, error) {
	s.count()
	return s.inner.Sample()
}

func (s countSource) Reset() {
	if r, ok := s.inner.(OptionalResetter); ok {
		r.Reset()
	}
}
