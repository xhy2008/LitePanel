package metrics

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Mount 是 /proc/mounts 的一行。
type Mount struct {
	Device     string
	Mountpoint string
	FSType     string
	Opts       string
}

// pseudoFSTypes 是已知不含真实容量的文件系统。
//
// ⚠️ 这份黑名单不完整是必然的（R9 就是漏了 5 种导致的），所以 FilterRealMounts
// 不能只靠它 —— 必须叠加「设备以 /dev/ 开头」这条硬规则。黑名单只是补充，
// 用来挡那些也挂在 /dev/ 下的伪设备（loop、光驱）。
var pseudoFSTypes = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "tmpfs": true,
	"devpts": true, "mqueue": true, "cgroup": true, "cgroup2": true,
	"overlay": true, "squashfs": true,
	// R9：2026-09-13 实测发现原设计漏掉的项
	"securityfs": true, "pstore": true, "efivarfs": true,
	"bpf": true, "configfs": true, "hugetlbfs": true,
	"debugfs": true, "tracefs": true, "fusectl": true,
	// 无真实容量的介质：插一次光盘/U 盘就凭空多一条进度条
	"iso9660": true, "udf": true,
}

// ParseMounts 解析 /proc/mounts（或任意同格式文本）。
// 接受 io.Reader：单测才能喂固定夹具做逐条断言。
func ParseMounts(r io.Reader) ([]Mount, error) {
	var out []Mount
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue // 畸形行跳过，不因一行坏数据丢掉整个采样
		}
		out = append(out, Mount{
			Device:     unescapeMount(f[0]),
			Mountpoint: unescapeMount(f[1]),
			FSType:     f[2],
			Opts:       strings.Join(f[3:], ","),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("metrics: 读取 mounts 失败: %w", err)
	}
	return out, nil
}

// unescapeMount 还原 /proc/mounts 的八进制转义：空格 \040、制表 \011、
// 换行 \012、反斜杠 \134。不还原会把名字显示成 "\040"。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// FilterRealMounts 挑出"值得显示容量进度条"的挂载点。
//
// 双保险（R9 的落点）：
//  1. 设备字段必须以 /dev/ 开头 —— 这一条就能挡掉所有 none/securityfs/
//     efivarfs 之类无块设备的挂载，比无限扩黑名单可靠；
//  2. fstype 黑名单 —— 补挡那些也挂在 /dev/ 下的伪设备（loop、光驱）。
//
// 另外默认排除 /boot 及其子挂载点（系统分区容量对运维无意义），
// 并按设备去重（bind mount 同设备多条时只留最短路径）。
func FilterRealMounts(ms []Mount) []Mount {
	byDev := make(map[string]Mount)
	for _, m := range ms {
		if !strings.HasPrefix(m.Device, "/dev/") {
			continue
		}
		if pseudoFSTypes[m.FSType] {
			continue
		}
		// loop 设备是镜像/snap 挂载，不是真实磁盘。
		if strings.HasPrefix(m.Device, "/dev/loop") {
			continue
		}
		if isBootMount(m.Mountpoint) {
			continue
		}
		if prev, ok := byDev[m.Device]; ok && !betterMount(prev.Mountpoint, m.Mountpoint) {
			continue
		}
		byDev[m.Device] = m
	}

	out := make([]Mount, 0, len(byDev))
	for _, m := range byDev {
		out = append(out, m)
	}
	// 稳定排序：先按路径深度（/ 在最前），同深度按字典序。
	// 不排序的话 map 遍历顺序每帧都变，进度条会跳位。
	sort.Slice(out, func(i, j int) bool {
		di, dj := depth(out[i].Mountpoint), depth(out[j].Mountpoint)
		if di != dj {
			return di < dj
		}
		return out[i].Mountpoint < out[j].Mountpoint
	})
	return out
}

// betterMount 决定同一设备的多个挂载点里保留哪个：先比深度（越浅越像
// 这块盘的"本名"），再比字典序。必须全序且与输入行顺序无关 ——
// 只用长度决胜的话，/DISK 与 /home 同为 5 字符，结果会随 /proc/mounts
// 的行序变化，同一台机器两次采样可能给出不同标签。
func betterMount(prev, cand string) bool {
	dp, dc := depth(prev), depth(cand)
	if dp != dc {
		return dp > dc
	}
	return cand < prev
}

func isBootMount(p string) bool {
	return p == "/boot" || strings.HasPrefix(p, "/boot/")
}

func depth(p string) int {
	return strings.Count(strings.Trim(p, "/"), "/")
}

// Usage 是一个挂载点的容量快照（字节）。
type Usage struct {
	Mountpoint string
	Device     string
	FSType     string
	Total      uint64
	Free       uint64
	Used       uint64
}

func (u Usage) UsedPercent() float64 { return percent(u.Used, u.Total) }

// DiskUsage 用 statfs 取真实容量。
// Free 用 Bavail（非特权可用），与 df 的 avail 列一致；
// 面板以 root 运行也要显示"用户还能写多少"，而不是"文件系统还剩多少"。
func DiskUsage(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, fmt.Errorf("metrics: statfs %s 失败: %w", path, err)
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	free := st.Bavail * bsize
	u := Usage{Total: total, Free: free}
	u.Used = subOrZero(total, free)
	return u, nil
}

// ReadMounts 读取 <procDir>/mounts。
func ReadMounts(procDir string) ([]Mount, error) {
	f, err := os.Open(procDir + "/mounts")
	if err != nil {
		return nil, fmt.Errorf("metrics: 打开 %s/mounts 失败: %w", procDir, err)
	}
	defer f.Close()
	return ParseMounts(f)
}

// Disks 枚举真实磁盘并逐个取容量。
// 某个挂载点在采样间隙被卸载（statfs 失败）时跳过它而不是整轮失败：
// 磁盘条少一条比整块指标消失好，且错误会由调用方记进日志。
func Disks(procDir string) ([]Usage, error) {
	mounts, err := ReadMounts(procDir)
	if err != nil {
		return nil, err
	}
	var out []Usage
	for _, m := range FilterRealMounts(mounts) {
		u, err := DiskUsage(m.Mountpoint)
		if err != nil {
			continue
		}
		u.Mountpoint, u.Device, u.FSType = m.Mountpoint, m.Device, m.FSType
		out = append(out, u)
	}
	return out, nil
}
