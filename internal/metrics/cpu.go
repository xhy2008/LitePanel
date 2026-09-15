package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cpuTimes 是 /proc/stat 里一行 cpu 计数（jiffies 累计值）。
// 只存原始累计量，利用率一律靠两次采样差分算出来 —— 累计量本身没有意义。
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

// total 是参与"利用率"分母的总和；idle 是其中的空闲部分（idle + iowait）。
// 注意 iowait 计入空闲：这是 top/procps 的口径，跟着它走才能和 top 对上。
func (t cpuTimes) total() uint64 {
	return t.user + t.nice + t.system + t.idle + t.iowait + t.irq + t.softirq + t.steal
}

func (t cpuTimes) idleAll() uint64 { return t.idle + t.iowait }

// CPUSample 是一次 /proc/stat 快照。
type CPUSample struct {
	at    time.Time
	total cpuTimes
	cores []cpuTimes // 按 cpuN 的数字下标升序
}

// ErrNoBaseline 表示缺少上一次采样，无法差分。
// 上层据此显示 “--”（warming），而不是显示 0 —— 0 会被读成“系统空闲”，
// 比一个明确的未知更误导人。
var ErrNoBaseline = errors.New("metrics: 无差分基线")

// parseCPUSample 从 io.Reader 读 /proc/stat。
// 刻意接受 Reader 而不是路径：单测才能喂固定文本做表驱动断言。
func parseCPUSample(r io.Reader) (*CPUSample, error) {
	return newCPUSample(r, time.Time{})
}

func newCPUSample(r io.Reader, at time.Time) (*CPUSample, error) {
	s := &CPUSample{}
	var (
		gotAll  bool
		cores   = map[int]cpuTimes{}
		sc      = bufio.NewScanner(r)
		maxLine = 1 << 16
	)
	buf := make([]byte, 0, 4096)
	sc.Buffer(buf, maxLine)

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue // intr/ctxt/processes 等行与我们无关
		}
		name, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		t, err := parseTimes(rest)
		if err != nil {
			return nil, fmt.Errorf("metrics: 解析 %s 失败: %w", name, err)
		}
		if name == "cpu" {
			s.total = t
			gotAll = true
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(name, "cpu"))
		if err != nil {
			// 不是 cpuN（例如内核将来加了新前缀），忽略而不是报错。
			continue
		}
		cores[idx] = t
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("metrics: 读取 /proc/stat 失败: %w", err)
	}
	if !gotAll {
		return nil, errors.New("metrics: /proc/stat 缺少 cpu 聚合行")
	}

	// 按下标排序：核号可能不连续（CPU 热插拔、cgroup 限制），
	// 但顺序必须稳定，否则前端每帧的核序会跳。
	idxs := make([]int, 0, len(cores))
	for i := range cores {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		s.cores = append(s.cores, cores[i])
	}

	s.at = at
	return s, nil
}

// parseTimes 解析一行里的计数。老内核字段更少（最少 4 个），
// 缺的按 0 处理；出现非数字则报错，绝不静默当 0 ——
// 静默的 0 会把"采集坏了"伪装成"系统很忙/很闲"。
func parseTimes(rest string) (cpuTimes, error) {
	var t cpuTimes
	fields := strings.Fields(rest)
	if len(fields) < 4 {
		return t, fmt.Errorf("字段不足（%d 个）", len(fields))
	}
	dst := []*uint64{&t.user, &t.nice, &t.system, &t.idle, &t.iowait, &t.irq, &t.softirq, &t.steal}
	for i, p := range dst {
		if i >= len(fields) {
			break
		}
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return t, fmt.Errorf("第 %d 列 %q 不是数字", i+1, fields[i])
		}
		*p = v
	}
	return t, nil
}

// UsageSince 返回自 prev 以来的整机利用率百分比（0–100）。
// ok=false 表示应当显示 “--”：没有基线、差分为零或差分为负。
func (s CPUSample) UsageSince(prev *CPUSample) (float64, bool) {
	if prev == nil {
		return 0, false
	}
	return usageOf(prev.total, s.total)
}

// PerCoreSince 返回每核利用率，顺序与采样时的核序一致。
// 任何一核不可用就整体报不可用（返回空）：半截数据比没有数据更难排查。
func (s CPUSample) PerCoreSince(prev *CPUSample) []float64 {
	if prev == nil || len(prev.cores) != len(s.cores) {
		return nil
	}
	out := make([]float64, 0, len(s.cores))
	for i := range s.cores {
		v, ok := usageOf(prev.cores[i], s.cores[i])
		if !ok {
			return nil
		}
		out = append(out, v)
	}
	return out
}

// usageOf 做一次差分。
// 差分为 0（两次采样同一时刻）或为负（读到更旧的数据、文本被截断）时
// 返回 ok=false。这里绝不能产出 NaN：NaN 会一路传到 SVG 的
// stroke-dasharray，让整条渲染链无声崩掉。
func usageOf(prev, cur cpuTimes) (float64, bool) {
	ct, pt := cur.total(), prev.total()
	if ct <= pt {
		return 0, false
	}
	dTotal := float64(ct - pt)
	var dIdle uint64
	if cur.idleAll() > prev.idleAll() {
		dIdle = cur.idleAll() - prev.idleAll()
	}
	usage := (dTotal - float64(dIdle)) / dTotal * 100
	if usage < 0 {
		usage = 0
	}
	if usage > 100 {
		usage = 100
	}
	return usage, true
}

// parseLoadAvg 解析 /proc/loadavg 的前三个数（1/5/15 分钟）。
func parseLoadAvg(r io.Reader) ([3]float64, error) {
	var out [3]float64
	b, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return out, fmt.Errorf("metrics: 读取 /proc/loadavg 失败: %w", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return out, fmt.Errorf("metrics: /proc/loadavg 字段不足（%d 个）", len(fields))
	}
	for i := range out {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return out, fmt.Errorf("metrics: /proc/loadavg 第 %d 个 %q 不是数字", i+1, fields[i])
		}
		out[i] = v
	}
	return out, nil
}
