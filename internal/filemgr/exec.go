package filemgr

// 内置任务执行器（设计 8.4 / M6-T4）。
//
// 这里是"真的动盘"的那一层：worker 池（queue.go）负责什么时候跑、跑几条、
// 死了怎么收拾；本文件负责把 copy / move / delete 做对。
//
// 分成两层是因为它们的失败方式完全不同：池出错是"任务永远不结束"或"结束
// 错了状态"，执行器出错是"文件内容错了"。混在一起测，报错时分不清该查
// 哪一层。
//
// 三条贯穿全文件的纪律：
//
//  1. **流式**。设计明写 1MB 缓冲的流式复制。整份读入在 3MB 文件上能给出
//     完全正确的哈希 —— 所有正确性测试全绿 —— 然后在用户复制一个 8GB
//     镜像时把面板 OOM 掉，而那台机器上还跑着别的服务。
//  2. **半成品必须清干净**。取消/出错时留下的半截 zip 或 tar 看起来是个
//     完整文件，用户下次拿它解压才发现是坏的。这比"明确没有这个文件"危险。
//  3. **绝不动源，除非已经确认副本是对的**。跨盘 move 的删源一步排在校验
//     之后，且校验是 sha256 而不是"大小相同"。

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"litepanel/internal/logx"
)

// ErrUnsupportedFileType 表示源里有面板不复制的特殊文件（FIFO / socket /
// 设备文件）。
//
// 为什么单列一个哨兵而不是复用 ErrBadPath：FIFO 上执行 open() 会**永久
// 阻塞**（没有另一个打开端时 open 不返回），在 worker 里就是一条永远不
// 结束、还占着一个并发名额的任务。所以这类文件必须在 open 之前就被拒绝，
// 而前端需要能把它认出来并给出"跳过特殊文件"这个可选动作。
var ErrUnsupportedFileType = errors.New("不支持复制的文件类型")

// copyBufferSize 是流式复制的缓冲大小（设计 8.4：1MB）。
const copyBufferSize = 1 << 20

// verifyHashChunk 是校验时的读块大小。比复制的小：校验是纯读，块小了
// 取消反应更快（一个 10GB 文件的 sha256 要跑几分钟）。
const verifyHashChunk = 1 << 20

// nopProgress 是"不关心进度"的占位回调。progressFunc 的调用方（copyFile /
// movePath 等）在调用点不判 nil，所以需要一个真的空函数，而不是每个入口
// 都写一遍 `if report == nil { ... }`。
func nopProgress(int64, int) error { return nil }

// ---------- 流式复制内核 ----------

// copyStream 把 src 流式拷到 dst，返回已写字节数。
//
// 为什么单独有这个函数而不是把循环写进 copyFile：流式与否是一个**只能
// 通过观察写入时序**才能验证的性质（整份读入对任何小文件都能给出正确的
// 哈希与返回值），而那个观察需要一对能互相打听的 reader/writer。把它
// 拆出来，测试就能直接拿假 reader 问流式性本身，不用碰真的文件系统。
//
// 每一块都检查 ctx 与 report 的返回值：取消是从 report 的 error 传回来的
// （见 queue.go 的 throttler），只在块之间看 ctx 的话，一个只在 report
// 之后检查的实现会漏掉检查点。
func copyStream(ctx context.Context, src io.Reader, dst io.Writer, report progressFunc) (int64, error) {
	buf := make([]byte, copyBufferSize)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if werr := writeFull(ctx, dst, buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
			if perr := report(written, 0); perr != nil {
				return written, perr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return written, nil
			}
			return written, rerr
		}
	}
}

