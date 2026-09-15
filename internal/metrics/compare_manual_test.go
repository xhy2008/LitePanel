package metrics

import (
	"os"
	"testing"
	"time"
)

// 人工对照用（非 CI 断言）：在真实 /proc 上连续两次采样，
// 打印结果供与 top 比对。需要真实 /proc 且间隔足够，故默认跳过，
// 用 `go test -run TestManualCompareWithTop -tags manual` 手动触发。
// 这里用环境变量而不是 build tag，避免 Makefile 里漏掉它。
func TestManualCompareWithTop(t *testing.T) {
	if os.Getenv("LITEPANEL_MANUAL") == "" {
		t.Skip("人工对照：LITEPANEL_MANUAL=1 go test ./internal/metrics/ -run Compare -v")
	}
	a, err := ReadCPU(DefaultProcDir)
	if err != nil {
		t.Skip("无 /proc/stat:", err)
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
	t.Logf("面板算出: 总利用率 %.1f%%  load=%.2f/%.2f/%.2f  核数=%d", total, l[0], l[1], l[2], len(b.cores))
	for i, c := range b.PerCoreSince(a) {
		t.Logf("  cpu%d %.1f%%", i, c)
	}
}
