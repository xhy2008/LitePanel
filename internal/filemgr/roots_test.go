package filemgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Roots（设计 692 行，D17：按 /proc/mounts 真实枚举，不写死）。
//
// 全部用注入的 procDir + 夹具。真实 /proc/mounts 的内容取决于跑测试的机器
// （本机是 Android/Termux：171 行、根是 erofs 只读），拿它当断言依据的话，
// "roots 必须包含 /"这类检查会在某些环境因形状恰好不合而变红，
// 而那不是被测代码的错。

// svcWithProc 用夹具挂载表 + **假 statfs** 装配。
//
// statfs 必须是注入点，不是实现细节：夹具里的 /DISK、/home 在这台跑测试的
// 机器上（Termux/Android）根本不存在，真 statfs 一定失败。实现若因此丢掉
// 这些挂载点，挂载枚举的测试就只能断言"本机恰好有 /" —— 那测的是环境，
// 不是代码。
func svcWithProc(t *testing.T, fixtureDir string) *Service {
	t.Helper()
	return NewService(Options{
		ProcDir: filepath.Join("testdata", fixtureDir),
		DiskUsage: fakeUsage(map[string]diskUsage{
			"/":     {Total: 100 << 30, Free: 20 << 30},
			"/DISK": {Total: 4_000_000_000, Free: 1 << 30},
			"/home": {Total: 2 << 30, Free: 1 << 30},
		}),
	})
}

// fakeUsage 只对表里列出的挂载点返回容量，其余报"挂载点不存在"，
// 与真实 statfs 在 ENOENT 下的行为一致。
//
// Used 在这里**算出来**而不是手写：真实的 metrics.DiskUsage 就是
// Used = Total - Free，假实现必须照做，否则它是个失真的桩 ——
// 而失真桩会让"Used 字段有没有透传"这件事看起来被测过了，实际测的是
// 一个现实中不存在的组合。
func fakeUsage(m map[string]diskUsage) func(string) (diskUsage, error) {
	return func(p string) (diskUsage, error) {
		u, ok := m[p]
		if !ok {
			return diskUsage{}, &fakeStatfsErr{p}
		}
		if u.Used == 0 && u.Total >= u.Free {
			u.Used = u.Total - u.Free
		}
		return u, nil
	}
}

type fakeStatfsErr struct{ path string }

func (e *fakeStatfsErr) Error() string { return "statfs " + e.path + ": no such file or directory" }

func TestRootsServerFixture(t *testing.T) {
	s := svcWithProc(t, "proc_server")
	roots, err := s.Roots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Root{}
	for _, r := range roots {
		byPath[r.Path] = r
	}
	// 目标机就应该是 "/" + /DISK + /home 三条。
	// 被挡掉的：/boot/efi（EFI 噪音）、/proc /sys /run（伪文件系统）、
	// overlay、/dev/loop3（snap）、pstore、以及 /mnt/disk-mirror
	// （与 /home 同一设备的第二个挂载点？不 —— 那是 /dev/sda1 的镜像,
	// 与 /DISK 同设备，按设备去重后只留最短路径）。
	var got []string
	for _, r := range roots {
		got = append(got, r.Path)
	}
	want := []string{"/", "/DISK", "/home"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("roots = %v, 期望 %v", got, want)
	}
	// /DISK 的挂载点全大写是实测事实（设计 4.2：回收站默认路径据此定），
	// 这里把它钉住：谁哪天"顺手规范成 /disk"，回收站默认值就会指向
	// 一个不存在的目录。
	if _, ok := byPath["/DISK"]; !ok {
		t.Fatal("机械盘 /DISK（全大写）必须在 roots 里")
	}
	// statfs 走假实现，夹具里每个挂载点都有确定数值 —— 数值断言这才真的
	// 有内容（不注入的话本机 /DISK 不存在，断言等于空转）
	d := byPath["/DISK"]
	if d.Total != 4_000_000_000 || d.Free != 1<<30 || d.Used != d.Total-(1<<30) {
		t.Errorf("/DISK 容量不符: %+v", d)
	}
	if !d.Measured {
		t.Errorf("statfs 成功应标 measured: %+v", d)
	}
	if d.FSType != "ext4" || d.Device != "/dev/sda1" {
		t.Errorf("/DISK 设备/类型不符: %+v", d)
	}
	// 排序：浅的在前，/ 一定排第一（地址栏下拉顺序跳变会被当成 bug 报）
	if roots[0].Path != "/" {
		t.Errorf("/ 必须排第一: %v", got)
	}
}

// statfs 失败时必须**保留**挂载点条目，只把容量标成未知。
//
// 磁盘进度条那边（metrics.Disks）的取舍是"少一条比整块消失好"，那是对的；
// 地址栏照抄就成了撒谎：一个真实存在、能进去浏览的挂载点从下拉里消失，
// 用户只可能解读成"我的盘掉了"。容量未知显示成"—"，盘在不在是另一回事。
func TestRootsKeepsMountWithUnknownUsage(t *testing.T) {
	s := NewService(Options{
		ProcDir:   filepath.Join("testdata", "proc_server"),
		DiskUsage: fakeUsage(map[string]diskUsage{"/": {Total: 1 << 30, Free: 1 << 29}}),
	})
	roots, err := s.Roots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range roots {
		if r.Path != "/DISK" {
			continue
		}
		found = true
		if r.Measured {
			t.Errorf("statfs 失败不该标 measured: %+v", r)
		}
		if r.Total != 0 || r.Free != 0 {
			t.Errorf("statfs 失败时容量必须是零值而不是垃圾数据: %+v", r)
		}
		if r.FSType != "ext4" || r.Device != "/dev/sda1" {
			t.Errorf("容量未知也要保留 fstype/设备（用户靠它认出是哪个盘）: %+v", r)
		}
	}
	if !found {
		t.Fatalf("statfs 失败的挂载点必须保留: %+v", roots)
	}
	// 全容器形态：一个 statfs 都不成功时也不能退化成"没有磁盘"
	s2 := NewService(Options{
		ProcDir:   filepath.Join("testdata", "proc_container"),
		DiskUsage: fakeUsage(nil),
	})
	roots2, err := s2.Roots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(roots2) == 0 {
		t.Fatal("容器形态：roots 不能为空")
	}
}

