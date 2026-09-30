package api

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"litepanel/internal/filemgr"
)

// Files 是 HTTP 层需要的文件管理能力（设计 690-694 行）。
//
// 只列 M6-T1/T6 骨架已实现的部分；上传/下载/任务/回收站随 M6-T2..T5
// 往这里加方法。**没接的端点必须 501**（见 router），不能返回 200 + 空：
// 空列表会被前端渲染成"这个目录是空的"，把"面板没接模块"说成"你目录里
// 没文件"。
//
// 参数一律是 ctx + 领域类型，不接受 *http.Request：文件操作要能被
// cmd/litepanel 的装配测试直接调用（"接的是不是真家伙"那一层），
// 接口里夹着 http.Request 就调不动了。
type Files interface {
	List(ctx context.Context, dir string, opts filemgr.ListOptions) (filemgr.ListPage, error)
	Stat(ctx context.Context, path string) (filemgr.Entry, error)
	Mkdir(ctx context.Context, path string) error
	Rename(ctx context.Context, from, to string) error
	Roots(ctx context.Context) ([]filemgr.Root, error)
	Open(ctx context.Context, path string) (filemgr.Opened, error)
	Zip(ctx context.Context, paths []string, w io.Writer) error

	// 上传四件套（设计 8.2）。BeginUpload 建立或恢复会话；PutChunk 收块
	// （参数在头里，见 handlers_upload.go）；UploadStatus 供断点续传查询；
	// AbortUpload 丢弃已收的分块。
	BeginUpload(ctx context.Context, in filemgr.UploadInit) (filemgr.UploadState, error)
	PutChunk(ctx context.Context, c filemgr.UploadChunk) (filemgr.UploadState, error)
	UploadStatus(ctx context.Context, id string) (filemgr.UploadState, error)
	AbortUpload(ctx context.Context, id string) error

	// 回收站（设计 8.6 / D12，按盘分置）。DeleteMany 是界面上的"删除"：
	// permanent=false 移进各盘自己的回收站，true 直接永久删除。
	DeleteMany(ctx context.Context, paths []string, permanent bool) (int, error)
	ListTrash(ctx context.Context) ([]filemgr.TrashItem, error)
	RestoreTrash(ctx context.Context, id string) (string, error)
	PurgeTrash(ctx context.Context, id string) error
	EmptyTrash(ctx context.Context) (int, error)
}

// 编译期把"真家伙满足接口"钉住。没有这行，接口与 *filemgr.Service 的
// 脱节只会在装配测试跑到时才暴露，而那时报错的是 cmd 包，看不出是
// 接口漏了方法还是实现签名漂了。
var _ Files = (*filemgr.Service)(nil)

// fileListQuery 把 URL 查询参数翻成 ListOptions。
//
// 非数字的 page/size 静默降级为默认值，不报 400：用户从书签/收藏夹打开
// 一个带着过期 ?page=abc 的 URL 时，他的意图是"打开这个目录"，
// 一个红框会把这件事变成"面板坏了"。分页参数不承载任何危险语义
// （size 有上限夹着），宽容没有代价。
//
// 未知的 sort 取值同样退回名称序（filemgr 侧也是这个行为，这里不重复
// 判断，只保证不拦）。
func fileListQuery(r *http.Request) filemgr.ListOptions {
	q := r.URL.Query()
	opts := filemgr.ListOptions{
		Sort:   q.Get("sort"),
		Desc:   q.Get("order") == "desc",
		Hidden: truthy(q.Get("show_hidden")),
	}
	// 显式清空未知值不做：filemgr.sortRows 对空串与未知值都走名称序分支。
	if n, err := strconv.Atoi(q.Get("page")); err == nil {
		opts.Page = n
	}
	if n, err := strconv.Atoi(q.Get("size")); err == nil {
		opts.Size = n
	}
	return opts
}

// truthy 认 "1" 与 "true"（大小写不敏感）。
//
// 为什么要有 "true"：布尔查询参数写成 show_hidden=true 是常见写法，
// 只认 "1" 会得到"设置了但没生效"这种静默失效 —— 用户以为隐藏文件
// 打开了，实际列表里还是没有，谁会怀疑是查询参数的解析？
// 再多的别名（yes/on/y）一律不认：每多一个不被任何调用方使用的写法，
// 就多一处只存在于注释里的行为。
func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true":
		return true
	}
	return false
}

// queryPath 取出 ?path= 并按"缺参 = 非法"处理。
//
// 绝不在这里给默认值（根目录或进程 cwd）：根目录会让手机用户一打开文件页
// 就站在 / 上（一屏系统目录、什么都干不了），cwd 解释则是 M6-T1 就立过的
// 禁令（同一个参数在开发机与目标机指向不同目录）。起始目录是设置项，
// 由前端带上。
func queryPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeError(w, http.StatusBadRequest, "bad_path", "缺少 path 参数")
		return "", false
	}
	return p, true
}

