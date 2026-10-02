package filemgr

// 回收站（设计 8.6 / D12）。
//
// 形状被一条实测结论重写了。原设计把回收站放在**一个**目录里（默认
// /DISK/.trash），那意味着删掉 /data 上的一个 10GB 文件要把它**完整
// 复制到另一个分区**再删源：用户按"删除"预期的是瞬间完成，实际得到的
// 是一次双盘擦写（源盘读 10GB + 目标盘写 10GB，之后清理还要再删一次），
// 机械盘上几分钟，而中途断电留下两份都不完整的副本。
//
// 改成**每个盘用自己的回收站**：<该文件所在盘根>/<回收站目录名>/。
// 于是删除永远是同一文件系统内的 rename(2) —— 瞬间、零额外擦写、天然
// 原子；设计里"回收站分区空间不足"这个失败模式在结构上不再存在（同盘
// rename 不占新空间）。
//
// 代价说清楚：
//   - 条目散落在各盘根目录，"查看回收站"要遍历所有盘；
//   - 显示隐藏文件时，每个盘根目录多出一个 .trash；
//   - 盘根目录写不进去时（Android 上非 root 的 /data 就是如此，实测
//     EACCES）删除**必须失败**并说明原委，绝不能退化成跨盘复制 ——
//     那正是用户要求避免的无意义擦写，而且它会慢到像卡死。
//
// 实测事实（本机 f2fs/fuse，探针已删，结论在此）：
//   - 同盘 rename 一棵 54 条目的目录树：瞬间完成，条目原样带过去；
//   - rename 保持 inode（2662289→2662289），所以"inode 前后相同"是
//     区分 rename 与 copy+unlink 的可靠观测；
//   - stat().Dev 能可靠区分文件系统（/=65034、/data=65078、
//     /storage/emulated/0=177 是 fuse、/dev=16）；
//   - 单个路径名 255 字节上限是真的：360 字节的扁平化路径名 →
//     ENAMETOOLONG，所以条目名不能塞完整原路径；
//   - 盘根目录 chmod 0500 后在其下建目录 → EACCES，而其下的子目录仍
//     可读写（"回收站建不起来"与"文件移不出去"是两个独立事实）；
//   - os.Link 在同目录内也 EACCES（Android 禁 hardlink），所以"给条目
//     建个链接留证据"这类做法不能要。
//
// 条目布局：<盘根>/.trash/<ID>（**载荷本身**）+ <盘根>/.trash/<ID>.meta.json
// （同级边文件）。载荷直接占条目名、元信息放旁边，而不是"每个条目一个
// 目录、里面装 meta 和 payload"：前者让 TrashItem.Path 恰好等于被移动的
// 那个东西，于是"移动不改变一棵树的条目数"这种断言直接成立，人手工进
// 回收站里翻也一眼看得懂。
//
// 认条目靠的是**旁边有没有一份可信的 <载荷名>.meta.json**，不是靠名字
// 长得像，也不是靠载荷是文件还是目录（两者都可能是条目）。这一
// 条决定了清理/清空的安全性：回收站是个普通目录，用户完全可以往里手放
// 东西（他刚 SFTP 传了个备份、或者手工 mv 了点东西进去）。靠名字认条目
// 的实现会把"我自己放的目录"当成过期条目删掉 —— 那是数据丢失，而且
// 发生在"清理回收站"这个本应最安全的动作里。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"litepanel/internal/metrics"
)

// 回收站的默认参数（设计 8.6）。
const (
	// DefaultTrashDirName 是每个盘根目录下的回收站目录名。
	DefaultTrashDirName = ".trash"
	// DefaultTrashRetain 是条目保留期。
	DefaultTrashRetain = 72 * time.Hour
	// min/maxTrashRetain 是设置页的取值边界（设计：1–90 天）。
	minTrashRetain = 24 * time.Hour
	maxTrashRetain = 90 * 24 * time.Hour
	// trashDirPerm 用 0700：回收站里的内容正是"用户以为自己删掉了"的
	// 东西（日志、配置、备份），别人能翻出来等于没删。原权限位记在
	// 条目自己的 mode 里，但还原时不恢复属主/权限 —— 面板以 root 跑，
	// 恢复一个 0600 的私钥对"误删找回"这个目的没有意义，而
	// chown 错对象反而危险。
	trashDirPerm = 0o700
	// maxIDHint 是条目名里那段可读提示的字节上限。
	maxIDHint = 48
	// maxIDLen 是条目名的总长度上限。文件系统单名上限 255 字节（实测），
	// 而条目名还要 +".meta.json"（11 字节）作为边文件名，所以这里留出
	// 富余取 200。
	maxIDLen = 200
)

