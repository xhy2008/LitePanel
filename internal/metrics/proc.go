package metrics

import (
	"fmt"
	"os"
	"time"
)

// DefaultProcDir 是生产环境的内核接口挂载点。
// 采集函数一律接受 procDir 参数而不是硬编码它：否则没法做表驱动单测，
// 也没法在非 Linux 平台跑测试。
const DefaultProcDir = "/proc"

// ReadCPU 读取一次 /proc/stat 快照。
func ReadCPU(procDir string) (*CPUSample, error) {
	f, err := os.Open(procDir + "/stat")
	if err != nil {
		return nil, fmt.Errorf("metrics: 打开 %s/stat 失败: %w", procDir, err)
	}
	defer f.Close()
	return newCPUSample(f, time.Now())
}

// ReadLoadAvg 读取 1/5/15 分钟平均负载。
// 单独读而不是从 stat 推：loadavg 含不可中断进程，是使用率之外
// 唯一能反映"排队等 CPU"的信号。
func ReadLoadAvg(procDir string) ([3]float64, error) {
	f, err := os.Open(procDir + "/loadavg")
	if err != nil {
		return [3]float64{}, fmt.Errorf("metrics: 打开 %s/loadavg 失败: %w", procDir, err)
	}
	defer f.Close()
	return parseLoadAvg(f)
}
