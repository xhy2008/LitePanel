package filemgr

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// 目录列举（设计 8.1）。

// ErrNotDirectory：路径存在但不是目录。API 映射 400 —— 它不是"写错了
// 格式"（ErrBadPath）也不是"没了"（fs.ErrNotExist → 404），是第三件事：
// 用户对一个文件执行了"进入目录"。
var ErrNotDirectory = errors.New("不是目录")

// 分页默认值。上限 5000 是护栏不是优化：size 排序的 entries 构造与
// JSON 序列化必须被夹住（全量 stat 是另一回事，见下）。
const (
	DefaultPageSize = 500
	MaxPageSize     = 5000
)

// 排序键。取值即 API 的 ?sort= 参数；未知取值按 SortName 处理，
// 不报错 —— 书签里的旧参数不该让页面打不开。
const (
	SortName  = "name"
	SortSize  = "size"
	SortMtime = "mtime"
	SortType  = "type"
)

// Entry 一个目录项。字段与前端一一对照，snake_case_tag 是 HTTP/WS
// 共用的对外契约。
type Entry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	// MTime unix 秒。不用 RFC3339：前端只拿它排序与显示相对时间，
	// 秒在 12GB 机器的 JSON 体积上比完整时间戳小一半。
	MTime int64 `json:"mtime"`
	// Mode 走 fs.FileMode 的 String 形态（"-rw-r--r--"）：属性对话框
	// 直接显示，chmod 是 M6 之后的事，这里先只读。
	MIME string `json:"mime"` // 按扩展名粗判，只用于挑图标
	Mode string `json:"mode"`
}

// ListPage 一页列表。Path 回显解析后的绝对路径：前端面包屑用它，
// 而不是自己信发出去的参数（符号链接会让两者指向不同东西）。
type ListPage struct {
	Path    string  `json:"path"`
	Page    int     `json:"page"`
	Size    int     `json:"size"`
	Total   int     `json:"total"`
	Entries []Entry `json:"entries"`
}

// ListOptions 列举参数。零值 = 默认页、按名称、不显示隐藏。
type ListOptions struct {
	Hidden bool
	Sort   string
	Desc   bool
	Page   int
	Size   int
}

// List 列出一个目录的一页。
//
// 性能契约（验收条款：10 万目录首屏 ≤500ms）：
//   - 按名称类型排序**不需要 stat 任何条目**：fs.ReadDir 的 DirEntry
//     自带类型，隐藏过滤和两个排序键都只用名字。stat 只发生在
//     当前页的 entries 上（≤500 次）。
//   - 按 size/mtime 排序必须 stat 全量：不 stat 就不知道谁大，分页
//     救不了这个。这是排序键的固有代价，不是实现偷懒 —— 所以循环
//     每一步都检查 ctx，用户切走之后旧请求不能继续抢磁盘。
func List(ctx context.Context, dir string, opts ListOptions) (ListPage, error) {
	// 入口检查，不是礼貌：测试用"已取消的 ctx + 3 条目的目录"钉这条。
	// 只在 stat 循环里检查的话，小目录根本进不了那个分支，取消的旧
	// 请求就会把自己的工作做完。
	if err := ctx.Err(); err != nil {
		return ListPage{}, err
	}
	full, err := AbsClean(dir)
	if err != nil {
		return ListPage{}, err
	}
	switch fi, err := os.Stat(full); {
	case err != nil:
		return ListPage{}, err // fs.ErrNotExist / fs.ErrPermission 原样上抛，errors.Is 保住
	case !fi.IsDir():
		return ListPage{}, fmt.Errorf("%w: %s", ErrNotDirectory, full)
	}
	des, err := os.ReadDir(full)
	if err != nil {
		return ListPage{}, err
	}

	rows := make([]row, 0, len(des))
	for _, de := range des {
		name := de.Name()
		if !opts.Hidden && strings.HasPrefix(name, ".") {
			continue
		}
		// IsDir 取 readdir 的目录项类型：符号链接指向目录时它是 false。
		// 资源管理器会把链接当链接显示（带角标），把"进去"留给双击 ——
		// 跟着解析反而让用户看不出自己在哪。
		r := row{name: name, isDir: de.IsDir(), key: foldKey(name)}
		if opts.Sort == SortType {
			r.ext = extOf(name)
		}
		rows = append(rows, r)
	}
	// 可见总数：隐藏文件不算。分页器拿 Total 算页数，把不可见的算进去
	// 会造出一批永远为空的尾页。
	total := len(rows)

	page, size := clampPaging(opts.Page, opts.Size)
	sortRows(rows, opts.Sort, opts.Desc)

	// 全量排序键（size/mtime）必须在此刻 stat；名称/类型只 stat 本页。
	if needsStat(opts.Sort) {
		if err := statRows(ctx, full, rows); err != nil {
			return ListPage{}, err
		}
		// stat 完再排：size/mtime 的值此刻才有
		sortRows(rows, opts.Sort, opts.Desc)
	}

	lo := (page - 1) * size
	if lo > total {
		lo = total
	}
	hi := lo + size
	if hi > total {
		hi = total
	}
	pageRows := rows[lo:hi]
	if !needsStat(opts.Sort) {
		if err := statRows(ctx, full, pageRows); err != nil {
			return ListPage{}, err
		}
	}
	entries := make([]Entry, 0, len(pageRows))
	for _, r := range pageRows {
		entries = append(entries, r.entry())
	}
	return ListPage{Path: full, Page: page, Size: size, Total: total, Entries: entries}, nil
}