// ErrTrashUnwritable 表示"这个盘的回收站建不起来"。
//
// 有独立哨兵是因为它是**唯一**一条"面板本可以偷偷做个更慢的事"的路径：
// 盘根目录写不进去时，跨盘复制一份到别处的回收站在技术上行得通，而那
// 恰恰是用户明确不要的无意义擦写。必须让它成为显式失败（403/409 一类），
// 并且文案里给出替代动作（永久删除，二次确认）。
var ErrTrashUnwritable = errors.New("回收站不可写")

// TrashItem 是回收站里的一条。
type TrashItem struct {
	// ID 是条目在回收站目录里的名字，同时就是它的唯一标识（API 路径参数）。
	ID string `json:"id"`
	// Name 是**原**文件名的 basename，界面主列显示它。
	Name string `json:"name"`
	// Origin 是原绝对路径；还原时要回到这里。
	Origin string `json:"origin"`
	// Path 是条目载荷的绝对路径（回收站内）。
	Path string `json:"path"`
	// Mount 是它属于哪个盘。界面按盘分组，也用来解释"这个条目在 /DISK
	// 上"（移动硬盘拔掉后条目会读不到，这时这个字段就是全部线索）。
	Mount string `json:"mount"`
	// IsDir 区分条目是目录。
	IsDir bool `json:"is_dir"`
	// Size 是文件字节数；**目录恒为 0**。
	//
	// 不是没实现，是故意不算：算一个目录的大小要遍历整棵树，而"删掉
	// node_modules"就会变成 O(文件数) 的遍历 —— 那正是"删除应当瞬间
	// 完成"被打破的地方。列举侧（browse.go）对目录同样给 0，两边一致。
	Size int64 `json:"size"`
	// DeletedAt 是删除时刻（unix 秒），保留期按它算。
	DeletedAt int64 `json:"deleted_at"`
}

// trashMeta 是条目边文件的内容。
//
// 与 TrashItem 分开是因为契约里要的字段与恢复所需的字段不同：meta 要带
// 载荷是文件还是目录、原始权限，而响应里不该出现"载荷在盘上的绝对路径"
// 之外的内部信息。有了独立类型，改 JSON 契约不会连带改盘上格式（反之
// 亦然 —— 盘上格式一改，老面板留下的条目就再也读不出来）。
type trashMeta struct {
	Name      string `json:"name"`
	Origin    string `json:"origin"`
	IsDir     bool   `json:"is_dir"`
	Size      int64  `json:"size"`
	DeletedAt int64  `json:"deleted_at"`
}

// rootFunc 查一个路径所属文件系统的**锚点**（该文件系统树里最浅的可访问
// 目录，实践中就是挂载点）。
type rootFunc func(path string) (string, error)

// trashRootsFunc 枚举"要管哪些盘"。
type trashRootsFunc func(ctx context.Context) ([]string, error)

// rootFinder 决定用真实锚点查找还是注入的。
func rootFinder(override func(string) (string, error)) rootFunc {
	if override != nil {
		return override
	}
	return realFilesystemRoot
}

// realFilesystemRoot 向上走祖先、比较设备号，找出"父目录已经在另一个
// 文件系统上"的那一层。
//
// 用设备号而不是查 /proc/mounts：后者要把用户路径与挂载表做前缀匹配，
// 而挂载表里有 bind mount、subvolume、overlay，同一路径可能匹配到多条
// 记录（匹配到哪条取决于行序）；设备号是内核给的事实，一个字都不用来
// 解释。这也让"这个文件在哪个盘"与"这块盘在挂载表里叫什么"解耦 ——
// 前者删除时必须准，后者只是展示。
//
// 路径末段不存在也要能定锚点（往上找到第一个存在的祖先就行）：删除一个
// 刚被别处删掉的文件时，我们仍然需要知道它本来在哪个盘。
func realFilesystemRoot(path string) (string, error) {
	return walkToAnchor(path, func(p string) (uint64, pathState) {
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil {
			// 区分"明确不存在"与"判不出来"（权限）：前者要继续往上退，
			// 后者意味着能确认的最深一层就是这里。
			if errors.Is(err, fs.ErrNotExist) {
				return 0, pathGone
			}
			return 0, pathUnknown
		}
		return st.Dev, pathHere
	})
}

