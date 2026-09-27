package filemgr

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"litepanel/internal/logx"
)

// 打包下载（设计 8.3：多选用右键"下载"→ zip 流式返回，不落临时文件）。
//
// "不落临时文件"是硬约束而不是优化：勾选下载往往是几十 GB 的日志目录，
// 先在磁盘上攒一份等于把用户的盘吃空一遍，而且失败时那坨临时文件没人清。
// archive/zip 支持流式写，所以这里直接把 zip.Writer 接到响应的 writer 上。
//
// 代价是**出错时无法回滚**：zip 的中央目录在流的末尾，中途报错时客户端
// 拿到的是一个坏 zip（下载管理器会判失败）。这是流式的固有性质，接受它，
// 换来的是"错误必须真的往上抛"——见 walkAdd 里的每一处 return err。

// Zip 把若干文件/目录打包写入 w。条目名用相对各顶层参数的路径，
// **顶层目录名保留**（压掉一层会让"解压到当前目录"把整棵树倒进用户目录）。
func (s *Service) Zip(ctx context.Context, paths []string, w io.Writer) error {
	if len(paths) == 0 {
		// 空选择回 400 而不是给一个 22 字节的空 zip：空 zip 会被下载器判
		// "成功"，用户盯着一个 0 文件的压缩包琢磨自己是不是点错了。
		return fmt.Errorf("%w: 没有选中任何文件", ErrBadPath)
	}
	zw := zip.NewWriter(w)

	// 条目名去重表。zip 规范允许重名条目，但读端只保留最后一个 ——
	// 不同目录下的 a.txt 一起打包会静默丢掉一个文件。
	used := map[string]bool{}

	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		full, err := AbsClean(p)
		if err != nil {
			return err
		}
		root := path.Base(full)
		fi, err := os.Lstat(full)
		if err != nil {
			// 顶层缺失是 fs.ErrNotExist → API 404：用户在列表里点了它、
			// 别人在这期间删了它，前端该刷新列表而不是弹"路径不合法"
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// 顶层就是一个链接时**打包链接指向的东西**，条目名仍用链接名。
			// 与单文件下载"读目标内容、用链接名另存"是同一个决定。
			//
			// 这里必须用 EvalSymlinks 而不是再调一次 AbsClean：AbsClean
			// 的语义是"解析存在的最深前缀、保留缺失的末段"（mkdir/rename
			// 要靠它保住目标名），末段本身是不是链接它并不保证处理。
			// 拿一个"不保证解析末段"的函数去实现"要解析末段"，
			// 正是 M6-T1 那次 Rename 走错门的镜像错误。
			real, err := filepath.EvalSymlinks(full)
			if err != nil {
				return err
			}
			if err := s.walkAdd(ctx, zw, real, root, used); err != nil {
				return err
			}
			continue
		}
		if err := s.walkAdd(ctx, zw, full, root, used); err != nil {
			return err
		}
	}
	// Close 必须单独判错：zip 的中央目录在 Close 里写，这里失败等于整个
	// 包作废。吞掉它就是交付一个坏 zip。
	if err := zw.Close(); err != nil {
		return fmt.Errorf("打包收尾失败: %w", err)
	}
	return nil
}

// walkAdd 把一个文件或目录树写进 zip。name 是它在包里的前缀。
func (s *Service) walkAdd(ctx context.Context, zw *zip.Writer, full, name string, used map[string]bool) error {
	fi, err := os.Lstat(full)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", full, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// 树**内部**的链接一律跳过。zip 没有可依赖的链接语义：NTFS 上
		// 存成普通文件（等于把 40GB 的稀疏目标整份复制一遍），某些解压端
		// 则会照着相对链接往解压目录外面写 —— 后者是 zip slip 的入口。
		// 跳过是这里唯一两头都安全的选。
		return nil
	}
	if fi.IsDir() {
		entries, err := os.ReadDir(full)
		if err != nil {
			// 读不了的子目录跳过而不中断整包：多选打包常见于"整个
			// /var/log"，里面必然有 root 才读得动的子目录。跳过时写不出
			// 目录条目，解压端因此不会留下一个空的假目录 —— 一个建不出
			// 内容的空目录比没有目录更让人以为数据还在。
			//
			// 这条路径**必须记日志**：它返回 nil，也就是说除了这一行，
			// 没有任何地方知道少了一棵树。不记的话事后根本答不出
			// "解压出来怎么比服务器上少一层"。
			logx.Info("打包跳过读不了的目录 %s: %v", full, err)
			return nil
		}
		// 目录条目以 '/' 结尾，这是 zip 里"这是个目录"的唯一表示法
		// （外部属性字段既非通用也非强制）。零个文件的空目录也要有：
		// 用户勾的就是这棵树，压掉空分支会让解压结果和源不一致。
		if err := addEntry(zw, used, name+"/", fi); err != nil {
			return err
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.walkAdd(ctx, zw, filepath.Join(full, e.Name()),
				name+"/"+e.Name(), used); err != nil {
				return err
			}
		}
		return nil
	}
	return addFile(zw, used, name, full, fi)
}

