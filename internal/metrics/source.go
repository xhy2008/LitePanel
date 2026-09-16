package metrics

import (
	"sync"
)

// SystemSource 把 CPU / 内存 / 磁盘组装成一个 Snapshot。
//
// 设计要点是"分项降级"：任何一个子系统读不到，只把该项降级成 null，
// 其余照常出数。在容器与 Android 上 /proc/stat、/proc/loadavg 常被
// SELinux 拒绝（本机实测就是如此），若"一项失败整轮失败"，
// 面板仪表会整块空白，而实际上内存和磁盘数据是完全可得的。
type SystemSource struct {
	procDir string

	// mu 保护 prev：Sample 跑在采集循环里，而 Reset 由 hub 的订阅计数
	// 回调触发（另一个 goroutine）。二者并发访问 prev 会读到半个基线，
	// 差分结果可能是任意值 —— 而且这种错误只在"恰好取消订阅"时出现，
	// 几乎无法复现，所以这里必须显式加锁而不是假定单线程。
	mu   sync.Mutex
	prev *CPUSample
}

func NewSystemSource(procDir string) *SystemSource {
	return &SystemSource{procDir: procDir}
}

// Sample 采一整轮。返回值 err 只在"连内存都拿不到"时才非 nil ——
// 那种情况下这台机器根本没有可显示的指标，让采集循环记日志后下一轮重试。
func (s *SystemSource) Sample() (Snapshot, bool, error) {
	cpu, warming, err := s.sampleCPU()
	if err != nil {
		return Snapshot{}, false, err
	}
	mem, err := ReadMem(s.procDir)
	if err != nil {
		return Snapshot{}, false, err
	}

	// 磁盘失败（mounts 读不到、全部挂载点被卸载）降级成空列表：
	// 少几条进度条比整块指标消失好。
	disks, _ := Disks(s.procDir)

	return Snapshot{
		CPU:   cpu,
		Mem:   toMemStat(mem),
		Disks: toDiskStats(disks),
	}, warming, nil
}

// sampleCPU 返回 CPU 段与 warming 标记。
// stat 读不到时仍返回非 nil 的 CPU 段（字段全为降级值），
// 这样前端不必区分"这一项不存在"和"这一项取不到"。
func (s *SystemSource) sampleCPU() (*CPUStat, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stat := &CPUStat{}

	cur, statErr := ReadCPU(s.procDir)
	if statErr != nil {
		// 保留 prev：若本轮只是偶发失败，下一轮仍能拿旧基线差分。
		// 但绝不能拿"隔了很久的 prev"算出一个跨度不明的平均值来显示 ——
		// 所以这里直接标 warming，让前端显示 --。
		return s.fillLoad(stat), true, nil
	}

	pct, ok := cur.UsageSince(s.prev)
	if ok {
		stat.Percent = &pct
		stat.Cores = cur.PerCoreSince(s.prev)
	}
	// 核数始终照实报，即使本轮没法差分。前端按 cores 数组画小格，
	// cores_total 只用于"16 核"这类文字，不用于索引。
	stat.CoresTotal = len(cur.cores)
	s.prev = cur

	return s.fillLoad(stat), !ok, nil
}

// fillLoad 补 loadavg。读失败留 0：负载这一项没有差分语义，
// 拿不到时 0 与"未知"在图上难以区分，但设计里负载只是辅助读数。
func (s *SystemSource) fillLoad(stat *CPUStat) *CPUStat {
	if l, err := ReadLoadAvg(s.procDir); err == nil {
		stat.Load1, stat.Load5, stat.Load15 = l[0], l[1], l[2]
	}
	return stat
}

// Reset 丢掉差分基线，由采集器停表时调用。
// 不停清的话，恢复后的第一帧会拿"停表期间"的累计差分，
// 算出一个跨了几分钟甚至几小时的平均值，读数会离谱地失真。
func (s *SystemSource) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prev = nil
}

func toMemStat(m Mem) *MemStat {
	return &MemStat{
		Total:     m.Total,
		Available: m.Available,
		Used:      m.Used,
		Percent:   m.UsedPercent(),
		SwapTotal: m.SwapTotal,
		SwapUsed:  m.SwapUsed,
		SwapPct:   m.SwapPercent(),
	}
}

func toDiskStats(us []Usage) []DiskStat {
	if len(us) == 0 {
		return nil
	}
	out := make([]DiskStat, 0, len(us))
	for _, u := range us {
		out = append(out, DiskStat{
			Mountpoint: u.Mountpoint,
			Device:     u.Device,
			FSType:     u.FSType,
			Total:      u.Total,
			Used:       u.Used,
			Free:       u.Free,
			Percent:    u.UsedPercent(),
		})
	}
	return out
}