// walkToAnchor 是锚点行走的内核：devOf(path) 回（设备号, 是否存在, 是否可判定）。
//
// 内核单独抽出来是为了能让**合成**的文件系统层次来测。真实 syscall 版本
// 只能在本机上测，而本机可能整台只有一个文件系统 —— 那时"往上走多了一层
// （把 /data 上的文件认成 / 上的）"这类错误在真实环境的测试里根本不会
// 显现：锚点是不是真的那个挂载点，取决于跑测试的机器长什么样。有了这张
// 表，多盘、单盘、父目录读不到，都是可以在任何机器上钉住的用例。
func walkToAnchor(path string, devOf func(string) (uint64, pathState)) (string, error) {
	cur := filepath.Clean(path)
	// 1) 往上退到第一个拿得到设备号的祖先
	var dev uint64
	for {
		d, state := devOf(cur)
		if state == pathHere {
			dev = d
			break
		}
		if state == pathGone { // 明确不存在：再往上退一层
			parent := filepath.Dir(cur)
			if parent == cur {
				return "", fmt.Errorf("%w: %q 往上到根都不存在", ErrBadPath, path)
			}
			cur = parent
			continue
		}
		// pathUnknown（EACCES：中间某个 0700 的目录）：它是否属于目标
		// 文件系统未知，但已经是能确认的最深一层，就地当锚点。
		return cur, nil
	}
	anchor := cur
	// 2) 往上走，直到父目录换了设备
	for {
		parent := filepath.Dir(anchor)
		if parent == anchor {
			return anchor, nil
		}
		pdev, state := devOf(parent)
		if state != pathHere {
			// 父目录读不到 / 判不准：停在能确认的最深层。
			return anchor, nil
		}
		if pdev != dev {
			return anchor, nil
		}
		anchor, dev = parent, pdev
	}
}

// pathState 是对一个路径 stat 的三种可区分结果。
type pathState int

const (
	pathHere    pathState = iota // 拿得到设备号
	pathGone                     // 明确不存在（可继续往上退）
	pathUnknown                  // 判不出来（权限等），只能就地停住
)

// discoverTrashRoots 从挂载表枚举要管的盘。
//
// 这里**必须**复用 metrics.FilterRealMounts 而不是自己筛（同 Roots：
// R9 的教训，伪文件系统黑名单注定不完整）。按设备去重也顺带解决了
// 同盘多挂载点：夹具里 /DISK 与 /mnt/disk-mirror 同为 /dev/sda1，去重
// 之后只在一个地方建回收站 —— 否则同一个盘会出现两个回收站，而
// "清空回收站"只清了一个，用户看到的另一半条目永远清不掉。
func (s *Service) discoverTrashRoots(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mounts, err := metrics.ReadMounts(s.procDir)
	if err != nil {
		return nil, err
	}
	real := metrics.FilterRealMounts(mounts)
	out := make([]string, 0, len(real))
	for _, m := range real {
		out = append(out, m.Mountpoint)
	}
	if len(out) == 0 {
		// 与 Roots 同样的兜底理由：一个真盘都没枚举到时，回收站至少
		// 要管面板自己所在的那个盘，否则"删除"在这台机器上永远失败。
		out = append(out, "/")
	}
	return out, nil
}

// trashRoot 拼出某个盘的回收站目录。
func (s *Service) trashRoot(mount string) string {
	return filepath.Join(mount, s.trashDirName)
}

// ensureTrashDir 建出（或确认存在）某个盘的回收站目录。
//
// 建不起来时必须报 ErrTrashUnwritable：这是全设计里唯一"技术上还有别的
// 办法"的失败点，而那个别的办法（挪到能写的盘的回收站）正是禁止的跨盘
// 复制。所以这里不仅要失败，还要在文案里给出去路（永久删除 / SFTP），
// 让用户知道面板没有卡住，只是这个盘不让写。
func (s *Service) ensureTrashDir(ctx context.Context, mount string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := s.trashRoot(mount)
	if err := os.MkdirAll(dir, trashDirPerm); err != nil {
		// MkdirAll 的 *PathError 里带着 EACCES/EROFS，包一层就够定位。
		return "", fmt.Errorf("%w: %s 建 %s 失败（%v）；这个盘上的文件只能永久删除，或用 SFTP 移走",
			ErrTrashUnwritable, mount, s.trashDirName, err)
	}
	// 再显式 chmod 一次：MkdirAll 的 mode 还要被进程 umask 削一道，而
	// umask 取决于面板是被 systemd 拉起还是从一个 shell 里手起的 ——
	// 一个"已删除内容对同机其它账号可读"的回收站不该由启动方式来决定。
	// 已存在的 .trash 也一并收紧（可能是老版本或用户自己建的 0755）。
	if err := os.Chmod(dir, trashDirPerm); err != nil {
		return "", fmt.Errorf("%w: %s 收紧 %s 权限失败（%v）",
			ErrTrashUnwritable, mount, s.trashDirName, err)
	}
	return dir, nil
}