// addFile 流式写入一个文件条目。
func addFile(zw *zip.Writer, used map[string]bool, name, full string, fi fs.FileInfo) error {
	f, err := os.Open(full)
	if err != nil {
		// 单个文件打不开**必须报错**（与上面"子目录跳过"相反）：文件是
		// 用户明确勾中的内容，悄悄少一个文件的包看起来是成功的。
		return fmt.Errorf("打开 %s 失败: %w", full, err)
	}
	defer f.Close()

	hdr, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	// zip.FileInfoHeader 会按 platform+mode 猜条目名（给的是绝对路径时
	// 猜出来的是一整条以 / 开头的路径），必须整个覆盖掉：包里的路径与
	// 服务器上的路径是两件事，保留绝对路径等于把服务器目录结构连同
	// 用户名一起发出去。
	hdr.Name = uniqueName(used, name)
	// Deflate 而不是 Store：日志类文件能压到十分之一，走的是公网/内网
	// 带宽这条更贵的路。CPU 换带宽在这台机器上永远是赚的（12 核对 1Gbps）。
	hdr.Method = zip.Deflate
	// 显式写 size：zip 的"流式写"（DataDescriptor）在 Windows 资源管理器的
	// 内置解压里对未知大小的条目支持历来不稳，写已知大小最稳。
	hdr.UncompressedSize64 = uint64(fi.Size())

	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", name, err)
	}
	return nil
}

// addEntry 写一个目录条目。目录在 zip 里是以 / 结尾、长度为 0 的条目，
// 这是"这是个目录"的唯一通用表示法（外部属性字段既非通用也非强制）。
func addEntry(zw *zip.Writer, used map[string]bool, name string, fi fs.FileInfo) error {
	hdr, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	hdr.Name = uniqueName(used, name)
	hdr.Method = zip.Store
	// 目录条目必须以 / 结尾：这是 zip 里"这是个目录"的唯一通用表示法
	// （外部属性字段既非通用也非强制），漏了斜杠部分解压端会把它当成
	// 0 字节的普通文件 —— "名为目录的空文件"比没有更让人迷惑。
	//
	// 这个不变量由**调用点**保证（walkAdd 传进来的就是 name+"/"），
	// 这里不再补斜杠：写过一版 `if !HasSuffix { += "/" }` 之后发现
	// 它永远不会命中，而注释里那句"FileInfoHeader 不保证补斜杠"是
	// 我猜的 —— 斜杠从来就是我们自己加的。留一段死代码外加一条
	// 编造的 stdlib 性质，比没有注释更糟。
	if _, err := zw.CreateHeader(hdr); err != nil {
		return err
	}
	return nil
}

// uniqueName 保证包内条目名唯一，冲突时插入 " (1)"、" (2)"…
//
// 形状取自上传的同名冲突策略（设计 8.2 的 name (1).ext），用户在那里
// 见过的规则在这里也认得。扩展名必须留在最后：backup.zip 变成
// "backup.zip (1)" 的话，Windows 上双击会问"用什么应用打开"。
func uniqueName(used map[string]bool, name string) string {
	if !used[name] {
		used[name] = true
		return name
	}
	slash := strings.LastIndexByte(name, '/')
	dir, leaf := "", name
	if slash >= 0 {
		dir, leaf = name[:slash+1], name[slash+1:]
	}
	// 目录条目（以 / 结尾）的后缀插在斜杠之前
	trimLeaf, trailing := leaf, ""
	if strings.HasSuffix(leaf, "/") {
		trimLeaf, trailing = strings.TrimSuffix(leaf, "/"), "/"
	}
	ext := path.Ext(trimLeaf)
	stem := strings.TrimSuffix(trimLeaf, ext)
	for i := 1; ; i++ {
		cand := dir + fmt.Sprintf("%s (%d)%s%s", stem, i, ext, trailing)
		if !used[cand] {
			used[cand] = true
			return cand
		}
	}
}
