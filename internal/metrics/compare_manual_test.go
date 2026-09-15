package metrics

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 人工对照（非 CI 断言）：把面板算法与 free / top 的真实输出放一起打印，
// 用于 M2 验收里的“与 top/free 对照”。默认跳过，避免 CI 依赖外部命令。
//
//	LITEPANEL_MANUAL=1 go test ./internal/metrics/ -run Compare -v
func TestManualCompareWithTop(t *testing.T) {
	if os.Getenv("LITEPANEL_MANUAL") == "" {
		t.Skip("人工对照：LITEPANEL_MANUAL=1 go test ./internal/metrics/ -run Compare -v")
	}

	m, err := ReadMem(DefaultProcDir)
	if err != nil {
		t.Logf("无 meminfo: %v", err)
		return
	}
	t.Logf("面板: used=%.0fMB (%.1f%%)  avail=%.0fMB  swap_used=%.0fMB/%.0fMB",
		mb(m.Used), m.UsedPercent(), mb(m.Available), mb(m.SwapUsed), mb(m.SwapTotal))

	out, ferr := exec.Command("free", "-k").Output()
	if ferr != nil {
		t.Log("无 free 命令，跳过对照")
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// Mem: 行是 7 列（label total used free shared buff/cache available），
		// Swap: 行只有 4 列。按下标盲取会 panic，必须先验长度。
		switch {
		case len(f) == 7 && f[0] == "Mem:":
			total := num(f[1])
			used := num(f[2])
			t.Logf("free  Mem: used=%.0fMB (%.1f%%)  avail=%.0fMB",
				mb(kib(used)), used/total*100, mb(kib(num(f[6]))))
			du := mb(m.Used) - mb(kib(used))
			if du < 0 {
				du = -du
			}
			// free 把 SReclaimable 也算进 available，口径略有差；应在 3% 内。
			tol := du / mb(kib(total)) * 100
			if tol > 3 {
				t.Errorf("内存 used 与 free 偏差 %.2f%% > 3%%", tol)
			} else {
				t.Logf("→ 内存偏差 %.2f%%（≤3%%，属口径差）", tol)
			}
		case len(f) == 4 && f[0] == "Swap:":
			t.Logf("free  Swap: used=%.0fMB  total=%.0fMB", mb(kib(num(f[2]))), mb(kib(num(f[1]))))
		}
	}

	a, err := ReadCPU(DefaultProcDir)
	if err != nil {
		t.Logf("CPU 无法本机对照（Android SELinux 拒绝 /proc/stat）: %v", err)
		return
	}
	time.Sleep(2 * time.Second)
	b, err := ReadCPU(DefaultProcDir)
	if err != nil {
		t.Fatal(err)
	}
	total, ok := b.UsageSince(a)
	if !ok {
		t.Fatal("warming")
	}
	l, _ := ReadLoadAvg(DefaultProcDir)
	t.Logf("面板: CPU %.1f%%  load=%.2f/%.2f/%.2f  核数=%d", total, l[0], l[1], l[2], len(b.cores))
}

// 字节 → MB。
func mb(b uint64) float64 { return float64(b) / 1024 / 1024 }

// free -k 的列单位是 KiB → 字节。
func kib(v float64) uint64 { return uint64(v) * 1024 }

func num(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