// Delete 把文件/目录移入**它所在盘**的回收站。
//
// 全程只有一次 rename(2)：不复制、不遍历、不看目录有多大。这是设计里
// "删除要瞬间完成"唯一的实现方式，也是"回收站不跨盘"这条裁定的全部
// 意义（用户的原话：避免无意义的磁盘擦写）。
//
// 写入顺序是按"崩在中间会留下什么"定的：
//  1. 用 O_CREATE|O_EXCL 原子地创建 meta 并写全 —— 一步同时占了条目名
//     （同一秒的并发删除各得一个后缀）又留下了元信息；
//  2. 最后 rename 载荷 —— 这一步成功才算删除成功，失败则删掉 meta 回滚。
//
// 于是任何一步之间断电都不会造成损失：载荷要么在原处（源完好），要么在
// 条目名里（元信息已经先在那儿了）。唯一可能的残留是一个写了一半的
// meta 文件，而它既不会被认成条目（认条目要求同名的**目录**载荷），
// 也不会删到任何东西。
//
// 占位为什么不是 Mkdir 一个条目目录：载荷可能就是普通文件，而 rename
// 一个文件到一个已存在的**空目录**上是 EISDIR —— 占位目录本身会让删除
// 文件这件事必然失败。
func (s *Service) Delete(ctx context.Context, path string) (TrashItem, error) {
	if err := ctx.Err(); err != nil {
		return TrashItem{}, err
	}
	real, mount, st, err := s.deletable(path)
	if err != nil {
		return TrashItem{}, err
	}
	base := filepath.Base(real)

	item := TrashItem{
		Name: base, Origin: real, Mount: mount,
		IsDir: st.Mode&syscall.S_IFMT == syscall.S_IFDIR, DeletedAt: s.clock().Unix(),
	}
	if !item.IsDir {
		item.Size = st.Size
	}

	dirPath, err := s.ensureTrashDir(ctx, mount)
	if err != nil {
		return TrashItem{}, err
	}
	id, metaPath, err := s.claimEntry(ctx, dirPath, real, trashMeta{
		Name: item.Name, Origin: item.Origin, IsDir: item.IsDir,
		Size: item.Size, DeletedAt: item.DeletedAt,
	})
	if err != nil {
		return TrashItem{}, err
	}
	entryPath := strings.TrimSuffix(metaPath, metaSuffix)
	if err := os.Rename(real, entryPath); err != nil {
		// 回滚掉占位，失败的删除不在回收站里留痕迹。
		_ = os.Remove(metaPath)
		if errors.Is(err, fs.ErrNotExist) {
			return TrashItem{}, fmt.Errorf("%w: %q", fs.ErrNotExist, real)
		}
		// 同盘 rename 失败的基本是权限/只读一类（EXDEV 不可能：锚点
		// 相同）。**不**改投别的盘的回收站 —— 见 ErrTrashUnwritable。
		return TrashItem{}, fmt.Errorf("%w: %q 移入回收站失败（%v）", ErrTrashUnwritable, real, err)
	}
	item.ID = id
	item.Path = entryPath
	return item, nil
}

// claimEntry 原子地占下一个条目名并写好它的 meta，返回（ID, meta 路径）。
//
// 靠 O_CREATE|O_EXCL 而不是"造个唯一名然后指望它唯一"：open 在内核层面
// 要么创建成功要么 EEXIST，两个并发删除撞上同一个名字时后一个换后缀，
// 而不是把前一个的条目覆盖掉（覆盖了就是数据丢失）。
func (s *Service) claimEntry(ctx context.Context, dirPath, origin string, m trashMeta) (string, string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", "", err
	}
	base := trashID(origin, m.DeletedAt)
	for n := 0; n < 1000; n++ {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		id := base
		if n > 0 {
			id = base + "-" + strconv.Itoa(n)
		}
		if len(id) > maxIDLen {
			id = id[:maxIDLen]
		}
		metaPath := filepath.Join(dirPath, id+metaSuffix)
		f, err := os.OpenFile(metaPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.Write(b)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(metaPath)
				return "", "", fmt.Errorf("%w: 写回收站元信息失败（%v）", ErrTrashUnwritable, err2(werr, cerr))
			}
			return id, metaPath, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", "", fmt.Errorf("%w: 建回收站条目失败（%v）", ErrTrashUnwritable, err)
		}
	}
	return "", "", fmt.Errorf("%w: 同一秒内同一路径的删除条目过多", ErrTrashUnwritable)
}

