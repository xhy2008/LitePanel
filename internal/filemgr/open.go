package filemgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"
)

// ErrIsDirectory 表示"对一个目录发起了只适用于文件的操作"。
//
// 不复用 ErrNotDirectory：那句话的意思是"你要的目录不是目录"，套在这里
// 正好说反，用户在屏幕上读到的是自相矛盾的一句话。
var ErrIsDirectory = errors.New("是目录")

// Opened 是一个已打开、可读的文件。
//
// 为什么把 Name/Size/ModTime 一起带出来而不让调用方再 Stat 一次：下载
// 响应需要这三样来填 Content-Length / Last-Modified / Content-Disposition，
// 而 Stat→Open 之间有窗口 —— 分开取就会写出"响应头说 1000 字节、实际
// 流了 900 字节"的响应（浏览器表现为下载永远失败或文件截断）。
// 一次 Open + 一次 File.Stat 拿全，头部与实际内容必然同源。
type Opened struct {
	File    io.ReadSeekCloser
	Name    string
	Path    string
	Size    int64
	ModTime time.Time
}

// Open 打开一个文件供流式读取（下载）。
//
// 与 Rename/Delete 不同，这里走 AbsClean 而**不是** SplitResolved：
// 下载一个符号链接，用户要的是它指向的内容 —— 链接自己那几十个字节的
// 目标路径字符串对任何人都是垃圾。同一个面板里两种语义并存不是不一致，
// 而是"读内容"与"改对象"本来就是两件事（见 safe.go 的分类说明）。
//
// 目录一律拒绝。打包下载是另一个端点（zip 流），不能靠这里"顺便"支持：
// 目录返回的内容是什么没有定义，而 Accept-Encoding/Content-Type 全都会
// 变成瞎猜。
func (s *Service) Open(ctx context.Context, p string) (Opened, error) {
	if err := ctx.Err(); err != nil {
		return Opened{}, err
	}
	full, err := AbsClean(p)
	if err != nil {
		return Opened{}, err
	}
	// 另存为对话框里的名字取**用户点的那个名字**，不是解析后的目标名：
	// 下载 link.txt 却弹出另存为 target.txt，用户会以为自己点错了文件，
	// 而更糟的是他不知道。AbsClean 已经把路径解析到目标，所以这里必须
	// 单独从原始输入取末段（cleanOnly 只做清理与合法性检查，不解析）。
	named, err := cleanOnly(p)
	if err != nil {
		return Opened{}, err
	}
	base := path.Base(named)

	f, err := os.Open(full)
	if err != nil {
		return Opened{}, fmt.Errorf("打开失败 (%v): %w", p, err)
	}
	// 用 File.Stat 而不是 Lstat：此刻 fd 已经打开，stat 的就是真正要读的
	// 那个 inode，不会再被并发的改名/换链接影响。
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return Opened{}, fmt.Errorf("读取信息失败 (%v): %w", p, err)
	}
	if fi.IsDir() {
		f.Close()
		return Opened{}, fmt.Errorf("%w: %s", ErrIsDirectory, full)
	}
	return Opened{
		File:    f,
		Name:    base,
		Path:    full,
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
	}, nil
}
