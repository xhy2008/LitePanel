package metrics

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 两段固定的 /proc/stat。字段顺序严格按 Linux 文档：
// user nice system idle iowait irq softirq steal guest guest_nice
const statT0 = `cpu  1000 100 200 6000 50 10 20 0 0 0
cpu0 500 50 100 3000 25 5 10 0 0 0
cpu1 500 50 100 3000 25 5 10 0 0 0
intr 12345
ctxt 6789
`

const statT1 = `cpu  1200 100 300 6300 50 10 20 20 0 0
cpu0 600 50 150 3160 25 5 10 10 0 0
cpu1 600 50 150 3140 25 5 10 10 0 0
intr 12999
ctxt 7000
`

// 手算（表 T0→T1）：
//
//	total_delta = (1200+100+300+6300+50+10+20+20) - (1000+100+200+6000+50+10+20+0)
//	            = 8000 - 7380 = 620
//	idle_delta  = (6300+50) - (6000+50) = 300          // idle + iowait
//	usage       = (620-300)/620 = 320/620 = 51.612903…%
//
// cpu0: delta=(600+50+150+3160+25+5+10+10)-(500+50+100+3000+25+5+10+0)=4010-3690=320
//
//	idle=(3160+25)-(3000+25)=160 → (320-160)/320 = 50%
//
// cpu1: delta=(600+50+150+3140+25+5+10+10)-3690=3990-3690=300
//
//	idle=(3140+25)-(3000+25)=140 → (300-140)/300 = 53.333333%
func TestCPUDiffMatchesHandComputed(t *testing.T) {
	s0, err := parseCPUSample(strings.NewReader(statT0))
	if err != nil {
		t.Fatal(err)
	}
	s1, err := parseCPUSample(strings.NewReader(statT1))
	if err != nil {
		t.Fatal(err)
	}

	total, ok := s1.UsageSince(s0)
	if !ok {
		t.Fatal("有差分基线时应可用")
	}
	if got, want := total, 51.61290322580645; diff(got, want) > 1e-6 {
		t.Errorf("总利用率 = %.8f, want %.8f", got, want)
	}

	cores := s1.PerCoreSince(s0)
	if len(cores) != 2 {
		t.Fatalf("应解析出 2 个核, got %d", len(cores))
	}
	if got, want := cores[0], 50.0; diff(got, want) > 1e-6 {
		t.Errorf("cpu0 = %.6f, want %.2f", got, want)
	}
	if got, want := cores[1], 53.333333333333336; diff(got, want) > 1e-6 {
		t.Errorf("cpu1 = %.6f, want %.6f", got, want)
	}
}

// 首次采样没有基线：必须报 warming，绝不能给 NaN 或 0。
// 0 会被前端画成"空闲"，比 -- 更误导。
func TestCPUFirstSampleIsWarming(t *testing.T) {
	s, err := parseCPUSample(strings.NewReader(statT0))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.UsageSince(nil); ok {
		t.Error("无基线时应不可用")
	}
}

// 两次采样落在同一时刻（tick 抖动、时钟回拨）时差分为 0。
// 除零会产出 NaN，NaN 一路传到 SVG 的 stroke-dasharray 会让整条渲染链崩掉。
func TestCPUZeroIntervalIsNotNan(t *testing.T) {
	s0, _ := parseCPUSample(strings.NewReader(statT0))
	s1, _ := parseCPUSample(strings.NewReader(statT0))

	total, ok := s1.UsageSince(s0)
	if ok {
		t.Error("差分为 0 时应报不可用（warming）")
	}
	// 即便可用也绝不能是 NaN。
	if ok && total != total {
		t.Error("出现 NaN")
	}
	// 每核同样不许出 NaN。
	for i, v := range s1.PerCoreSince(s0) {
		if v != v {
			t.Errorf("cpu%d 为 NaN", i)
		}
	}
}

// 差分为负（读到的内容比基线旧，例如换了 /proc 挂载或文本被截断）时
// 必须报不可用，而不是算出负百分比。
func TestCPUNegativeDeltaIsRejected(t *testing.T) {
	s1, _ := parseCPUSample(strings.NewReader(statT1))
	s0, _ := parseCPUSample(strings.NewReader(statT0))

	if v, ok := s0.UsageSince(s1); ok && v < 0 {
		t.Errorf("负差分应报不可用, got %v", v)
	}
}

// /proc/loadavg：三个浮点数 + 运行/总进程数 + 最后 pid。
func TestLoadAvgParse(t *testing.T) {
	cases := []struct {
		in   string
		want [3]float64
	}{
		{"0.52 0.48 0.39 1/389 12345\n", [3]float64{0.52, 0.48, 0.39}},
		{"0.00 0.01 0.05 2/100 9\n", [3]float64{0, 0.01, 0.05}},
		{"12.34 5.67 0.00 17/210 99999\n", [3]float64{12.34, 5.67, 0}},
	}
	for _, c := range cases {
		got, err := parseLoadAvg(strings.NewReader(c.in))
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("%q => %v, want %v", c.in, got, c.want)
		}
	}
}

// 残缺或畸形的 loadavg 必须报错而不是静默给 0：
// 静默的 0 会让前端显示"负载正常"，掩盖真实的采集故障。
func TestLoadAvgRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "0.1 0.2\n", "a b c 1/2 3\n", "   \n"} {
		if _, err := parseLoadAvg(strings.NewReader(in)); err == nil {
			t.Errorf("%q 应报错", in)
		}
	}
}

// /proc/stat 文本畸形（缺行、字段不足）时必须报错，不能返回半个样本。
func TestCPUParseRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"intr 1 2 3\n",           // 没有 cpu 聚合行
		"cpu  1 2 3\n",           // 字段不足
		"cpu  a b c d e f g h\n", // 非数字
	} {
		if _, err := parseCPUSample(strings.NewReader(in)); err == nil {
			t.Errorf("%q 应报错", in)
		}
	}
}

// 采样间隔由采集器提供；这里只验证样本自身带时间戳，
// 否则跨 tick 的差分无法判断新鲜度。
func TestCPUSampleCarriesTimestamp(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s, err := newCPUSample(strings.NewReader(statT0), now)
	if err != nil {
		t.Fatal(err)
	}
	if !s.at.Equal(now) {
		t.Errorf("时间戳 = %v, want %v", s.at, now)
	}
}

func diff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// 真实读文件路径：文件不存在时必须报错并带上目录，
// 否则在非 Linux 上跑起来只会看到一个没头没尾的 panic。
func TestReadCPUFromDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "stat", statT0)
	writeFile(t, dir, "loadavg", "1.25 0.50 0.10 2/300 42\n")

	s, err := ReadCPU(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.cores) != 2 {
		t.Errorf("cores = %d, want 2", len(s.cores))
	}
	if s.at.IsZero() {
		t.Error("从真实文件读取应带时间戳")
	}

	l, err := ReadLoadAvg(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l != [3]float64{1.25, 0.50, 0.10} {
		t.Errorf("loadavg = %v", l)
	}

	missing := t.TempDir()
	if _, err := ReadCPU(missing); err == nil {
		t.Error("缺文件应报错")
	} else if !strings.Contains(err.Error(), missing) {
		t.Errorf("错误里应含目录名便于定位, got %v", err)
	}
	if _, err := ReadLoadAvg(missing); err == nil {
		t.Error("缺文件应报错")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
