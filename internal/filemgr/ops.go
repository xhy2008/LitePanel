package filemgr

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"litepanel/internal/metrics"
)

// ErrExists 表示目标名字已被占用。
//
// 它必须与 fs.ErrNotExist 一样是可 errors.Is 的具名错误：mkdir 撞名回 409、
// rename 撞名回 409 是前端"是否弹重命名建议"的依据。更关键的是 rename：
// os.Rename 在 Unix 上**默认覆盖目标**，全盘 root（D14）意味着没有任何
// 系统层护栏会拦住"重命名撞名 → 别人的文件凭空消失"，这道检查是唯一的
// 护栏，所以它得有自己的错误类型而不是混在通用错误里。
var ErrExists = errors.New("目标已存在")

// SplitResolved 把路径拆成"已解析的父目录 + 原样保留的末段"。
//
// 为什么不直接用 AbsClean 的结果：AbsClean 会连末段一起解析，于是
// "对符号链接做 stat/重命名/删除"实际作用到了**目标**上。文件管理器的
// 语义必须是操作链接本身（mv 改的是链接，rm 删的是链接，ls -l 显示的是
// 链接），所以写入类操作一律"解析父目录、末段原样"。
//
// 解析父目录仍然要做：中间某层是符号链接时，不解析就会在错误的地方建
// 出一个同名目录（用户看着链接双击"新建文件夹"，结果东西落在链接的
// 目标之外）。
func SplitResolved(p string) (dir, base string, err error) {
	clean, err := cleanOnly(p)
	if err != nil {
		return "", "", err
	}
	dir, err = AbsClean(filepath.Dir(clean))
	if err != nil {
		return "", "", err
	}
	return dir, filepath.Base(clean), nil
}

// join 把 SplitResolved 的两半接回去。用 filepath.Join 是为了处理
// dir="/"（Join 不会产生 "//x"）。
func join(dir, base string) string { return filepath.Join(dir, base) }

// Stat 返回一个路径自身的条目信息（不跟随符号链接）。
func (s *Service) Stat(ctx context.Context, p string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	dir, base, err := SplitResolved(p)
	if err != nil {
		return Entry{}, err
	}
	full := join(dir, base)
	fi, err := os.Lstat(full)
	if err != nil {
		return Entry{}, err // fs.ErrNotExist / fs.ErrPermission 原样上抛
	}
	r := row{name: base, isLink: fi.Mode()&fs.ModeSymlink != 0}
	r.isDir = fi.IsDir() // Stat 的 is_dir 跟随真实类型：属性对话框问的是"这是什么"
	r.size, r.mtime, r.mode = fi.Size(), fi.ModTime(), fi.Mode()
	e := r.entry()
	// Lstat 到的链接：is_dir 恒 false（与 List 一致）；想知道链接指向的是
	// 不是目录，前端看 is_symlink 自己判断。
	if r.isLink {
		e.IsDir = false
	}
	e.Name = filepath.Base(full)
	e.Path = full
	return e, nil
}

// Mkdir 建目录（多级）。
//
// 撞名必须报错而不是 os.MkdirAll 的静默成功：静默成功会让用户以为"新建"
// 生成了一个新目录，而实际上他进了一个同名旧目录（里面可能有几百个文件）。
// 存在性检查用 **Lstat**：悬空符号链接也占着这个名字，Mkdir 在它上面
// 一定失败（EEXIST），如实报"名字被占用"比报一个 syscall 错误诚实。
func (s *Service) Mkdir(ctx context.Context, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := AbsClean(p)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(full); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, full)
	} else if !errors.Is(err, fs.ErrNotExist) {
		// 权限不足（EACCES）之类：父目录读不了，继续 MkdirAll 也只会
		// 得到同样的失败，但这里的错误要带上是**哪个**路径。
		return fmt.Errorf("创建目录 %s 失败: %w", full, err)
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		// 竞态：Lstat 与 MkdirAll 之间别人建好了同名目录。
		// MkdirAll 对已存在目录返回 nil，所以要再认一次并转成 ErrExists。
		if _, lerr := os.Lstat(full); lerr == nil {
			return fmt.Errorf("%w: %s", ErrExists, full)
		}
		return fmt.Errorf("创建目录 %s 失败: %w", full, err)
	}
	return nil
}

// Rename 改名或移动（同一操作，os.Rename 两者都能做，"剪切+粘贴"就是它）。
//
// 三道检查一道都不能省，全都在 os.Rename **之前**：
//  1. 源必须存在（Lstat）→ 否则 fs.ErrNotExist → 404；
//  2. 目标的父目录必须是目录 → 否则 ErrNotDirectory → 400。os.Rename 在
//     这里给的是 ENOENT，与"源不存在"同一个 errno，前端会显示成矛盾的
//     文案（源明明在，却说找不到）；
//  3. 目标名字未被占用 → ErrExists → 409。**os.Rename 默认覆盖目标**，
//     而全盘 root 没有任何系统护栏（D14）：没有这一条，重命名撞名的后果
//     是另一个文件无声消失。
//
// 失败时源必须完好无损：rename 是文件系统层的原子操作，检查都在它前面，
// 所以这一条由"还没调用它"保证，测试把它钉住是为了将来有人把实现换成
// "复制 + 删源"（跨文件系统的 move 真的需要这么做，见 M6-T4）时不至于
// 悄悄丢掉这个保证。
func (s *Service) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	srcDir, srcBase, err := SplitResolved(from)
	if err != nil {
		return err
	}
	dstDir, dstBase, err := SplitResolved(to)
	if err != nil {
		return err
	}
	src, dst := join(srcDir, srcBase), join(dstDir, dstBase)
	if src == dst {
		return fmt.Errorf("%w: 源与目标相同 (%s)", ErrBadPath, src)
	}
	if _, err := os.Lstat(src); err != nil {
		return err // fs.ErrNotExist → 404；fs.ErrPermission → 403
	}
	if fi, err := os.Stat(dstDir); err != nil {
		return fmt.Errorf("%w: 目标目录 %s 不存在", ErrNotDirectory, dstDir)
	} else if !fi.IsDir() {
		return fmt.Errorf("%w: 目标目录 %s 不是目录", ErrNotDirectory, dstDir)
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("重命名到 %s 失败: %w", dst, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("重命名 %s → %s 失败: %w", src, dst, err)
	}
	return nil
}