// 只读标记：地址栏要提前告诉用户"这个盘写不了"。没有它，用户在只读挂载
// 上点"新建文件夹"只会拿到一个语义不明的失败。
func TestRootsReadOnlyFlag(t *testing.T) {
	s := svcWithProc(t, "proc_server")
	roots, err := s.Roots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Root{}
	for _, r := range roots {
		byPath[r.Path] = r
	}
	home, ok := byPath["/home"]
	if !ok {
		t.Fatalf("/home 必须在 roots 里（夹具里它是真实挂载）: %+v", roots)
	}
	if !home.ReadOnly {
		t.Errorf("/home 在夹具里是 ro,noatime，必须标只读: %+v", home)
	}
	if disk, ok := byPath["/DISK"]; ok && disk.ReadOnly {
		t.Errorf("/DISK 是 rw,noatime，不该标只读: %+v", disk)
	}
}

// ro 的判定必须整段比对选项。"relatime"／"remount-ro"／"ro_noexec" 这些
// 含 ro 子串的选项若被 strings.Contains 误判，一个正常可写的盘会被显示
// 成只读 —— 用户就不往那儿传文件了，而这是假警报。
func TestIsReadOnlyOpts(t *testing.T) {
	cases := []struct {
		opts string
		want bool
	}{
		{"rw,relatime", false},
		{"ro,noatime", true},
		{"rw,relatime,seclabel", false},
		{"ro,seclabel,relatime", true},   // 本机 / 的实际形态
		{"rw,devtmpfs", false},           // 含 ro 子串但可写
		{"rw,remount-ro", false},         // systemd 的写法
		{"rw,relatime,code=utf8", false}, // 含 "co" 不含 "ro" 段
		{"", false},
		{"ro", true},
	}
	for _, c := range cases {
		if got := isReadOnlyOpts(c.opts); got != c.want {
			t.Errorf("isReadOnlyOpts(%q) = %v, 期望 %v", c.opts, got, c.want)
		}
	}
}

// 一个真盘都没枚举到时**必须**退回 "/"。
//
// 这条是"面板不能撒谎"的具体形态：地址栏下拉为空等于对用户说
// "这台机器没有磁盘"，而面板自己就跑在这台机器上 —— 这句话一定是假的，
// 真实原因八成是 /proc 不可读或跑在容器里。少一个盘可以接受，
// 一个"看起来正常但没有盘"的面板不行。
func TestRootsFallbackToSlash(t *testing.T) {
	for _, fx := range []string{"proc_container", "proc_empty"} {
		s := svcWithProc(t, fx)
		roots, err := s.Roots(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", fx, err)
		}
		if len(roots) != 1 || roots[0].Path != "/" {
			t.Errorf("%s: 应退回恰好一条 /, got %+v", fx, roots)
			continue
		}
		// 兜底条目的容量必须真取到了：Total=0 说明 statfs 也失败了，
		// 那不如报错，别给前端一个"0 字节磁盘"
		if roots[0].Total == 0 {
			t.Errorf("%s: 兜底 / 的容量为 0（statfs 失败）: %+v", fx, roots[0])
		}
	}
}

// /proc 不存在（注入路径写错、或 --ro-bind 掉了 /proc 的二进制）：
// 必须报错，而不是返回空列表 —— 空列表会被渲染成"没有磁盘"。
func TestRootsProcUnreadable(t *testing.T) {
	s := NewService(Options{ProcDir: "/nonexistent-proc-dir"})
	if _, err := s.Roots(context.Background()); err == nil {
		t.Fatal("/proc 读不到时必须报错")
	}
	// 空 ProcDir 收敛到 "/proc"（装配漏传不该导致 roots 全空）
	if NewService(Options{}).ProcDir() != "/proc" {
		t.Fatal("Options{} 应默认 /proc")
	}
}

// ctx 取消：roots 每次都要读 /proc + 对每个挂载点 statfs。
// 手机用户切走之后旧请求不该继续转磁盘。
func TestRootsHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svcWithProc(t, "proc_server").Roots(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后必须报 context.Canceled, got %v", err)
	}
}

// 夹具本身的自检：如果夹具被改坏（比如有人把 /DISK 改成小写来"适配"实现），
// 这条会先红，指出是夹具变了而不是实现坏了。
func TestRootsFixturesSanity(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "proc_server", "mounts"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"/DISK ext4 rw", "/home ext4 ro", "/dev/loop3", "overlay /var/lib/docker"} {
		if !strings.Contains(s, want) {
			t.Errorf("夹具缺少 %q —— 它就不再覆盖那一类挂载点了", want)
		}
	}
}
