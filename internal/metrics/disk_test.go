package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 目标机（Ubuntu 26.04 / E5-2650 v2）的 /proc/mounts。
// 依据设计文档第 22 节 2026-09-13 实测结果重建：真实块设备为
// /dev/sda2=/（SSD）、/dev/sda1=/boot/efi（默认排除）、/dev/sdb1=/DISK（机械盘），
// 外加当时实测发现的 9 种伪文件系统。
//
// ⚠️ 不是原始文件逐字拷贝：服务器上那份全文从未存盘。
// 若能在服务器上重新采集，应直接覆盖本文件，届时下面几条断言
// 仍应全部成立（它们描述的是规则，不是这份数据的巧合）。
const serverMounts = "testdata/proc_mounts_server.txt"

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// R9 的核心验收：过滤后恰好 2 条 —— / 与 /DISK。
// 原设计漏掉 securityfs 等 5 种时这里会变成 11 条。
func TestFilterMountsServerFixture(t *testing.T) {
	mounts, err := ParseMounts(strings.NewReader(readFixture(t, serverMounts)))
	if err != nil {
		t.Fatal(err)
	}
	got := FilterRealMounts(mounts)

	var points []string
	for _, m := range got {
		points = append(points, m.Mountpoint)
	}
	want := []string{"/", "/DISK"}
	if len(points) != len(want) {
		t.Fatalf("应恰好 %d 条, got %d: %v", len(want), len(points), points)
	}
	for i := range want {
		if points[i] != want[i] {
			t.Errorf("第 %d 条 = %s, want %s", i, points[i], want[i])
		}
	}
	// /DISK 是全大写（不是早期假设的 /mnt/hdd）。
	if got[1].Mountpoint != "/DISK" {
		t.Errorf("机械盘挂载点应为 /DISK, got %s", got[1].Mountpoint)
	}
	if got[1].Device != "/dev/sdb1" || got[1].FSType != "ext4" {
		t.Errorf("sdb1 字段错: %+v", got[1])
	}
}

// 双保险的两条规则各自都要能独立挡住伪文件系统：
// 单靠 fstype 黑名单会漏（R9 就是这么发生的），所以设备必须以 /dev/ 开头。
func TestFilterRequiresDevPrefix(t *testing.T) {
	cases := map[string]string{
		"securityfs": "securityfs /sys/kernel/security securityfs rw 0 0",
		"pstore":     "pstore /sys/fs/pstore pstore rw 0 0",
		"efivarfs":   "efivarfs /sys/firmware/efi/efivars efivarfs rw 0 0",
		"hugetlbfs":  "none /dev/hugepages hugetlbfs rw 0 0",
		"debugfs":    "debugfs /sys/kernel/debug debugfs rw 0 0",
		"bpf":        "bpf /sys/fs/bpf bpf rw 0 0",
		"configfs":   "none /sys/kernel/config configfs rw 0 0",
		"tracefs":    "tracefs /sys/kernel/tracing tracefs rw 0 0",
		"fusectl":    "fusectl /fs/fusectl fusectl rw 0 0",
		"tmpfs":      "tmpfs /run tmpfs rw 0 0",
		"overlay":    "overlay /var/lib/docker/x overlay rw 0 0",
		"squashfs":   "squashfs /snap/core/1 squashfs ro 0 0",
		"cgroup2":    "none /sys/fs/cgroup cgroup2 rw 0 0",
		"cgroup":     "none /dev/memcg cgroup rw 0 0",
		"devpts":     "devpts /dev/pts devpts rw 0 0",
		"mqueue":     "mqueue /dev/mqueue mqueue rw 0 0",
		"proc":       "proc /proc proc rw 0 0",
		"sysfs":      "sysfs /sys sysfs rw 0 0",
		"devtmpfs":   "devtmpfs /dev devtmpfs rw 0 0",
		// 设计未列但真实会出现的：snap 的 loop 设备、光驱。
		// 它们设备字段确实是 /dev/ 开头，必须靠 fstype/设备类型挡掉，
		// 否则无头服务器上插一次 U 盘就凭空多几条磁盘条。
		"iso9660": "/dev/sr0 /media/cdrom iso9660 ro 0 0",
		"udf":     "/dev/sr0 /media/dvd udf ro 0 0",
		"loop":    "/dev/loop3 /snap/foo/2 squashfs ro 0 0",
		"loop2":   "/dev/loop0 /mnt/img ext4 rw 0 0",
	}
	for name, line := range cases {
		got := FilterRealMounts(mustParse(t, line))
		if len(got) != 0 {
			t.Errorf("%s 应被过滤, got %+v", name, got)
		}
	}
}

// 同一设备的多个挂载点（bind mount）只保留一条，否则一块盘会出现两条进度条，
// 而两条都指向同一份容量，读起来像是两块盘快满了。
func TestFilterDedupesSameDevice(t *testing.T) {
	got := FilterRealMounts(mustParse(t, `
/dev/sdb1 /DISK ext4 rw 0 0
/dev/sdb1 /home ext4 rw 0 0
/dev/sdb1 /var/lib/docker ext4 rw 0 0
`))
	if len(got) != 1 {
		t.Fatalf("同设备应只留 1 条, got %d: %+v", len(got), got)
	}
	// 保留最短路径（最"根"的那个），语义上更像这块盘的本名。
	if got[0].Mountpoint != "/DISK" {
		t.Errorf("应保留最短路径 /DISK, got %s", got[0].Mountpoint)
	}
}