// Root 是一个可浏览的磁盘/挂载点（设计 692 行 /api/fs/roots）。
type Root struct {
	// Path 是挂载点，可直接喂给 /api/fs/list。
	Path   string `json:"path"`
	Device string `json:"device"`
	FSType string `json:"fstype"`
	Total  uint64 `json:"total"`
	Free   uint64 `json:"free"`
	Used   uint64 `json:"used"`
	// Measured=false 表示 statfs 没成功、容量未知，前端显示"—"。
	//
	// 这个字段的存在是因为"容量取不到"和"盘不在了"是两件事，不能合并：
	// 磁盘进度条那边（metrics.Disks）跳过 statfs 失败的挂载点是对的 ——
	// 少一条进度条比整块指标消失好。地址栏照抄就是撒谎：一个真实存在、
	// 能进去浏览的挂载点从下拉里消失，用户只可能解读成"我的盘掉了"。
	// 所以这里保留条目、只把容量标成未知。
	Measured bool `json:"measured"`
	// ReadOnly 让地址栏提前告诉用户"这个盘写不了"。没有它，在只读挂载
	// 上点"新建文件夹"只会得到一个措辞含糊的 500。
	ReadOnly bool `json:"read_only"`
}

// Roots 枚举挂载点供地址栏与收藏用（设计 D17：按 /proc/mounts 真实枚举，
// 不写死）。
//
// 磁盘枚举的**主人是 metrics 包**（FilterRealMounts），这里只做展示层的
// 转换，不复制一份过滤逻辑：那份逻辑带 R9 的教训（fstype 黑名单注定
// 不完整，必须叠加"设备以 /dev/ 开头"的硬规则），复制一份等于埋下
// 第二主人，两处的结果会随机器不同而悄悄分叉 —— 磁盘进度条少一个盘和
// 地址栏多五个伪文件系统，谁都看不出是代码问题。
func (s *Service) Roots(ctx context.Context) ([]Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mounts, err := metrics.ReadMounts(s.procDir)
	if err != nil {
		return nil, err
	}
	// 只读判定要 Mount.Opts，而 metrics 的 Usage 把它丢了 —— 所以这里
	// 自己读表 + 复用 FilterRealMounts；statfs 仍交给注入的 s.usage
	// （默认 metrics.DiskUsage），不在这里重写。
	ro := make(map[string]bool)
	for _, m := range mounts {
		ro[m.Mountpoint] = isReadOnlyOpts(m.Opts)
	}
	out := make([]Root, 0, 4)
	for _, m := range metrics.FilterRealMounts(mounts) {
		r := Root{Path: m.Mountpoint, Device: m.Device, FSType: m.FSType, ReadOnly: ro[m.Mountpoint]}
		// statfs 失败不丢条目，只留 Measured=false（见 Root.Measured 的说明）
		if u, err := s.usage(m.Mountpoint); err == nil {
			r.Total, r.Free, r.Used, r.Measured = u.Total, u.Free, u.Used, true
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		// 一个真盘都没枚举到（容器里跑面板、/proc 不可读的二进制、
		// 或者挂载表全是 overlay）。此时**必须**还有 "/"：地址栏下拉
		// 为空等于"面板说这台机器没有磁盘"，而面板自己就跑在这台机器上，
		// 这句话一定是假的。
		if r, err := s.rootFallback(); err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// rootFallback 兜底一条 "/"。
//
// statfs 失败也要返回条目（Measured=false）：这条兜底存在的理由恰恰是
// "环境读不到东西"，那时 statfs 大概率也失败；如果因此返回错误，兜底
// 就等于没有 —— 面板仍然会显示"这台机器没有磁盘"，而面板自己就跑在
// 这台机器 上。
func (s *Service) rootFallback() (Root, error) {
	r := Root{Path: "/", ReadOnly: s.mountReadOnly("/")}
	if u, err := s.usage("/"); err == nil {
		r.Total, r.Free, r.Used, r.Measured = u.Total, u.Free, u.Used, true
	}
	return r, nil
}

// isReadOnlyOpts 判断挂载选项里有没有 ro。必须整段比对：
// strings.Contains(opts,"ro") 会把 "rw,relatime,ro_noexec" 之外的一堆
// 含 ro 子串的选项（devtmpfs、remount-ro 之类）误判成只读。
func isReadOnlyOpts(opts string) bool {
	for _, o := range strings.Split(opts, ",") {
		if o == "ro" {
			return true
		}
	}
	return false
}

// mountReadOnly 在 /proc/mounts 里查一个 mounting point 是否只读。
// 找不到该 mount 时按可写处理（roots 的兜底路径：容器里 / 可能压根
// 不在 mounts 表里，但 statfs 能成功）。
func (s *Service) mountReadOnly(p string) bool {
	mounts, err := metrics.ReadMounts(s.procDir)
	if err != nil {
		return false
	}
	for _, m := range mounts {
		if m.Mountpoint == p {
			return isReadOnlyOpts(m.Opts)
		}
	}
	return false
}