// err2 挑出两个错误里非 nil 的那个（写失败与关闭失败都算失败，报第一
// 个能说明问题的就行）。
func err2(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// trashID 造一个既全局唯一、又人能看懂一点的条目名。
//
// 三段的由来：
//   - unix 秒：让条目按名字排序就是按删除时间排序，人 ls 一眼看出哪个新；
//   - 原路径的可读提示（只留 ASCII alnum 与 ._-，截断）：管理员 SSH 进去
//     看到 nginx-conf 而不是裸哈希。中文名会被过滤空，那时宁可少一段 ——
//     条目名要当 API 路径参数用，非 ASCII 在里面会引来一连串编码歧义；
//   - 原绝对路径的哈希：唯一性的来源。只拿"时间 + 相对路径"会在两块盘
//     同一秒删掉同名文件时撞车，而撞了以后还原一个会带走另一个。
func trashID(origin string, at int64) string {
	sum := sha256.Sum256([]byte(origin))
	hash := hex.EncodeToString(sum[:])[:8]
	hint := idHint(origin)
	if hint == "" {
		return strconv.FormatInt(at, 10) + "-" + hash
	}
	return strconv.FormatInt(at, 10) + "-" + hint + "-" + hash
}

// idHint 从原路径里榨出一段 ASCII 可读提示。
func idHint(origin string) string {
	var b strings.Builder
	for i := len(origin) - 1; i >= 0 && b.Len() < maxIDHint; i-- {
		c := origin[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '.', c == '-', c == '_':
			b.WriteByte(c)
		default:
			// 非 ASCII（中文/空格/斜杠）一律丢掉，不替换成 '-'：
			// 中文名会得一串横线，比什么都没有更吵。
		}
	}
	out := []rune(b.String())
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return strings.Trim(string(out), "._-")
}

// metaSuffix 是条目边文件的后缀。条目载荷就叫 <ID>，meta 是 <ID>+此后缀。
const metaSuffix = ".meta.json"

// metaPathOf 是条目载荷对应的边文件路径。
func metaPathOf(entryPath string) string { return entryPath + metaSuffix }

// readTrashMeta 读一个条目的元信息。
//
// 返回 error 表示"这不是面板放的条目"，调用方据此跳过。这不是懒，而是
// 回收站里"用户自己放的东西"必须被无差别跳过的直接后果：判定标准只能
// 是"读得出可信元信息"，任何按目录名的猜测都会误伤真实用户文件。
func readTrashMeta(entryPath string) (trashMeta, error) {
	var m trashMeta
	b, err := os.ReadFile(metaPathOf(entryPath))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	// Origin 为空/非绝对的 meta 一定是坏文件：照着它还原会在奇怪的地方
	// 建目录，宁可当它不存在。
	if m.Origin == "" || !filepath.IsAbs(m.Origin) {
		return m, fmt.Errorf("条目元信息不完整")
	}
	return m, nil
}

// ListTrash 列出所有盘回收站里的条目。
//
// 按盘分置换来的代价就是这里要遍历。条目按删除时间**倒序**返回：回收站
// 抽屉里"最近删掉的在最上面"是直觉，而顺序必须有确定性 —— 每次刷新换序
// 会让用户以为条目在增减。时间相同时用 ID 决胜，否则同秒删除的两条在
// map 遍历顺序下每次不同。
func (s *Service) ListTrash(ctx context.Context) ([]TrashItem, error) {
	out := make([]TrashItem, 0, 8)
	err := s.forEachEntry(ctx, func(e entryRef) error {
		out = append(out, e.item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeletedAt != out[j].DeletedAt {
			return out[i].DeletedAt > out[j].DeletedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// entryRef 是一个已认出条目的现场：条目 + 它在盘上的两个路径。
type entryRef struct {
	item     TrashItem
	metaPath string
}

// forEachEntry 遍历所有盘的回收站，只对**认得出的条目**回调。
//
// 列举、清理、永久删除前的查找共用这一条识别路径 —— 必须共用：三个动作
// 对"什么算条目"的理解不一致时，最坏的组合是"列举里看得见、清理里当成
// 用户文件跳过"（清不掉）或反过来（把用户文件删了）。
//
// 某个盘的回收站目录不存在不是错误（全新机器的正常状态）；读目录失败
// （拔掉的移动硬盘留下的死挂载点）也不是错误，跳过该盘继续 —— 回收站
// 抽屉因为一块不存在的盘而整体打不开，用户就看不出是哪一个盘的问题。
func (s *Service) forEachEntry(ctx context.Context, fn func(entryRef) error) error {
	roots, err := s.trashRoots(ctx)
	if err != nil {
		return err
	}
	for _, mount := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := s.trashRoot(mount)
		des, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, de := range des {
			// 边文件本身不是条目。
			if strings.HasSuffix(de.Name(), metaSuffix) {
				continue
			}
			entryPath := filepath.Join(dir, de.Name())
			m, err := readTrashMeta(entryPath)
			if err != nil {
				continue
			}
			if err := fn(entryRef{
				item: TrashItem{
					ID: de.Name(), Name: m.Name, Origin: m.Origin, Path: entryPath,
					Mount: mount, IsDir: m.IsDir, Size: m.Size, DeletedAt: m.DeletedAt,
				},
				metaPath: metaPathOf(entryPath),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// findEntry 按 ID 定位一个条目。
//
// ID 来自 URL 路径参数，而它会被直接拼进文件路径 —— 所以先做字符集校验
// 再去盘上找。`..` 或带斜杠的 ID 不加校验就能读出（甚至删掉）回收站外
// 的 meta/目录，那是一条从 API 通往任意路径的路。
func (s *Service) findEntry(ctx context.Context, id string) (entryRef, error) {
	if err := cleanTrashID(id); err != nil {
		return entryRef{}, err
	}
	var found entryRef
	var hit bool
	if err := s.forEachEntry(ctx, func(e entryRef) error {
		if !hit && e.item.ID == id {
			found, hit = e, true
		}
		return nil
	}); err != nil {
		return entryRef{}, err
	}
	if !hit {
		return entryRef{}, fmt.Errorf("%w: 回收站条目 %q", fs.ErrNotExist, id)
	}
	return found, nil
}

// cleanTrashID 校验条目名的字符集与长度。
func cleanTrashID(id string) error {
	if id == "" || len(id) > maxIDLen {
		return fmt.Errorf("%w: 回收站条目 id %q", ErrBadPath, id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return fmt.Errorf("%w: 回收站条目 id %q 含非法字符", ErrBadPath, id)
		}
	}
	// 全是点的 id（"." / ".." / "..."）字符集合法而语义上是跳转，单独挡。
	if strings.Trim(id, ".") == "" {
		return fmt.Errorf("%w: 回收站条目 id %q", ErrBadPath, id)
	}
	return nil
}

// RestoreTrash 把条目还原回原路径，返回还原后的路径。
//
// 原目录已经没了要补建父目录："删了整个文件夹、后来想找回其中一个文件"
// 是回收站最常见的用法，要求用户先手工把目录建回来等于让还原在这个场景
// 里失效。
//
// 原路径已被占用则拒绝（ErrExists）而**条目必须留在回收站**：静默改名会
// 造出"报告 (1).txt"，用户以为找回来了而原件还在原地；静默覆盖则是吃掉
// 别人的新文件；而报着"失败"又把条目删掉，等于把这次还原变成真删除。
func (s *Service) RestoreTrash(ctx context.Context, id string) (string, error) {
	e, err := s.findEntry(ctx, id)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target := e.item.Origin
	if parent := filepath.Dir(target); !dirExists(parent) {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			// 父目录建不起来时条目仍在回收站：还原失败只该回报失败，
			// 不该有任何副作用。
			return "", fmt.Errorf("重建 %s 失败（%v）", parent, err)
		}
	}
	// 已存在就拒绝。用 Lstat 而不是 Stat：占位的也可能是一个坏符号链接，
	// Stat 会跟着指过去并把它当成"不存在"。
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%w: %q 已存在，先把它删掉或改名再还原", ErrExists, target)
	}
	// 同盘 rename（条目与 Origin 必然同盘 —— 条目就建在 Origin 所在盘
	// 的根上），所以还原同样是瞬间的。
	if err := os.Rename(e.item.Path, target); err != nil {
		return "", fmt.Errorf("还原到 %q 失败（%v）", target, err)
	}
	_ = os.Remove(e.metaPath)
	return target, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// PurgeTrash 永久删除单个条目（前端二次确认之后才调）。
func (s *Service) PurgeTrash(ctx context.Context, id string) error {
	e, err := s.findEntry(ctx, id)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return removeEntry(e)
}

// EmptyTrash 清空所有盘的回收站，返回删掉的条数（界面显示"已清空 N 项"）。
func (s *Service) EmptyTrash(ctx context.Context) (int, error) {
	return s.dropEntries(ctx, func(entryRef) bool { return true })
}

// CleanTrash 把超过保留期的条目永久删除，返回删掉的条数。
//
// 与上传的 GCUploads 同构：由 cmd/litepanel 的定时任务调用，判定用注入
// 时钟，测试里推进假时钟就能测到过期，不必真等 3 天。
//
// 边界是**严格大于**保留期才删：写成 >= 会让一个"刚好 3 天"的文件在用户
// 第三次打开回收站时消失，而他会坚持说昨天看还在 —— 这类"差一个等号"
// 的差别在真实使用中无法与 bug 区分，所以它需要一条测试钉住方向。
func (s *Service) CleanTrash(ctx context.Context) (int, error) {
	cutoff := s.clock().Add(-s.trashRetain).Unix()
	return s.dropEntries(ctx, func(e entryRef) bool { return e.item.DeletedAt < cutoff })
}

// dropEntries 删除满足 keep=false 的条目。
func (s *Service) dropEntries(ctx context.Context, doomed func(entryRef) bool) (int, error) {
	n := 0
	// 先收集再删：遍历目录的同时删条目依赖 ReadDir 的返回快照，
	// 先把要删的攒下来再去删，就不需要依赖那个隐含保证。
	var list []entryRef
	if err := s.forEachEntry(ctx, func(e entryRef) error {
		if doomed(e) {
			list = append(list, e)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	for _, e := range list {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if err := removeEntry(e); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// removeEntry 永久删除一个条目（载荷 + 边文件）。
func removeEntry(e entryRef) error {
	if err := os.RemoveAll(e.item.Path); err != nil {
		return fmt.Errorf("永久删除 %q 失败（%v）", e.item.Path, err)
	}
	if err := os.Remove(e.metaPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("删除回收站元信息失败（%v）", err)
	}
	return nil
}

// DeleteMany 删除一批路径：permanent=false 移入各自盘的回收站，true 直接
// 永久删除（盘根不可写时用户的唯一出路）。
//
// **先全部校验、再全部动手**：批次里只要有一个不存在/不合法，就整批不
// 处理并回那个错误。部分成功是最坏结果 —— 界面上报了错，而实际上有些
// 文件已经动了，用户无从知道到底哪几个受影响。先跑一遍 Lstat 把这个
// 窗口关到"校验与执行之间恰好被别处删掉"那种真正的竞态（此时第二遍的
// Delete 会报 ErrNotExist，那是无法在无锁下避免的，且极少见）。
//
// permanent 与进回收站是**两个**危险等级：前者不可撤销。但两者都在这
// 里、都过同一套路径校验（末段不解析符号链接、拒绝删除回收站自身与
// 挂载点），因为 permanent 去掉的只是"能不能还原"，不该顺带去掉其它
// 任何一道护栏。
func (s *Service) DeleteMany(ctx context.Context, paths []string, permanent bool) (int, error) {
	return s.deleteMany(ctx, paths, permanent, nopProgress)
}

// deleteMany 是 DeleteMany 的可上报进度内核（job 执行器要走这条路）。
//
// 拆两层而不是给 DeleteMany 加个可选参数：删除五千个文件要一分多钟，队列
// 必须能报"已删 1200/5000"，否则界面是一个几分钟不动的进度条，用户只会
// 以为面板挂了。同步调用方（HTTP 旧路径）传 nopProgress，行为一字不差。
func (s *Service) deleteMany(ctx context.Context, paths []string, permanent bool, report progressFunc) (int, error) {
	if len(paths) == 0 {
		return 0, fmt.Errorf("%w: 没有要删除的路径", ErrBadPath)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// 第一遍：把每条解析成确定路径并确认可删除（存在 + 不是回收站自身）。
	targets := make([]string, 0, len(paths))
	for _, p := range paths {
		real, _, _, err := s.deletable(p)
		if err != nil {
			return 0, err
		}
		targets = append(targets, real)
	}
	n := 0
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		var err error
		if permanent {
			err = s.removePermanent(t)
		} else {
			_, err = s.Delete(ctx, t)
		}
		if err != nil {
			return n, err
		}
		n++
		// 传**累计**条目数 n 而不是 1：progressFunc 的约定是累计值，
		// 节流器拿它跟上次做差。传 1 会让 pendEntries 永远是 1，
		// 抽屉里的"已删 N"卡在一动不动。
		if err := report(0, n); err != nil {
			return n, err
		}
	}
	return n, nil
}

// deletable 把"这个路径能不能删"回答完，返回确定路径、所在盘与 stat。
//
// Delete 与 DeleteMany 都只调它：两处各写一份判定早晚会漂，而漂了的
// 后果是批量接口把"整批拒绝"的陈诺偷换成"删到那条才失败"。
//
// 末段不解析符号链接（删链接要删链接本身，见 SplitResolved）。
func (s *Service) deletable(p string) (real, mount string, st syscall.Stat_t, err error) {
	dir, base, err := SplitResolved(p)
	if err != nil {
		return "", "", st, err
	}
	if base == "." || base == "/" {
		return "", "", st, fmt.Errorf("%w: 不能删除 %q", ErrBadPath, p)
	}
	real = join(dir, base)
	if err := syscall.Lstat(real, &st); err != nil {
		// 报 fs.ErrNotExist（404）而不是"已移入回收站"：后者会让实现
		// 建出一条指向虚空的条目，而还原它必然失败。
		return "", "", st, fmt.Errorf("%w: %q", fs.ErrNotExist, real)
	}
	mount, err = s.fsRoot(real)
	if err != nil {
		return "", "", st, err
	}
	// 盘根是锚点：删了它等于这块盘"没了"，而它的回收站又建在它自己身上，
	// 这个状态不能进入。
	if real == mount {
		return "", "", st, fmt.Errorf("%w: 不能删除挂载点 %q", ErrBadPath, mount)
	}
	// 不许把回收站自己（或它里面的任何东西）丢进回收站：那会造出一条
	// 自我包含的条目，遍历与还原都解不了，而它永远清不掉。
	if trash := s.trashRoot(mount); real == trash || strings.HasPrefix(real, trash+string(os.PathSeparator)) {
		return "", "", st, fmt.Errorf("%w: %q 已经在回收站里", ErrBadPath, real)
	}
	return real, mount, st, nil
}

// removePermanent 永久删除一个已校验过的路径。
func (s *Service) removePermanent(real string) error {
	if err := os.RemoveAll(real); err != nil {
		return fmt.Errorf("永久删除 %q 失败（%v）", real, err)
	}
	return nil
}

// PreflightTrash 在**受理**一批删除时检查"这些盘的回收站建得起来吗"。
//
// 为什么需要它（而删除本身在执行时也会遇到同一个问题）：界面上"这个盘只
// 能永久删除"那个按钮，靠的是 HTTP 层拿到 ErrTrashUnwritable 这个**哨兵**
// （errors.Is）才亮得起来。删除改走队列之后，这个错误本来只出现在任务
// 执行时（抽屉里一条红色记录），前端就只剩"对中文错误文本做子串匹配"一条
// 路可走 —— 而那条路在本项目里被明令禁止：改一句文案就会把 422 变成没有。
// 与其让前端去猜文本，不如把判定提前到受理时，让哨兵仍然走 HTTP。
//
// 这与"受理时顺手 stat 一下源存不存在"是两回事，不要因为看起来像就合并：
// 源存在性会过期（检查完到 worker 执行之间文件可能被人删了），提前检查给
// 不了任何保证；而"这块盘的回收站建得起来"是**盘的性质**，稳定得多。执行
// 时的检查照样会跑，预检只是把一个稳定的坏消息提前说出口。
//
// 检查本身**不能有副作用**。这里不复用 ensureTrashDir：它会 MkdirAll +
// Chmod，把"检查"做成"顺手改一下盘"，用户会因为只是想删个文件而看到回收站
// 目录的权限与 ctime 在变。用 access(2)：只问不写，也不创建。
//
// 按**盘**去重是它便宜的前提：按文件查的话，框选 10 万个路径就是 10 万次
// 系统调用，为了提前报错付出的代价比删除本身还高。
func (s *Service) PreflightTrash(ctx context.Context, paths []string, permanent bool) error {
	// permanent=true 根本不碰回收站，拦它等于死锁：盘根写不进去 + 唯一
	// 可行的删除方式被自己的前置检查拒掉 = 这个盘上一个文件都删不了。
	if permanent {
		return nil
	}
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		mount, err := s.fsRoot(p)
		if err != nil {
			// 认不出在哪块盘上：不在这里下结论。执行时会用同一套解析
			// 再判一次，那时报的是"这个路径本身有问题"，比这里瞎猜
			// "回收站不可写"诚实。
			continue
		}
		if seen[mount] {
			continue
		}
		seen[mount] = true
		if err := s.trashWritable(mount); err != nil {
			return err
		}
	}
	return nil
}

// trashWritable 回答"这个盘的回收站现在能用吗"，不改变盘上任何东西。
func (s *Service) trashWritable(mount string) error {
	trash := s.trashRoot(mount)
	// 目标目录取"回收站本身"或"盘根"两者中真正要写的那个：回收站还不存在
	// 时，要写成功的是**盘根**（mkdir 落在盘根上）；已经存在时，要写的是
	// 回收站目录本身。判错对象的两种后果都是假的：拿盘根去判一个已存在
	// 但被收紧成只读的回收站 = 漏报；拿回收站去判一个还不存在的回收站 =
	// 因为 ENOENT 而误报。
	target := mount
	if fi, err := os.Lstat(trash); err == nil && fi.IsDir() {
		target = trash
	}
	if err := unix.Access(target, unix.W_OK); err != nil {
		return fmt.Errorf("%w: %s 这个盘写不进回收站（%s，%v）；这个盘上的文件只能永久删除，或用 SFTP 移走",
			ErrTrashUnwritable, mount, s.trashDirName, err)
	}
	return nil
}