// /boot 与 /boot/efi 属于系统分区，容量信息对运维无意义，默认排除且可配置。
func TestFilterExcludesBootByDefault(t *testing.T) {
	got := FilterRealMounts(mustParse(t, `
/dev/sda2 / ext4 rw 0 0
/dev/sda1 /boot/efi vfat rw 0 0
/dev/sda3 /boot ext4 rw 0 0
`))
	if len(got) != 1 || got[0].Mountpoint != "/" {
		t.Errorf("/boot 及子挂载应被排除, got %+v", got)
	}
}

// 顺序必须稳定：按挂载点深度再字典序，否则每帧刷新时进度条会跳位。
func TestFilterOrderIsStable(t *testing.T) {
	in := `
/dev/sdd1 /data/deep/deeper ext4 rw 0 0
/dev/sda1 / ext4 rw 0 0
/dev/sdc1 /DISK ext4 rw 0 0
/dev/sdb1 /data ext4 rw 0 0
`
	first := FilterRealMounts(mustParse(t, in))
	second := FilterRealMounts(mustParse(t, in))
	if len(first) != 4 {
		t.Fatalf("应 4 条, got %d", len(first))
	}
	for i := range first {
		if first[i].Mountpoint != second[i].Mountpoint {
			t.Fatalf("顺序不稳定: %v vs %v", first, second)
		}
	}
	if first[0].Mountpoint != "/" || first[1].Mountpoint != "/DISK" {
		t.Errorf("应按深度+字典序, got %v", names(first))
	}
}

func TestParseMountsRejectsMalformed(t *testing.T) {
	// 字段不足的行直接跳过（内核偶尔会输出奇怪的东西），
	// 但完全读不出来要报错。
	if got := mustParse(t, "garbage\n"); len(got) != 0 {
		t.Errorf("畸形行应被跳过, got %+v", got)
	}
}

// 挂载点含空格时 /proc/mounts 用八进制转义（\040）。
// 不还原会把进度条名字显示成 "\040"，也可能让前端拼路径时错位。
func TestParseMountsUnescapesOctal(t *testing.T) {
	got := mustParse(t, `/dev/sdb1 /DISK/my\040drive ext4 rw 0 0`)
	if len(got) != 1 {
		t.Fatalf("应解析出 1 条, got %d", len(got))
	}
	if got[0].Mountpoint != "/DISK/my drive" {
		t.Errorf("应还原空格, got %q", got[0].Mountpoint)
	}
}

// statfs 走真实调用（临时目录一定有文件系统）。
func TestDiskUsageRealStatfs(t *testing.T) {
	dir := t.TempDir()
	u, err := DiskUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u.Total == 0 {
		t.Fatal("Total 为 0")
	}
	if u.Total < u.Used {
		t.Errorf("Total(%d) < Used(%d)", u.Total, u.Used)
	}
	if p := u.UsedPercent(); p < 0 || p > 100 {
		t.Errorf("百分比越界: %v", p)
	}
	if u.Free > u.Total {
		t.Errorf("Free(%d) > Total(%d)", u.Free, u.Total)
	}
}

// 路径不存在必须报错并带上路径，否则磁盘条"少了一条"时无从排查。
func TestDiskUsageMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	_, err := DiskUsage(missing)
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("错误应含路径, got %v", err)
	}
}

func mustParse(t *testing.T, s string) []Mount {
	t.Helper()
	m, err := ParseMounts(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func names(ms []Mount) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Mountpoint
	}
	return out
}

// 同一设备的两个挂载点路径长度相同（/DISK 与 /home 都是 5 字符）时，
// 保留哪一条必须由规则决定，不能随 /proc/mounts 的行序变化 ——
// 否则同一台机器两次采样可能给出不同标签，前端进度条会莫名改名。
func TestDedupeTieIsOrderIndependent(t *testing.T) {
	a := mustParse(t, "/dev/sdb1 /DISK ext4 rw 0 0\n/dev/sdb1 /home ext4 rw 0 0\n")
	b := mustParse(t, "/dev/sdb1 /home ext4 rw 0 0\n/dev/sdb1 /DISK ext4 rw 0 0\n")
	ga, gb := FilterRealMounts(a), FilterRealMounts(b)
	if len(ga) != 1 || len(gb) != 1 {
		t.Fatalf("各应 1 条: %d / %d", len(ga), len(gb))
	}
	if ga[0].Mountpoint != gb[0].Mountpoint {
		t.Errorf("行序不应改变结果: %s vs %s", ga[0].Mountpoint, gb[0].Mountpoint)
	}
	// 同深度时取字典序较小者，规则写死在代码里。
	if ga[0].Mountpoint != "/DISK" {
		t.Errorf("应取字典序较小者 /DISK, got %s", ga[0].Mountpoint)
	}
}

// 深浅不同的 bind mount：应保留更浅的那个（更像盘的本名）。
func TestDedupePrefersShallower(t *testing.T) {
	got := FilterRealMounts(mustParse(t, `
/dev/sdc1 /srv/data/live ext4 rw 0 0
/dev/sdc1 /srv ext4 rw 0 0
`))
	if len(got) != 1 || got[0].Mountpoint != "/srv" {
		t.Errorf("应保留 /srv, got %+v", got)
	}
}