func needsStat(sortKey string) bool {
	return sortKey == SortSize || sortKey == SortMtime
}

func clampPaging(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = DefaultPageSize
	}
	if size > MaxPageSize {
		size = MaxPageSize
	}
	return page, size
}

type row struct {
	name string
	// key/ext 在扫目录时算一次。排序每轮比较都要 key，10 万条目下
	// 现场算等于 170 万次折叠；预计算只花 10 万次，且常见名字零分配。
	key   string
	ext   string
	isDir bool
	size  int64
	mtime time.Time
	mode  fs.FileMode
}

func (r row) entry() Entry {
	e := Entry{Name: r.name, IsDir: r.isDir, Size: r.size, Mode: r.mode.String()}
	if !r.mtime.IsZero() {
		e.MTime = r.mtime.Unix()
	}
	e.MIME = mimeOf(r.name, r.isDir)
	return e
}

func statRows(ctx context.Context, dir string, rows []row) error {
	for i := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		fi, err := os.Stat(filepath.Join(dir, rows[i].name))
		if err != nil {
			// 单个条目 stat 失败绝不拖垮整页：列一半的目录比整页 500
			// 有用。悬空符号链接也走这条路 —— readdir 看得见它、Stat
			// 跟不到目标，于是它以 size=0 留在列表里：用户看得见它才
			// 删得掉它。mode 退回目录项类型，size/mtime 保持零值。
			continue
		}
		rows[i].size = fi.Size()
		rows[i].mtime = fi.ModTime()
		rows[i].mode = fi.Mode()
	}
	return nil
}

func sortRows(rows []row, sortKey string, desc bool) {
	cmp := func(a, b row) int {
		// 目录恒在前，与 desc 无关：翻转会把文件夹淹进文件堆里，
		// 而"找目录"是文件页第一动作。
		if a.isDir != b.isDir {
			if a.isDir {
				return -1
			}
			return 1
		}
		var c int
		switch sortKey {
		case SortSize:
			c = cmpInt64(a.size, b.size)
		case SortMtime:
			c = a.mtime.Compare(b.mtime)
		case SortType:
			c = strings.Compare(a.ext, b.ext)
		}
		// size/mtime 相等（比如同批 touch 的文件）退回名字：没有这一层，
		// 两页之间同键条目的相对顺序不稳定，翻页会看到条目跳动重复。
		if c == 0 {
			c = strings.Compare(a.key, b.key)
		}
		if c == 0 { // 折叠后同名（README.md / readme.md）时按字节，保证全序
			c = strings.Compare(a.name, b.name)
		}
		if desc {
			return -c
		}
		return c
	}
	slices.SortFunc(rows, cmp)
}

