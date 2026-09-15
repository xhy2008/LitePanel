package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Mem 是一次内存采样。字段全为字节。
type Mem struct {
	Total     uint64
	Available uint64
	Used      uint64

	SwapTotal uint64
	SwapUsed  uint64
}

// parseMemInfo 解析 /proc/meminfo。
// 口径（设计 5.1）：used = MemTotal - MemAvailable。
// 老内核没有 MemAvailable 时回落到 MemFree + Buffers + Cached（procps 老口径）。
func parseMemInfo(r io.Reader) (Mem, error) {
	var (
		m        Mem
		scan     = bufio.NewScanner(r)
		gotTotal bool
		gotAvail bool
		// 回落口径需要的三项
		free, buffers, cached uint64
	)
	for scan.Scan() {
		line := scan.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // 不是 "key: value kB" 的行，忽略
		}
		v, ok := parseKiB(rest)
		if !ok {
			continue
		}
		switch name {
		case "MemTotal":
			m.Total, gotTotal = v, true
		case "MemAvailable":
			m.Available, gotAvail = v, true
		case "MemFree":
			free = v
		case "Buffers":
			buffers = v
		case "Cached":
			cached = v
		case "SwapTotal":
			m.SwapTotal = v
		case "SwapFree":
			if v > m.SwapTotal {
				v = m.SwapTotal
			}
			m.SwapUsed = m.SwapTotal - v
		}
	}
	if err := scan.Err(); err != nil {
		return Mem{}, fmt.Errorf("metrics: 读取 meminfo 失败: %w", err)
	}
	if !gotTotal {
		return Mem{}, errors.New("metrics: meminfo 缺少 MemTotal")
	}

	if !gotAvail {
		m.Available = free + buffers + cached
	}
	m.Used = subOrZero(m.Total, m.Available)
	return m, nil
}

// parseKiB 解析 "  1234 kB"。meminfo 的单位一律省略 kB 之外的值；
// 单位缺失或非法时返回 ok=false 由调用方忽略该行，而不是当成 0。
func parseKiB(rest string) (uint64, bool) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	// 内核的 "kB" 实际是 KiB。按 1000 换算会让 12GB 的机器显示成 11.4GB。
	if len(fields) > 1 && fields[1] != "kB" {
		return 0, false
	}
	return n * 1024, true
}

// UsedPercent 是已用内存占比，夹在 0–100。
func (m Mem) UsedPercent() float64 { return percent(m.Used, m.Total) }

// SwapPercent 是 swap 占比；无 swap 时为 0（绝不除零出 NaN）。
func (m Mem) SwapPercent() float64 { return percent(m.SwapUsed, m.SwapTotal) }

func subOrZero(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// percent 在分母为 0 时返回 0 而不是 NaN。
// NaN 会一路传到 SVG 的 stroke-dasharray / CSS width，让整条渲染链无声崩掉。
func percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	p := float64(used) / float64(total) * 100
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// ReadMem 读取一次 <procDir>/meminfo。
func ReadMem(procDir string) (Mem, error) {
	f, err := os.Open(procDir + "/meminfo")
	if err != nil {
		return Mem{}, fmt.Errorf("metrics: 打开 %s/meminfo 失败: %w", procDir, err)
	}
	defer f.Close()
	return parseMemInfo(f)
}