func handleFSList(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dir, ok := queryPath(w, r)
		if !ok {
			return
		}
		page, err := svc.List(r.Context(), dir, fileListQuery(r))
		if err != nil {
			writeFSError(w, err)
			return
		}
		// Entries 为 nil 时序列化成 []：前端直接 entries.length / .map，
		// null 会抛 TypeError 把整个文件页打成白屏。
		if page.Entries == nil {
			page.Entries = []filemgr.Entry{}
		}
		writeJSON(w, http.StatusOK, page)
	}
}

func handleFSStat(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := queryPath(w, r)
		if !ok {
			return
		}
		e, err := svc.Stat(r.Context(), p)
		if err != nil {
			writeFSError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, e)
	}
}

func handleFSRoots(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roots, err := svc.Roots(r.Context())
		if err != nil {
			writeFSError(w, err)
			return
		}
		if roots == nil {
			roots = []filemgr.Root{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"roots": roots})
	}
}

type fsPathJSON struct {
	Path string `json:"path"`
}

type fsRenameJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func handleFSMkdir(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in fsPathJSON
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if err := svc.Mkdir(r.Context(), in.Path); err != nil {
			writeFSError(w, err)
			return
		}
		// 201 而不是 200：前端拿它区分"新建出来了"与"什么都没发生"。
		// os.MkdirAll 对已存在目录返回 nil，如果这里回 200，用户点两次
		// "新建文件夹"会以为新建成功了，而其实他进了一个同名旧目录。
		writeJSON(w, http.StatusCreated, map[string]any{"path": in.Path})
	}
}

func handleFSRename(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in fsRenameJSON
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if strings.TrimSpace(in.To) == "" {
			writeError(w, http.StatusBadRequest, "bad_path", "缺少 to 参数")
			return
		}
		if err := svc.Rename(r.Context(), in.From, in.To); err != nil {
			writeFSError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": in.To})
	}
}

// writeFSError 把 filemgr 的领域错误翻成状态码。
//
// 顺序很重要：ErrBadPath / ErrNotDirectory 这些**自定义**错误内部会
// 包装 syscall 错误（fmt.Errorf("%w: ... (%v)")），如果先判 fs.ErrNotExist
// 之类的底层哨兵，一个"穿过普通文件"的 ENOTDIR 会先撞上 fs.ErrNotExist
// 分支被报成 404"文件不存在" —— 而用户看着那个文件在眼前，
// 一句自相矛盾的报错比不报错更糟。所以领域错误一律先判。
//
// 兜底是 500 而不是 400：未知错误就是未知。把它归成"你的路径不对"
// 会让人反复改一个本来正确的路径，而真正的原因（权限模型、磁盘故障、
// 内核错误）永远没人去看。Detail 里带上原始错误文本：远程运维时
// 屏幕上的那一行就是全部线索。
func writeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, filemgr.ErrBadPath):
		writeError(w, http.StatusBadRequest, "bad_path", err.Error())
	case errors.Is(err, filemgr.ErrNotDirectory):
		writeError(w, http.StatusBadRequest, "not_directory", err.Error())
	case errors.Is(err, filemgr.ErrIsDirectory):
		// 与 ErrNotDirectory 是两个方向相反的判断，不能合并成一个错误：
		// 那句提示的意思正好相反，合并的话必有一侧对用户说反话。
		// 回 400 而不是 404 —— 目录确实在，只是不能这么下载。
		writeError(w, http.StatusBadRequest, "is_directory", err.Error())
	case errors.Is(err, filemgr.ErrTrashUnwritable):
		// 422 而不是 500：500 的意思是"面板坏了"，而这里坏的是环境
		// （盘根只读 / 没权限），面板的行为完全正确 —— 它拒绝了一次会
		// 变成跨盘复制的删除。前端要靠这个 code 把唯一可行的替代动作
		// （永久删除，二次确认）摆到用户面前；没有专属 code 的话用户
		// 只看到一个红框，下一步什么都做不了。
		// 也不用 403：403 在 HTTP 语义里是"没登录/没权限"，而用户确实是
		// 管理员，只是这块文件系统不让他这么删。
		writeError(w, http.StatusUnprocessableEntity, "trash_unwritable", err.Error())
	case errors.Is(err, filemgr.ErrExists):
		// 409：前端按它弹"重命名建议 / 是否覆盖"，而不是把红字甩在脸上。
		// 这也是 rename 唯一的护栏（Unix rename(2) 默认覆盖目标，D14 下
		// 没有系统层会拦），所以它必须是可区分的状态码。
		writeError(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, fs.ErrPermission):
		writeError(w, http.StatusForbidden, "permission_denied", err.Error())
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeErrorDetail(w, http.StatusInternalServerError, "fs_error",
			"文件系统操作失败", err.Error())
	}
}
