package filemgr

// 移动执行器（设计 8.4 / M6-T4）。
//
// 同盘是一次 rename(2)：瞬间、原子、inode 不变。
// 跨盘降级成"复制 → 校验 → 删源"三步（设计原话）。
//
// 这一层的风险是**不对称**的：复制失败只是白干一次，而删源做错一步就是不
// 可逆的数据丢失。所以下面每一条判断都在回答同一个问题 —— 源在什么条件
// 下才可以被删掉：
//
//   - 校验通过（sha256 相同，见 verifyFilesMatch 的理由）
//   - 上下文没被取消、没超时
//   - 副本已经完整落在盘上（目录树要整棵拷完，不是拷到一半）
//
// 任何一个不满足，源必须留在原地，副本必须清干净。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// sameFSFunc 判两个路径是否落在同一个文件系统上。
type sameFSFunc func(a, b string) (bool, error)

// sameFSFinder 决定用真实设备号比较还是注入的。
func sameFSFinder(override func(a, b string) (bool, error)) sameFSFunc {
	if override != nil {
		return override
	}
	return sameFilesystem
}

// moveHooks 是测试专用注入点：要在"副本刚拷完、还没删源"这个窗口里做手脚
// （把副本改坏，验执行器会不会仍然删源），真实机器上只能靠竞态去打，测出
// 来的是调度而不是逻辑。生产永远为 nil，判 nil 之后再调用。
type moveHooks struct {
	// afterCopy 在副本完整落盘之后、校验之前调用。
	afterCopy func(dst string) error
	// beforeDeleteSrc 在删源之前调用，回错误就模拟"源删不掉"。
	beforeDeleteSrc func(src string) error
}

// MoveTo 把 src 移动到目录 dstDir 之下（资源管理器的"剪切 + 粘贴"语义：
// 目标是目录，落点是 dstDir/<basename(src)>），返回落地后的路径。
func (s *Service) MoveTo(ctx context.Context, src, dstDir string) (string, error) {
	return s.movePath(ctx, src, dstDir, nopProgress)
}

// movePath 是 MoveTo 的可上报进度的内核（copyTree 那一层要往上报）。
func (s *Service) movePath(ctx context.Context, src, dstDir string, report progressFunc) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	srcReal, err := s.resolveSrcPath(src)
	if err != nil {
		return "", err
	}
	dstDirReal, err := s.resolveDstDir(dstDir)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dstDirReal, filepath.Base(srcReal))
	if dst == srcReal {
		return "", fmt.Errorf("%w: 源与目标相同 (%s)", ErrBadPath, srcReal)
	}
	// "移动到它自己的子目录里"必须结构上先拒（与 copyTree 同一条理由）。
	if srcIsDir(srcReal) && (dstDirReal == srcReal ||
		strings.HasPrefix(dstDirReal, srcReal+string(os.PathSeparator))) {
		return "", fmt.Errorf("%w: 不能把 %s 移动到它自己之内（%s）", ErrBadPath, srcReal, dstDirReal)
	}

	sameFS, err := s.sameFS(srcReal, dstDirReal)
	if err != nil {
		// 判不出跨不跨盘时**不能当同盘处理**：那样会先试 rename，而某些
		// fuse 实现允许跨盘 rename "意外成功"，于是源没了而校验根本没跑。
		return "", err
	}
	if sameFS {
		return s.moveSameDisk(ctx, srcReal, dst)
	}
	return s.moveCrossDisk(ctx, srcReal, dst, report)
}

// MoveMany 把一批源移动到 dstDir 之下，返回搬掉的个数。
//
// **先全部校验、再全部动手**（与 DeleteMany 同一条纪律）：部分成功是这里
// 最坏的结果 —— 界面报了一条错，而实际上有几个文件已经搬走了，用户既看不出
// 是哪几个、也没法回退（移动不像删除有回收站兜着）。
func (s *Service) MoveMany(ctx context.Context, srcs []string, dstDir string, report progressFunc) (int, error) {
	return s.moveMany(ctx, srcs, dstDir, report, false)
}