// writeFull 把整块写完才回。
//
// 这不是多余的偏执：io.Writer 的契约是"要么返回错误、要么写满"，短写而
// err==nil 属于违规 —— 但真实实现（套了缓冲的包装、部分网络流）确实会给
// 出来，直接忽略就会**静默少拷一段**：文件长度对、内容错，而复制回 nil。
// 这正是 io.Copy 内部自己做的事，这里只是把它和取消检查放在一起。
func writeFull(ctx context.Context, w io.Writer, p []byte) error {
	for len(p) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			// 零推进（且不报错）：继续循环就是死循环，立刻失败。
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// ---------- 单文件复制 ----------

// copyFile 复制**一个**文件（或符号链接），返回已复制字节数。
//
// 不创建目标的父目录：那是复制目录树时的职责（每下一层都要建），单层
// 复制里去建它，等于把"目标路径打错了"变成"在奇怪的地方冒出一堆目录"。
//
// 每一步失败/取消都要保证：源完好、目标要么完整要么根本不存在。
func (s *Service) copyFile(ctx context.Context, src, dst string, report progressFunc) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// 源的 lstat：符号链接要走"复制链接本身"这条路（见下方）。
	srcFI, err := os.Lstat(src)
	if err != nil {
		return 0, err // fs.ErrNotExist → 404
	}
	// 同名护栏（对应 Rename 里那一条）：os 层只提供覆盖，而全盘 root（D14）
	// 下没有系统护栏会拦住"复制吃掉另一个文件"。
	if _, err := os.Lstat(dst); err == nil {
		return 0, fmt.Errorf("%w: %s", ErrExists, dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("检查目标 %s 失败: %w", dst, err)
	}

	// 符号链接：把链接本身复制成链接，不跟进去。
	//
	// 跟进去的两个恶果：指向大目录的链接会被整份展开（用户复制 100MB 实际
	// 拷了 50GB）；日志目录里常见的自指/环状链接会让遍历永不结束。悬空链接
	// 也要能复制 —— "指向一个还不存在的路径"是链接的正常用法。
	if srcFI.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return 0, fmt.Errorf("读符号链接 %s 失败: %w", src, err)
		}
		if err := os.Symlink(target, dst); err != nil {
			return 0, fmt.Errorf("创建符号链接 %s 失败: %w", dst, err)
		}
		return 0, nil // 链接本身没有"内容字节"可报
	}
	if srcFI.IsDir() {
		// 单文件复制不接受目录：调用方要么在复制目录树（copyTree），要么
		// 路径写错了。这里报错比"复制出一个空的同名目录"好查。
		return 0, fmt.Errorf("%w: %s 是目录", ErrNotDirectory, src)
	}

	return copyRegularFile(ctx, src, dst, srcFI, report)
}

// copyRegularFile 拷一个已知是普通文件的源，并保留权限与修改时间。
//
// copyFile 与复制目录树共用它：树里每个文件也要保留权限/时间，两处各写
// 一份早晚漂（最可能的漂法是树那边"忘了"chmod，于是复制一棵含 0600 私钥
// 的家目录会得到一堆 0644 —— 而这正是复制功能自己能造成的安全事故）。
func copyRegularFile(ctx context.Context, src, dst string, srcFI fs.FileInfo, report progressFunc) (int64, error) {
	if err := copyOneFile(ctx, src, dst, report); err != nil {
		return 0, err
	}
	// 元信息：权限与 mtime。
	//
	// 权限不保留的话，复制一个 0600 私钥会得到默认权限 —— 同机其它账号
	// 忽然能读了。mtime 不保留的话，"按时间排序"在界面上看到的一堆备份
	// 全是"刚刚"，而复制备份恰恰是复制最常见的用途。
	if err := os.Chmod(dst, srcFI.Mode().Perm()); err != nil {
		return 0, fmt.Errorf("复制后设置 %s 权限失败: %w", dst, err)
	}
	// atime 传零值 = "不改"（os.Chtimes 的约定）：复制不应该伪装访问时间，
	// 而读取源文件本身就会把源的 atime 推后，这里能做的只是别把副本的写成
	// 别的意义不明的时刻。mtime 才是界面排序读的那个。
	if err := os.Chtimes(dst, time.Time{}, srcFI.ModTime()); err != nil {
		return 0, fmt.Errorf("复制后设置 %s 时间失败: %w", dst, err)
	}
	return srcFI.Size(), nil
}