// foldKey 名字的大小写折叠排序键。
//
// 为什么预计算而不是边比边折叠：10 万条目的排序要做约 170 万次比较，
// 无论每次比较是 ToLower（分配两个串）还是逐 rune 解码（不分配但每轮
// 重新解码），都是 49ms 量级；预计算把解码次数从 O(n log n) 压到 O(n)。
//
// 纯 ASCII 无大写的名字**原样返回**（不分配）。服务器磁盘上的大多数
// 文件名就是这个形态，于是绝大多数条目一分钱不花。
//
// 语义与 strings.ToLower 逐 rune 等价；仅当单个大写 rune 折叠成多个
// 小写 rune 时（U+0130 之类生僻字符）与 ToLower 的整体结果有差异，
// 那种名字仍得到确定且自洽的次序，页面不会因此乱。
func foldKey(name string) string {
	var buf []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			if buf == nil {
				buf = append(buf, name[:i]...)
			}
			buf = append(buf, c+('a'-'A'))
			continue
		}
		if buf != nil {
			buf = append(buf, c)
			continue
		}
		if c >= utf8.RuneSelf { // 非 ASCII：交给 strings.ToLower 整串处理
			return strings.ToLower(name)
		}
	}
	if buf == nil {
		return name
	}
	return string(buf)
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// extOf "类型"列的扩展名：小写含点。无扩展名返回空串，空串在任何
// 非空扩展名之前 —— 正是"未知类型垫底/垫头"想要的字典序。
// ".bashrc" 这类点开头文件算有扩展名（bashrc 的图标理应是 shell）。
func extOf(name string) string {
	return strings.ToLower(filepath.Ext(name))
}

var mimeByExt = map[string]string{
	".md": "text/markdown", ".markdown": "text/markdown", ".txt": "text/plain",
	".log": "text/plain", ".csv": "text/csv",
	".go": "text/x-go", ".c": "text/x-c", ".h": "text/x-c", ".py": "text/x-python",
	".sh": "text/x-shellscript", ".bash": "text/x-shellscript",
	".js": "text/javascript", ".ts": "text/typescript", ".vue": "text/x-vue",
	".json": "application/json", ".yaml": "application/yaml", ".yml": "application/yaml",
	".toml": "application/toml", ".html": "text/html", ".css": "text/css",
	".sql": "application/sql",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml",
	".ico": "image/x-icon", ".bmp": "image/bmp",
	".mp4": "video/mp4", ".mkv": "video/x-matroska", ".avi": "video/x-msvideo",
	".mov": "video/quicktime", ".webm": "video/webm",
	".mp3": "audio/mpeg", ".flac": "audio/flac", ".wav": "audio/wav",
	".ogg": "audio/ogg", ".m4a": "audio/mp4",
	".zip": "application/zip", ".tar": "application/x-tar", ".gz": "application/gzip",
	".bz2": "application/x-bzip2", ".xz": "application/x-xz", ".7z": "application/x-7z-compressed",
	".rar": "application/vnd.rar", ".tgz": "application/gzip",
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".apk":  "application/vnd.android.package-archive",
}

// mimeOf 按扩展名粗判（设计 8.1）。刻意不用 mime.TypeByExtension：
// 它读 /etc/mime.types，带 charset 参数（"text/markdown; charset=utf-8"），
// 而且**这台机器上有没有那个文件都不一定** —— 图标映射要的是跨机器
// 稳定的查表，不是内容协商。
func mimeOf(name string, isDir bool) string {
	if isDir {
		return "inode/directory"
	}
	if m, ok := mimeByExt[extOf(name)]; ok {
		return m
	}
	return "application/octet-stream"
}