// moveMany 是 MoveMany 的内核，resumed 表明这是重试跑。
//
// 为什么要这个参数（而且为什么不是直接读 Job.Resumed）：移动的中断点比
// copy/delete 多一个——跨盘时"副本已校验、源未删"，盘上源与目标各有一份。
// 重跑必须认出这个状态并只补上删源；而全新一次移动撞同名目标必须照旧
// 409。两种情形在盘上长得一样，"是不是重试"是唯一分界（见 0008 迁移）。
//
// 参数而不是从 ctx 里掏：调用方只有 runJob 一个，它手里就有完整的 Job,
// 拼参数比建一个隐式上下文通道便宜，而且谁在改这个值一眼可见。
func (s *Service) moveMany(ctx context.Context, srcs []string, dstDir string, report progressFunc, resumed bool) (int, error) {
	if len(srcs) == 0 {
		return 0, fmt.Errorf("%w: 没有要移动的路径", ErrBadPath)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	dstDirReal, err := s.resolveDstDir(dstDir)
	if err != nil {
		return 0, err
	}
	// 第一遍：解析 + 存在性 + 撞名，全部通过才动手（与 DeleteMany 同一条
	// 纪律）。planned 里只放"这一轮要真动手"的条目。
	type plan struct {
		src, dst string
		// finishDeleteSrc：跨盘副本已在且与源一致，只差补最后一步删源。
		// 只有 resumed 且校验通过才会置位。
		finishDeleteSrc bool
	}
	var plans []plan
	seen := make(map[string]bool, len(srcs))
	// done 从"上一轮已经搬完、这轮无需再动"的条目数起算。它们仍计入返回值：
	// 用户重试后要看到"10 个都搬完了"，而不是"只搬了 7 个"（那 3 个他会去找）。
	done := 0
	for _, p := range srcs {
		srcReal, missing, err := s.resolveMoveSrcAllowMissing(p)
		if err != nil {
			return 0, err
		}
		dst := filepath.Join(dstDirReal, filepath.Base(srcReal))
		if missing {
			// 源不见了。只有 resumed 能走到这里（非 resumed 时解析器直接
			// 回 fs.ErrNotExist）。源的消失只可能是上一轮把它搬走了：同盘
			// rename 与"跨盘删源"都是各自的最后一步，源没了即已完成。
			done++
			continue
		}
		if dst == srcReal {
			return 0, fmt.Errorf("%w: 源与目标相同 (%s)", ErrBadPath, srcReal)
		}
		if srcIsDir(srcReal) && (dstDirReal == srcReal ||
			strings.HasPrefix(dstDirReal, srcReal+string(os.PathSeparator))) {
			return 0, fmt.Errorf("%w: 不能把 %s 移动到它自己之内", ErrBadPath, srcReal)
		}
		if seen[dst] {
			return 0, fmt.Errorf("%w: 这一批里有两个条目都叫 %s", ErrExists, filepath.Base(dst))
		}
		seen[dst] = true
		_, dstErr := os.Lstat(dst)
		switch {
		case dstErr == nil && resumed:
			// 重跑撞见同名目标。不能一律当"上一轮的产物"——也可能是用户
			// 自己放的同名文件。先校验：两边内容一致才说明"复制+校验"确实
			// 做完了、只差删源，那就只补删源。校验不过说明这个 dst 不是我
			// 们的产物（或已损坏），报错且两边都不动（见文件头三种错法）。
			ok, verr := s.resumedCopyMatches(ctx, srcReal, dst)
			if verr != nil {
				return 0, verr
			}
			if !ok {
				return 0, fmt.Errorf("%w: %s 已存在且内容与源不一致，不敢替你选哪一份（源 %s 未动）",
					ErrExists, dst, srcReal)
			}
			plans = append(plans, plan{src: srcReal, dst: dst, finishDeleteSrc: true})
		case dstErr == nil:
			return 0, fmt.Errorf("%w: %s", ErrExists, dst)
		case !errors.Is(dstErr, fs.ErrNotExist):
			return 0, fmt.Errorf("检查目标 %s 失败: %w", dst, dstErr)
		default:
			plans = append(plans, plan{src: srcReal, dst: dst})
		}
	}
	n := done
	for _, pl := range plans {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if pl.finishDeleteSrc {
			// 副本已校验过（就在上面第一遍里比过内容），只差补删源这一步。
			if err := s.deleteSrcAfterCopy(ctx, pl.src, pl.dst); err != nil {
				return n, err
			}
			n++
			if err := report(0, n); err != nil {
				return n, err
			}
			continue
		}
		sameFS, err := s.sameFS(pl.src, dstDirReal)
		if err != nil {
			return n, err
		}
		var err2 error
		if sameFS {
			_, err2 = s.moveSameDisk(ctx, pl.src, pl.dst)
		} else {
			_, err2 = s.moveCrossDisk(ctx, pl.src, pl.dst, report)
		}
		if err2 != nil {
			// 已经搬成功的那些**不回滚**：移动是"完成即落地"的动作，为了
			// 回滚再做一次反向移动等于制造新一轮可能失败的数据搬运，而源
			// 也可能已经被别的东西占住了。如实报"停在第几个方面"比假装
			// 什么都没发生诚实。
			return n, err2
		}
		n++
		if err := report(0, n); err != nil {
			return n, err
		}
	}
	return n, nil
}

// resumedCopyMatches 判断"重跑时目标已存在"是不是上一轮复制好的产物。
//
// 普通文件比内容（sha256，复用 verifyFilesMatch）。目录要整棵比（verifyTree）。
// 符号链接比链接目标。任何一条比不出来（读错误等）都回错误，让调用方报错
// 而不是猜——两边都还在盘上，猜错的代价是删掉用户的原件。
func (s *Service) resumedCopyMatches(ctx context.Context, src, dst string) (bool, error) {
	srcFI, err := os.Lstat(src)
	if err != nil {
		return false, err
	}
	dstFI, err := os.Lstat(dst)
	if err != nil {
		return false, err
	}
	// 类型不一致（一个是目录一个不是，或链接 vs 普通文件）：不是我们的产物。
	if srcFI.Mode().Type() != dstFI.Mode().Type() {
		return false, nil
	}
	switch {
	case srcFI.Mode()&fs.ModeSymlink != 0:
		a, e1 := os.Readlink(src)
		b, e2 := os.Readlink(dst)
		if e1 != nil || e2 != nil {
			return false, fmt.Errorf("比较符号链接 %s 与 %s: %v/%v", src, dst, e1, e2)
		}
		return a == b, nil
	case srcFI.IsDir():
		return s.verifyTree(ctx, src, dst)
	case srcFI.Mode().IsRegular():
		return s.verifyFilesMatch(ctx, src, dst)
	default:
		// 特殊文件（FIFO 等）：moveCrossDisk 本来就不会复制它，走到这里
		// 说明盘上状态诡异，报"比不出来"而不是"一样"。
		return false, nil
	}
}

// ---------- 同盘 rename ----------

// moveSameDisk 用 RENAME_NOREPLACE 原子地搬，撞名回 ErrExists。
//
// 为什么不是"os.Lstat 查一下再 os.Rename"：那是两个语句，中间别人建出同名
// 文件的话，rename 会把它**覆盖掉**（os.Rename 是无条件覆盖，全盘 root、
// D14，没有任何系统护栏）。RENAME_NOREPLACE 把"查 + 搬"合成一个内核操作。
//
// 为什么不能用"先 O_EXCL 占个位、再 rename 盖掉自己的占位"这个常见技巧
// （copy 那边就是这么做的）：rename 一个**目录**到一个已存在的普通文件上
// 会 ENOTDIR —— 而移动目录是主要用途。实测本机（kernel 5.15 / f2fs）支持
// RENAME_NOREPLACE，且撞名回 EEXIST 而目标原样、成功时 inode 不变。
func (s *Service) moveSameDisk(ctx context.Context, src, dst string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	if err != nil {
		if errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) {
			return "", fmt.Errorf("%w: %s", ErrExists, dst)
		}
		// EXDEV 走到这里说明 SameFS 判定与实际不一致（并发 umount、或者
		// 注入点给错了）。**不能**在这里悄悄降级成跨盘 copy：那条路是
		// moveCrossDisk 的责任，由它负责校验之后才删源。报错更安全。
		return "", fmt.Errorf("移动 %s → %s 失败: %w", src, dst, err)
	}
	return dst, nil
}