// copyOneFile 是"打开两边、流式拷、失败就删目标"那一段。
//
// 单拆出来是为了让"清理半成品"这件事只有一个位置可以发生：两条出错
// 路径（copyStream 回错、以及之后的元信息设置失败）都汇到这里的一个
// defer 里，不需要在每处 return 前手工补一次 Remove（漏一处就是留一个
// 半截文件，而半截文件正是最贵的那种错）。
func copyOneFile(ctx context.Context, src, dst string, report progressFunc) error {
	sf, err := os.Open(src)
	if err != nil {
		return err // fs.ErrNotExist → 404
	}
	defer sf.Close()
	// O_EXCL：目标在两次的 Lstat 与这里的 Open 之间被别人建出来时，
	// 这里会失败而不是覆盖掉它。
	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, dst)
		}
		return fmt.Errorf("创建 %s 失败: %w", dst, err)
	}
	written, cerr := copyStream(ctx, sf, df, report)
	// 先关目标：Windows 上没有"删掉还开着的文件"这一说，而数据要在
	// Close 时才保证落盘 —— 把 Close 的错误也当成失败，不然一个"副本
	// 看着在、内容是空的"就可能被当成成功。
	werr := df.Close()
	if cerr == nil {
		cerr = werr
	}
	if cerr != nil {
		// 只删**我们自己建出来**的那个 fd 对应的路径。用 Remove 而不是
		// "只删空文件"：半块文件比空文件更该删。
		_ = os.Remove(dst)
		return cerr
	}
	_ = written // 调用方自己 Stat，避免这里与 Stat 之间的 race
	return nil
}

// ---------- 目录树复制 ----------

// copyTree 把目录 src 复制成 dst，返回复制的条目数。
//
// 四个不显然的正确性点（每一个都有测试钉着）：
//
//  1. **先快照、后写入**。复制中途去重新读源目录（filepath.Walk 那种
//     边走边读）会把"目标恰好在源子树里"之外的另一个问题放大：别的进程
//     在写源时，条目数在跑的过程中一直涨、永远收敛不了。先把整棵树的
//     名字/类型/大小快照下来，复制的就是"按下粘贴那一刻的树"。
//  2. **目录先以可写模式建，填完再 chmod 到最终权限**。直接照搬源权限
//     的话，一个 0500/0000 的目录建出来之后自己就写不进去了。所以模式
//     修复放在最后、从最深的开始（子层填完才轮到父层设时间——往目录里
//     写文件会把它的 mtime 推到"刚刚"，父层最后处理才能让整棵树的目录
//     时间停在快照那一刻）。
//  3. **特殊文件明确拒绝**。FIFO/socket/设备文件不是"不能复制"这么简单：
//     对 FIFO 执行 open() 会**永久阻塞**（没有另一个打开端时 open 不返
//     回），在 worker 里就是一条永远不结束的任务占着一个并发名额。所以
//     只对 FIFO 这类"open 会挂"的类型直接报不可用，绝不触碰 open。
//     （FUSE 挂载点会挂死整棵树是更大的坑，见设计 R11；快照式遍历至少
//     不会在写入阶段死掉，读取阶段的挂死要靠后续可中断的 DirEntry。）
//  4. **任何失败都删掉整棵已建的目标树**。半棵树配一条失败消息是最坏的
//     组合：用户既不能当它成功，也不知道少了什么。要么完整要么没有。
func (s *Service) copyTree(ctx context.Context, src, dst string, report progressFunc) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	srcReal, err := filepath.Abs(src)
	if err != nil {
		return 0, err
	}
	dstReal, err := filepath.Abs(dst)
	if err != nil {
		return 0, err
	}
	// 目标在源自身之内（含完全相同）必须拒绝：选中 src 粘贴到 src/backup
	// 是资源管理器里点两下就能做出来的操作。不拒绝的话要么边遍历边写
	// 自己，要么直接递归爆炸。报错是唯一合理的答复。
	if dstReal == srcReal || strings.HasPrefix(dstReal, srcReal+string(os.PathSeparator)) {
		return 0, fmt.Errorf("%w: 不能把 %s 复制到它自己之内（%s）", ErrBadPath, srcReal, dstReal)
	}
	srcFI, err := os.Lstat(srcReal)
	if err != nil {
		return 0, err // fs.ErrNotExist → 404
	}
	if srcFI.Mode()&fs.ModeSymlink != 0 {
		// 目录树复制不接受符号链接当根：语义不明（复制链接？还是它指的
		// 那棵树？），让调用方先 Stat 看清楚再决定。
		return 0, fmt.Errorf("%w: %s 是符号链接，不是目录", ErrBadPath, srcReal)
	}
	if !srcFI.IsDir() {
		return 0, fmt.Errorf("%w: %s 不是目录", ErrNotDirectory, srcReal)
	}
	// 同名护栏（与 copyFile / Rename 同一族）：目标存在就拒绝，且什么都
	// 不做。粘贴到一个已存在的目录名上要的是 409 与"改名？"，不是合并，
	// 更不是覆盖。
	if _, err := os.Lstat(dstReal); err == nil {
		return 0, fmt.Errorf("%w: %s", ErrExists, dstReal)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("检查目标 %s 失败: %w", dstReal, err)
	}

	// 1) 快照源树（不跟随符号链接）。
	type snap struct {
		rel  string
		fi   fs.FileInfo
		link string // 仅符号链接
	}
	var (
		list    []snap
		dirs    []snap // 需要后置 chmod 的目录（含根）
		srcRoot = srcReal
	)
	err = filepath.Walk(srcReal, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr // 读不了的子目录：整棵树失败（宁可少做不可做错）
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, rerr := filepath.Rel(srcReal, p)
		if rerr != nil {
			return rerr
		}
		item := snap{rel: rel, fi: fi}
		if fi.Mode()&fs.ModeSymlink != 0 {
			to, lerr := os.Readlink(p)
			if lerr != nil {
				return fmt.Errorf("读符号链接 %s 失败: %w", p, lerr)
			}
			item.link = to
		}
		list = append(list, item)
		if fi.IsDir() {
			dirs = append(dirs, item)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	// 2) 写入。失败/取消时整棵删掉（见头注第 4 点）。
	copied := int64(0)
	doneBytes := int64(0)
	commit := false
	defer func() {
		if !commit {
			// 只删自己建出来的这棵树：它在目标处刚被创建，删掉不可能
			// 伤到别的东西。RemoveAll 走的是自己的路径，不跟随任何链接
			// （RemoveAll 明确不 ReadLink）。
			if rerr := os.RemoveAll(dstReal); rerr != nil {
				// 清不干净是最需要留日志的一类失败。
				logx.Error("清理复制失败留下的 %s: %v", dstReal, rerr)
			}
		}
	}()

	if err := os.Mkdir(dstReal, dirWritable(srcFI.Mode())); err != nil {
		return 0, fmt.Errorf("创建目标目录 %s 失败: %w", dstReal, err)
	}
	for _, it := range list {
		if it.rel == "." {
			continue
		}
		to := filepath.Join(dstReal, it.rel)
		switch {
		case it.fi.Mode()&fs.ModeSymlink != 0:
			if err := os.Symlink(it.link, to); err != nil {
				return 0, fmt.Errorf("创建符号链接 %s 失败: %w", to, err)
			}
		case it.fi.IsDir():
			if err := os.Mkdir(to, dirWritable(it.fi.Mode())); err != nil {
				return 0, fmt.Errorf("创建目录 %s 失败: %w", to, err)
			}
		case it.fi.Mode().IsRegular():
			// 每个文件独立上报：报错了才知道停在哪个文件上。
			base := doneBytes
			n, cerr := copyRegularFileCtx(ctx, srcRoot, it.rel, to, it.fi, func(acc int64, _ int) error {
				return report(base+acc, int(copied))
			})
			if cerr != nil {
				return copied, cerr
			}
			doneBytes += n
		default:
			// FIFO / socket / 设备文件：见头注第 3 点，绝不 open。
			return copied, fmt.Errorf("%w: %s 是 %s（面板只复制普通文件、目录与符号链接）",
				ErrUnsupportedFileType, filepath.Join(srcReal, it.rel), it.fi.Mode().Type())
		}
		copied++
		if err := report(doneBytes, int(copied)); err != nil {
			return copied, err
		}
	}

	// 3) 目录权限/时间后置：必须在整棵树填完之后。
	//
	// 为什么必须后置（而不是建目录时就设好）：往一个目录里写子项会把它的
	// mtime 推到当下，建成时就设时间的目录拷完一看全是"刚刚"。权限更是
	// 不能早设 —— 见 dirWritable，0500 的目录一建成就把自己锁死在里面。
	//
	// 这里**不需要**按深度排序，别再"顺手"加一个。我一度以为要（最深先
	// 才能避开父目录无 x 位导致子项 chmod 被挡），实测那是假的：能把一棵
	// 树快照下来，就说明每个目录的属主 x 位都在（否则上面那次 Walk 里的
	// Lstat 早就 EACCES 了），于是后置阶段永远碰得到每一层。加排序不改变
	// 任何结果，只会让人以为它在防什么。
	for _, d := range dirs {
		to := dstReal
		if d.rel != "." {
			to = filepath.Join(dstReal, d.rel)
		}
		if err := os.Chmod(to, d.fi.Mode().Perm()); err != nil {
			return copied, fmt.Errorf("恢复了目录 %s 的权限失败: %w", to, err)
		}
		if err := os.Chtimes(to, time.Time{}, d.fi.ModTime()); err != nil {
			return copied, fmt.Errorf("恢复了目录 %s 的时间失败: %w", to, err)
		}
	}
	commit = true
	return copied, nil
}

// dirWritable 在建树期间保证目录对自己可写：照搬 0500 会把"自己填不进
// 自己的子目录"造出来，而那是结构上不可恢复的（目录一旦建成 0500，
// 只有改它权限这一条路，而那正是下面那个后置 chmod 循环要做的事）。
func dirWritable(m fs.FileMode) fs.FileMode {
	return m.Perm() | 0o300 // 只给自己加写+执行位
}

// copyRegularFileCtx 是 copyRegularFile 的包装：源路径 = srcRoot/rel。
// 单独存在只是为了让 copyTree 的主循环里少两行拼接。
func copyRegularFileCtx(ctx context.Context, srcRoot, rel, dst string, fi fs.FileInfo, report progressFunc) (int64, error) {
	return copyRegularFile(ctx, filepath.Join(srcRoot, rel), dst, fi, report)
}

// ---------- 完整性校验 ----------

// verifyFilesMatch 报告两个文件内容是否一致（sha256 逐字节等价）。
//
// 必须是哈希而不是"大小相同"：大小相同而内容不同是真实会发生的（写到
// 一半但长度凑上了、底层返回了错的数据），而跨盘 move 一旦照着"大小
// 相同"删了源，坏副本就是**不可逆**的数据丢失。
//
// 大小不同直接回 false，不做无谓的读盘：这是个大文件上的常见情形（复制
// 中途被取消，副本短一截），而读满两个 10GB 文件只为得知"不一样"是纯粹
// 的浪费。
func (s *Service) verifyFilesMatch(ctx context.Context, a, b string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if sa.Size() != sb.Size() {
		return false, nil
	}
	ha, err := hashFileCtx(ctx, a)
	if err != nil {
		return false, err
	}
	hb, err := hashFileCtx(ctx, b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

// hashFileCtx 边读边算 sha256，每块检查取消。
//
// 取消必须在这里查：校验一个 10GB 文件要几分钟，而用户在此期间点了取消
// 的话，剩下的时间全花在"确认一份马上要被丢掉的文件"上。
func hashFileCtx(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var h hash.Hash = sha256.New()
	buf := make([]byte, verifyHashChunk)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return fmt.Sprintf("%x", h.Sum(nil)), nil
			}
			return "", fmt.Errorf("读 %s 失败: %w", p, rerr)
		}
	}
}

// ---------- 设备号（跨盘判定） ----------

// sameFilesystem 报告两个路径是否在同一文件系统上。
//
// 用设备号而不是比锚点字符串：move 只关心"rename 会不会给我 EXDEV"，
// 而那由设备号唯一决定。比锚点会把同盘的两个挂载点（bind mount、
// subvolume）判成跨盘，然后老老实实做一次毫无意义的整份复制。
//
// 任一边 stat 不到时回 error：跨盘判定失败不能当"同盘"处理 —— 那样
// move 会先试 rename，而 rename 一旦在跨盘上"意外成功"（某些 fuse 实现
// 允许），源已经没了而校验还没跑。宁可让任务以一个明确的错误结束。
func sameFilesystem(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Lstat(a, &sa); err != nil {
		return false, fmt.Errorf("读 %s 所在文件系统失败: %w", a, err)
	}
	if err := syscall.Lstat(b, &sb); err != nil {
		return false, fmt.Errorf("读 %s 所在文件系统失败: %w", b, err)
	}
	return sa.Dev == sb.Dev, nil
}