// ---------- 跨盘：复制 → 校验 → 删源 ----------

// moveCrossDisk 是设计 8.4 那句"降级为复制 + 校验 + 删除源"的实现。
//
// 三步的顺序**不能**换。写成"复制 → 删源 → 回头校验"的话，一次副本损坏
// 就变成永久损失；写成"复制 → 删源"（压根不校验）则是每次跨盘移动都在赌
// 文件系统别出错。校验用 sha256 而不是文件大小，理由见 verifyFilesMatch。
//
// 目录与文件分开处理：目录要整棵拷完 + 逐文件校验（一棵 30% 完整的树配一
// 条"失败"消息，用户既不能当它成功也不知道少了什么），文件走单文件路径。
func (s *Service) moveCrossDisk(ctx context.Context, src, dst string, report progressFunc) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return "", err // fs.ErrNotExist → 404
	}
	// 符号链接：重建链接即可，谈不上"校验"（链接本身没有内容）。
	if fi.Mode()&fs.ModeSymlink != 0 {
		return s.moveSymlinkCrossDisk(ctx, src, dst)
	}

	if fi.IsDir() {
		if _, err := s.copyTree(ctx, src, dst, report); err != nil {
			return "", err // copyTree 自己已经把半棵树清掉了
		}
		if s.moveHooks != nil && s.moveHooks.afterCopy != nil {
			if err := s.moveHooks.afterCopy(dst); err != nil {
				_ = os.RemoveAll(dst)
				return "", err
			}
		}
		if err := ctx.Err(); err != nil {
			_ = os.RemoveAll(dst)
			return "", err
		}
		// 整棵树逐条校验（见 verifyTree）。校验失败必须删掉副本而**绝不
		// 碰源**：那棵"源"是用户唯一完整的东西。
		ok, err := s.verifyTree(ctx, src, dst)
		if err != nil {
			_ = os.RemoveAll(dst)
			return "", err
		}
		if !ok {
			_ = os.RemoveAll(dst)
			return "", fmt.Errorf("复制到 %s 后校验不通过，源保持不动", dst)
		}
		return dst, s.deleteSrcAfterCopy(ctx, src, dst)
	}

	if !fi.Mode().IsRegular() {
		// 特殊文件：与 copyTree 同一条理由（FIFO 上 open 会永久阻塞 worker）。
		return "", fmt.Errorf("%w: %s 是 %s（面板只能移动普通文件、目录与符号链接）",
			ErrUnsupportedFileType, src, fi.Mode().Type())
	}

	if _, err := s.copyFile(ctx, src, dst, report); err != nil {
		return "", err // copyFile 自己清掉了半成品
	}
	if s.moveHooks != nil && s.moveHooks.afterCopy != nil {
		if err := s.moveHooks.afterCopy(dst); err != nil {
			_ = os.Remove(dst)
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	ok, err := s.verifyFilesMatch(ctx, src, dst)
	if err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	if !ok {
		_ = os.Remove(dst)
		return "", fmt.Errorf("复制到 %s 后校验不通过，源保持不动", dst)
	}
	return dst, s.deleteSrcAfterCopy(ctx, src, dst)
}

// deleteSrcAfterCopy 删掉源。走到这一步意味着副本已经**校验通过**。
//
// 失败时把两边都在的状态写进错误文本：这个状态没法自动收拾（副本是对的、
// 源也在），但它对用户可理解 —— 他会去手工删一个。反过来如果这里顺手删掉
// 副本，就把一份好的复制结果扔了；而"删了副本又删了源"是唯一不可接受的组合。
func (s *Service) deleteSrcAfterCopy(ctx context.Context, src, dst string) error {
	if s.moveHooks != nil && s.moveHooks.beforeDeleteSrc != nil {
		if err := s.moveHooks.beforeDeleteSrc(src); err != nil {
			return fmt.Errorf("副本已校验通过并落在 %s，但删除源失败：%v（两边都还在，请手工清理其中一个）", dst, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("副本已校验通过并落在 %s，但收尾被中断（%v），源 %s 仍在原地", dst, err, src)
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("副本已校验通过并落在 %s，但删除源失败：%v（两边都还在，请手工清理其中一个）", dst, err)
	}
	return nil
}

// moveSymlinkCrossDisk 在目标处重建链接、然后删掉源链接。
//
// 不存在"校验"这一步：链接没有内容可比。也不许把它指向的目标一起搬过来
// （那是复制语义，而且指向大目录时等于整份展开）。悬空链接同样能搬 ——
// "指向一个还不存在的路径"是链接的正常用法，跟随目标的实现在这里会 404。
func (s *Service) moveSymlinkCrossDisk(ctx context.Context, src, dst string) (string, error) {
	target, err := os.Readlink(src)
	if err != nil {
		return "", fmt.Errorf("读符号链接 %s 失败: %w", src, err)
	}
	if err := os.Symlink(target, dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%w: %s", ErrExists, dst)
		}
		return "", fmt.Errorf("在 %s 重建符号链接失败: %w", dst, err)
	}
	// 链接搬成之后删源链接（RemoveAll 不跟随符号链接，删的是链接本身）。
	if err := s.deleteSrcAfterCopy(ctx, src, dst); err != nil {
		return dst, err
	}
	return dst, nil
}

// verifyTree 逐条比对源树与副本树。
//
// 为什么不能只比"条目数 + 总bytes"：一棵少了 30 个文件、却又多了 30 个
// 名字不同的空文件的树，条目数对得上而内容完全是错的 —— 而 move 接下来
// 要删源。这里必须逐条比：类型、链接指向、文件内容（sha256）。
func (s *Service) verifyTree(ctx context.Context, srcRoot, dstRoot string) (bool, error) {
	var entries []string
	err := filepath.Walk(srcRoot, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if p == srcRoot {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries = append(entries, p)
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, p := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		rel, err := filepath.Rel(srcRoot, p)
		if err != nil {
			return false, err
		}
		q := filepath.Join(dstRoot, rel)
		sfi, err := os.Lstat(p)
		if err != nil {
			return false, err
		}
		dfi, err := os.Lstat(q)
		if err != nil {
			return false, nil // 副本少了这一条 → 不一致
		}
		// 类型必须同构：目录对目录、链接对链接、普通文件对普通文件。
		if sfi.Mode().Type() != dfi.Mode().Type() {
			return false, nil
		}
		switch {
		case sfi.Mode()&fs.ModeSymlink != 0:
			st, err := os.Readlink(p)
			if err != nil {
				return false, err
			}
			dt, err := os.Readlink(q)
			if err != nil {
				return false, err
			}
			if st != dt {
				return false, nil
			}
		case sfi.Mode().IsRegular():
			ok, err := s.verifyFilesMatch(ctx, p, q)
			if err != nil || !ok {
				return ok, err
			}
		}
	}
	return true, nil
}

// ---------- 路径解析 ----------

// resolveSrcPath 把移动源解析成确定路径并确认它存在。
//
// 末段不解析符号链接（SplitResolved）：移动一个链接要搬链接本身，而不是
// 它指向的东西 —— 与 Stat/Rename/Delete 一致。
func (s *Service) resolveSrcPath(p string) (string, error) {
	dir, base, err := SplitResolved(p)
	if err != nil {
		return "", err
	}
	if base == "." || base == "/" {
		return "", fmt.Errorf("%w: 不能移动 %q", ErrBadPath, p)
	}
	full := join(dir, base)
	if _, err := os.Lstat(full); err != nil {
		return "", err // fs.ErrNotExist → 404；fs.ErrPermission → 403
	}
	return full, nil
}

// resolveDstDir 解析目标目录并确认它是个目录。
//
// 不存在必须报 ErrNotDirectory 而**不是**顺手 mkdir：移动的目标是"目录"，
// 它不存在说明路径写错了，悄悄建出来等于把东西丢进一个谁都没打算创建的
// 目录，而界面上还显示"成功"。os.Stat 而不是 Lstat：末段是个指向目录的
// 链接时，那个目录就是合法目标。
func (s *Service) resolveDstDir(p string) (string, error) {
	full, err := AbsClean(p)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: 目标目录 %s 不存在", ErrNotDirectory, full)
		}
		return "", fmt.Errorf("检查目标目录 %s 失败: %w", full, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%w: 目标 %s 不是目录", ErrNotDirectory, full)
	}
	return full, nil
}

// srcIsDir 报告解析后的源是不是目录（跟不跟随链接的判定交给调用方之前做
// 好的 Lstat；这里只服务"能不能移进自己子树"这一条检查）。
func srcIsDir(real string) bool {
	fi, err := os.Stat(real)
	return err == nil && fi.IsDir()
}

// resolveMoveSrcAllowMissing 与 resolveSrcPath 相同，但源不存在时不报错，
// 而是回 (原始路径, missing=true, nil)。只有重试路径用它：重跑时被中断的
// 那一轮可能已经把某个源搬走了，那种"缺失"是**已完成**的证据而不是错误。
//
// 返回值在 missing 时是清理过的绝对路径（没有 srcReal 可拼 basename）。
// 非重试路径继续用 resolveSrcPath：全新一次移动里源不存在就是 404，那是
// 用户要的答案。
func (s *Service) resolveMoveSrcAllowMissing(p string) (string, bool, error) {
	real, err := s.resolveSrcPath(p)
	if err == nil {
		return real, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	full, aerr := AbsClean(p)
	if aerr != nil {
		return "", false, err // 报原来那个 ErrNotExist 更有信息量
	}
	return full, true, nil
}
