package filemgr

// 移动执行器。
//
// 设计 8.4："移动跨文件系统时自动降级为复制 + 校验 + 删除源"。
//
// 这一层的风险不对称得厉害：复制失败只是白干一次，而"删源"这一步做错就是
// **不可逆的数据丢失**。所以下面的测试有一半在钉同一件事——源在什么条件下
// 才可以被删掉。
//
// 跨盘路径在本机没法靠真实挂载测（这台手机只有一个可写文件系统），所以
// 走注入点：两块**逻辑**盘共用同一个真实文件系统，但 SameFS 说它们不同盘。
// 于是"跨盘降级"那条代码路径是**真的在跑真的文件操作**，只有"是不是跨盘"
// 这个判定来自夹具。这比"把复制和删除都换成假的"强得多：假实现不会在
// 校验之前把源删掉，而真实现会。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// moveEnv：两块逻辑盘（fast/ 与 slow/），共用一个真实文件系统。
type moveEnv struct {
	svc       *Service
	fast      string
	slow      string
	sameFS    bool  // 两盘之间是否算同一文件系统（夹具的"跨盘开关"）
	sameFSErr error // 非 nil 时让 SameFS 本身报错（测"判不出跨盘"那条路）
}

func newMoveEnv(t testing.TB) *moveEnv {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &moveEnv{sameFS: true, fast: filepath.Join(base, "fast"), slow: filepath.Join(base, "slow")}
	for _, d := range []string{e.fast, e.slow} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.svc = NewService(Options{
		// 逻辑挂载表：fast/ 与 slow/ 是两个"盘根"。
		FilesystemRoot: func(path string) (string, error) {
			for _, r := range []string{e.fast, e.slow} {
				if path == r || strings.HasPrefix(path, r+string(os.PathSeparator)) {
					return r, nil
				}
			}
			return "", fmt.Errorf("假挂载表里没有 %s", path)
		},
		// SameFS：同一盘根之内恒为同盘；跨盘根时由夹具的 sameFS 决定。
		SameFS: func(a, b string) (bool, error) {
			if e.sameFSErr != nil {
				return false, e.sameFSErr
			}
			ra, err1 := e.svc.fsRoot(a)
			rb, err2 := e.svc.fsRoot(b)
			if err1 != nil {
				return false, err1
			}
			if err2 != nil {
				return false, err2
			}
			if ra == rb {
				return true, nil
			}
			return e.sameFS, nil
		},
	})
	return e
}

// ---------- 同盘：一次 rename ----------

// 同盘移动必须是一次 rename：瞬间、原子、inode 不变。
//
// inode 是这里的可观测量。本机 rename 保持 inode（trash 那边也靠同一条
// 事实）。如果同盘移动走了"复制 + 删源"，inode 会变，"复制"与"移动"在用户
// 眼里就没区别了——一个 10GB 目录的移动会从瞬间变成几分钟。
func TestMoveSameDiskKeepsInode(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = true
	src := filepath.Join(e.fast, "src", "a.txt")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	dstDir := filepath.Join(e.fast, "dst")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var before syscall.Stat_t
	if err := syscall.Lstat(src, &before); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.MoveTo(context.Background(), src, dstDir); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(dstDir, "a.txt"), &st); err != nil {
		t.Fatal(err)
	}
	if st.Ino != before.Ino {
		t.Errorf("同盘移动该是 rename（inode %d → %d 变了）", before.Ino, st.Ino)
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("源该没了: %v", err)
	}
}

// 同盘移动一个目录树必须是瞬间的（不遍历、不看大小）。
func TestMoveSameDiskDirectory(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = true
	src := buildTree(t, filepath.Join(e.fast, "tree"), treeSpec{
		"a.txt":      "A",
		"sub/b.txt":  "BB",
		"sub/deep/":  "",
		"sub/dangle": "@不存在",
	})
	dstDir := filepath.Join(e.fast, "elsewhere")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.MoveTo(context.Background(), src, dstDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("源该没了: %v", err)
	}
	got := walkTree(t, filepath.Join(dstDir, "tree"))
	if got["a.txt"] != "A" || got[filepath.Join("sub", "b.txt")] != "BB" {
		t.Errorf("树被搬坏了: %v", got)
	}
	if _, ok := got["sub/dangle"]; !ok {
		t.Error("符号链接没跟着搬过来")
	}
}

// ---------- 跨盘：复制 + 校验 + 删源 ----------

// 跨盘移动必须留下正确的副本、并把源删掉。
func TestMoveCrossDiskCopiesThenDeletesSource(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false // 关键：告诉执行器"这两个盘不是一个文件系统"
	src := filepath.Join(e.fast, "big.bin")
	want := writeFileN(t, src, 2<<20)
	if _, err := e.svc.MoveTo(context.Background(), src, e.slow); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("跨盘移动成功后源该被删掉: %v", err)
	}
	dst := filepath.Join(e.slow, "big.bin")
	if got := hashFile(t, dst); got != want {
		t.Errorf("副本内容不对\n want %s\n  got %s", want, got)
	}
}

// 跨盘移动的副本权限也要保留（复制路径共用，但这里是 move，容易漏）。
func TestMoveCrossDiskPreservesMode(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	src := filepath.Join(e.fast, "key")
	if err := os.WriteFile(src, []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.MoveTo(context.Background(), src, e.slow); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(e.slow, "key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("跨盘移动后权限该是 0600, got %o", fi.Mode().Perm())
	}
}

// 跨盘移动一棵树。
func TestMoveCrossDiskDirectory(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	src := buildTree(t, filepath.Join(e.fast, "tree"), treeSpec{
		"a.txt":     "A",
		"sub/b.txt": "BB",
	})
	dstDir := filepath.Join(e.slow, "in")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.MoveTo(context.Background(), src, dstDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("源该被删掉: %v", err)
	}
	got := walkTree(t, filepath.Join(dstDir, "tree"))
	if got["a.txt"] != "A" || got[filepath.Join("sub", "b.txt")] != "BB" {
		t.Errorf("跨盘搬树搬坏了: %v", got)
	}
}

// ---------- 校验必须先于删源（本文件最重要的一条） ----------

// 校验没通过时**绝不能删源**，而且那份坏副本要清掉。
//
// 这是整个 move 里唯一会丢数据的地方：实现只要写成"copy → 删源 → 回头
// 校验"，一次副本损坏就变成永久损失；写成"copy → 删源"（压根不校验）则是
// 每次跨盘移动都在赌文件系统别出错。这里用真实盘上的一次"事后破坏副本"
// 来验顺序：副本刚拷完、还没删源时把它改坏，看执行器会不会仍然删源。
//
// 这个时间窗在真实机器上要用竞态去打，所以用 moveHooks 注入这个时机——
// 测的是逻辑而不是调度。
func TestMoveCrossDiskVerifyFailureKeepsSource(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	src := filepath.Join(e.fast, "important.db")
	writeFileN(t, src, 1<<20)

	e.svc.moveHooks = &moveHooks{
		// 副本刚拷完、还没删源：把副本改成坏的
		afterCopy: func(dst string) error {
			return os.WriteFile(dst, []byte("磁盘坏了内容没了"), 0o600)
		},
	}
	_, err := e.svc.MoveTo(context.Background(), src, e.slow)
	if err == nil {
		t.Fatal("副本坏了却被认为成功")
	}
	// 源必须完好 —— 这是这条测试的全部意义
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("校验失败却把源删了（数据丢失）: %v", err)
	}
	if fi, err := os.Stat(src); err == nil && fi.Size() != 1<<20 {
		t.Errorf("源被改动: size=%d", fi.Size())
	}
	// 坏副本也要清掉：留着它，用户下次看到的是"文件在，内容错"
	if _, lerr := os.Lstat(filepath.Join(e.slow, "important.db")); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("校验失败的坏副本该被清掉, got stat=%v", lerr)
	}
}

// 删源失败时副本必须留着（两边都在对用户可理解，他会自己删一个）。
func TestMoveCrossDiskSrcDeleteFailureKeepsCopy(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	src := filepath.Join(e.fast, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.svc.moveHooks = &moveHooks{
		beforeDeleteSrc: func(string) error { return errors.New("源所在盘不让写") },
	}
	_, err := e.svc.MoveTo(context.Background(), src, e.slow)
	if err == nil {
		t.Fatal("删源失败该报错")
	}
	if _, lerr := os.Lstat(filepath.Join(e.slow, "a.txt")); lerr != nil {
		t.Errorf("删源失败时副本必须留着（源也还在，用户能自己收拾）: %v", lerr)
	}
	if _, lerr := os.Lstat(src); lerr != nil {
		t.Errorf("源也该还在: %v", lerr)
	}
}

// ---------- 拒绝覆盖 ----------

// 目标已存在 → ErrExists，且两边都不能动。
//
// 同盘时 os.Rename 会**直接覆盖目标**（全盘 root、D14，没有系统护栏）；
// 跨盘时副本会以 O_EXCL 失败。两条路都必须收敛成同一个 ErrExists，界面
// 才能给出"改名 / 先删"的选择。
func TestMoveRefusesExistingDst(t *testing.T) {
	for _, cross := range []bool{false, true} {
		name := "同盘"
		if cross {
			name = "跨盘"
		}
		t.Run(name, func(t *testing.T) {
			e := newMoveEnv(t)
			e.sameFS = !cross
			// 源在一个子目录，目标目录里已有一个**同名**的别的文件。
			srcDir := filepath.Join(e.fast, "from")
			if err := os.MkdirAll(srcDir, 0o755); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(srcDir, "note.txt")
			if err := os.WriteFile(src, []byte("新来的"), 0o644); err != nil {
				t.Fatal(err)
			}
			dstDir := filepath.Join(e.slow, "to")
			if !cross {
				// 同盘：目标目录也放 fast 上，确保 sameFS 判到同盘
				dstDir = filepath.Join(e.fast, "to")
			}
			if err := os.MkdirAll(dstDir, 0o755); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(dstDir, "note.txt")
			if err := os.WriteFile(victim, []byte("别人的重要文件"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := e.svc.MoveTo(context.Background(), src, dstDir)
			if !errors.Is(err, ErrExists) {
				t.Fatalf("应 ErrExists（409）, got %v", err)
			}
			got, rerr := os.ReadFile(victim)
			if rerr != nil || string(got) != "别人的重要文件" {
				t.Errorf("目标被改动: %q (%v)", got, rerr)
			}
			if _, lerr := os.Lstat(src); lerr != nil {
				t.Errorf("拒绝时源必须还在: %v", lerr)
			}
		})
	}
}

// 移进自己的子目录要拒绝（ErrBadPath）。
func TestMoveRefusesIntoOwnSubtree(t *testing.T) {
	for _, cross := range []bool{false, true} {
		e := newMoveEnv(t)
		e.sameFS = !cross
		src := buildTree(t, filepath.Join(e.fast, "src"), treeSpec{"a.txt": "A", "sub/": ""})
		dstDir := filepath.Join(src, "backup")
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := e.svc.MoveTo(context.Background(), src, dstDir)
		if !errors.Is(err, ErrBadPath) {
			t.Errorf("应 ErrBadPath, got %v", err)
		}
		if _, lerr := os.Lstat(filepath.Join(dstDir, "src", "a.txt")); lerr == nil {
			t.Error("拒绝时不该写出任何东西")
		}
	}
}

// 源与目标完全是同一个位置也要拒绝（与 Rename/Edit 同一条护栏）。
func TestMoveRefusesSamePath(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = true
	src := filepath.Join(e.fast, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 目标目录就是源所在目录、且源 basename 在目标目录里就是它自己
	if _, err := e.svc.MoveTo(context.Background(), src, e.fast); !errors.Is(err, ErrBadPath) {
		t.Errorf("原地移动到同一位置该 ErrBadPath, got %v", err)
	}
	// 源必须原样还在
	if _, err := os.Stat(src); err != nil {
		t.Errorf("原地移动不该动源: %v", err)
	}
}

// 源不存在 → fs.ErrNotExist（404）。
func TestMoveMissingSource(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = true
	_, err := e.svc.MoveTo(context.Background(), filepath.Join(e.fast, "没有这个"), e.slow)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("应 fs.ErrNotExist, got %v", err)
	}
}

// 目标目录不存在 → ErrNotDirectory（400），不自动 mkdir。
func TestMoveMissingDstDir(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = true
	src := filepath.Join(e.fast, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.MoveTo(context.Background(), src, filepath.Join(e.slow, "没有这个目录"))
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("应 ErrNotDirectory, got %v", err)
	}
	if _, lerr := os.Lstat(filepath.Join(e.slow, "没有这个目录")); !errors.Is(lerr, os.ErrNotExist) {
		t.Error("不该顺手创建目标目录")
	}
}

// ---------- 取消 ----------

// 跨盘移动中途取消：源必须完好、副本必须清干净。
func TestMoveCrossDiskCancelKeepsSource(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	src := filepath.Join(e.fast, "big.bin")
	writeFileN(t, src, 6<<20)

	ctx, cancel := context.WithCancel(context.Background())
	first := true
	_, err := e.svc.movePath(ctx, src, e.slow, func(int64, int) error {
		if first {
			first = false
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应 context.Canceled, got %v", err)
	}
	if _, serr := os.Stat(src); serr != nil {
		t.Errorf("取消把源弄丢了: %v", serr)
	}
	if _, derr := os.Lstat(filepath.Join(e.slow, "big.bin")); !errors.Is(derr, os.ErrNotExist) {
		t.Errorf("取消后半个副本该清掉, got stat=%v", derr)
	}
}

// 不是取消而是外部超时时，同样不许丢源。
//
// 取消与超时在 Go 里都是 ctx.Err() != nil；但如果清理只写在"收到取消"那个
// 分支里，一次网络抖动造成的超时就走了另一条没收尾的路。
func TestMoveCrossDiskTimeoutKeepsSource(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	src := filepath.Join(e.fast, "big.bin")
	writeFileN(t, src, 6<<20)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	_, err := e.svc.MoveTo(ctx, src, e.slow)
	if err == nil {
		t.Fatal("超时该失败")
	}
	if _, serr := os.Stat(src); serr != nil {
		t.Errorf("超时把源弄丢了（数据丢失）: %v", serr)
	}
}

// ---------- 符号链接 ----------

// 移动符号链接要搬**链接本身**（同盘 rename 天然如此；跨盘必须重建链接，
// 不能把它指向的目标整份搬过来）。
func TestMoveSymlinkMovesLinkItself(t *testing.T) {
	for _, cross := range []bool{false, true} {
		e := newMoveEnv(t)
		e.sameFS = !cross
		target := filepath.Join(e.fast, "target")
		if err := os.WriteFile(target, []byte("内容"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(e.fast, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.MoveTo(context.Background(), link, e.slow); err != nil {
			t.Fatal(err)
		}
		if _, lerr := os.Lstat(link); !errors.Is(lerr, os.ErrNotExist) {
			t.Errorf("链接本身该被搬走, got stat=%v", lerr)
		}
		moved := filepath.Join(e.slow, "link")
		fi, err := os.Lstat(moved)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("副本该是符号链接, got mode=%v", fi.Mode())
		}
		to, err := os.Readlink(moved)
		if err != nil {
			t.Fatal(err)
		}
		if to != target {
			t.Errorf("链接指向变了: %q → %q", target, to)
		}
		if got, rerr := os.ReadFile(target); rerr != nil || string(got) != "内容" {
			t.Errorf("链接指向的文件被动过: %q (%v)", got, rerr)
		}
	}
}

// 悬空链接也要能移动。
func TestMoveDanglingSymlink(t *testing.T) {
	for _, cross := range []bool{false, true} {
		e := newMoveEnv(t)
		e.sameFS = !cross
		link := filepath.Join(e.fast, "dangling")
		if err := os.Symlink(filepath.Join(e.fast, "永远不存在"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.MoveTo(context.Background(), link, e.slow); err != nil {
			t.Fatalf("悬空链接该能移动: %v", err)
		}
		if _, err := os.Readlink(filepath.Join(e.slow, "dangling")); err != nil {
			t.Errorf("移动之后副本该是链接: %v", err)
		}
	}
}

// ---------- 一次移动多个源 ----------

// 一次粘贴多个文件：任何一个撞名，整批在动手前就该被拒，一个都不许动。
//
// 部分成功是最坏结果：用户看到"报错了"，却发现有几个文件已经搬走了——
// 与批量删除同一条纪律（先全部校验再动手）。
func TestMoveManyRejectsWholeBatchBeforeTouching(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = true
	var srcs []string
	for i := 0; i < 3; i++ {
		p := filepath.Join(e.fast, "f"+itoa(i))
		if err := os.WriteFile(p, []byte(itoa(i)), 0o644); err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, p)
	}
	// 第三个在目标处已有同名
	if err := os.WriteFile(filepath.Join(e.slow, "f2"), []byte("占位"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.MoveMany(context.Background(), srcs, e.slow, nopProgress)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("应 ErrExists, got %v", err)
	}
	for _, p := range srcs {
		if _, lerr := os.Lstat(p); lerr != nil {
			t.Errorf("整批拒绝却搬走了 %s: %v", p, lerr)
		}
	}
	if got := mustRead(t, filepath.Join(e.slow, "f2")); string(got) != "占位" {
		t.Error("目标处的同名文件被改写了")
	}
}

// 成功路径：全部搬过去，条目数对上。
func TestMoveManySucceeds(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = true
	var srcs []string
	for i := 0; i < 3; i++ {
		p := filepath.Join(e.fast, "g"+itoa(i))
		if err := os.WriteFile(p, []byte(itoa(i)), 0o644); err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, p)
	}
	n, err := e.svc.MoveMany(context.Background(), srcs, e.slow, nopProgress)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("该回 3 个, got %d", n)
	}
	for i := 0; i < 3; i++ {
		if _, err := os.Stat(filepath.Join(e.slow, "g"+itoa(i))); err != nil {
			t.Errorf("第 %d 个没搬过去: %v", i, err)
		}
	}
}

// ---------- 补齐三条守卫（变异测试逼出来的） ----------

// 跨盘目录树：任何一条对不上都不能删源。
//
// 单文件路径有 verifyFilesMatch 兜着，目录树曾经只比"条目数 + 总字节"——
// 那会被一棵"少了 30 个、又凭空多出 30 个同名不同内容文件"的树骗过去，
// 而 move 接下来要删源。这里在 afterCopy（副本整棵落盘之后、校验之前）
// 偷偷改坏一个文件的内容，验执行器不会删源、且会把坏副本整棵清掉。
//
// afterCopy 收到的 dst 对目录树而言是**目标目录**，改它里面的子文件即可。
func TestMoveCrossDiskTreeVerifyFailureKeepsSource(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	src := buildTree(t, filepath.Join(e.fast, "tree"), treeSpec{
		"a.txt":     "A",
		"sub/b.txt": "BB",
	})
	dstDir := filepath.Join(e.slow, "in")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.svc.moveHooks = &moveHooks{
		afterCopy: func(dst string) error {
			// 把副本里的一个文件改坏（内容变、长度也变，确保 sha256 不匹配）
			return os.WriteFile(filepath.Join(dst, "sub", "b.txt"), []byte("坏掉了XXXX"), 0o644)
		},
	}
	if _, err := e.svc.MoveTo(context.Background(), src, dstDir); err == nil {
		t.Fatal("副本被改坏却被认为成功")
	}
	// 源必须整棵还在
	if _, err := os.Stat(filepath.Join(src, "sub", "b.txt")); err != nil {
		t.Fatalf("校验失败却删了源（数据丢失）: %v", err)
	}
	// 坏副本整棵要清掉
	if _, lerr := os.Lstat(filepath.Join(dstDir, "tree")); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("校验失败的坏副本该整棵清掉, got stat=%v", lerr)
	}
}

// 副本已经落盘、正要校验时上下文才过期 → 不许删源，也不留副本。
//
// 这个窗口 copyFile 内部的取消检查够不着（那是拷贝**过程中**）：拷贝已经
// 成功返回，ctx 是在"要不要删源"这道门前过期的。此时删源就是丢数据——用户
// 取消了，副本又没被确认，两边都可能不完整。用 afterCopy 注入这次过期，
// 让窗口精确落在门上。
func TestMoveCrossDiskCancelJustBeforeVerify(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	src := filepath.Join(e.fast, "a.bin")
	writeFileN(t, src, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.moveHooks = &moveHooks{
		afterCopy: func(string) error { cancel(); return nil },
	}
	_, err := e.svc.movePath(ctx, src, e.slow, nopProgress)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应 context.Canceled, got %v", err)
	}
	if _, serr := os.Stat(src); serr != nil {
		t.Errorf("门前的取消不该删源: %v", serr)
	}
	if _, derr := os.Lstat(filepath.Join(e.slow, "a.bin")); !errors.Is(derr, os.ErrNotExist) {
		t.Errorf("取消后未确认的副本该清掉, got stat=%v", derr)
	}
}

// 含 FIFO 的目录树跨盘移动必须明确失败，而不是永久挂住。
//
// 与 copy 那边同一条纪律，但这里是 move：worker 里一个 open 阻塞就等于
// 一条永不结束、还占着并发名额的任务。断言"有超时也定会返回"。
func TestMoveCrossDiskRejectsFIFO(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	src := filepath.Join(e.fast, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "ok.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(src, "myfifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("本机造不出 FIFO: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(fifo) })
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.MoveTo(context.Background(), src, e.slow)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("含 FIFO 的树该失败")
		}
		if !errors.Is(err, ErrUnsupportedFileType) {
			t.Errorf("应 ErrUnsupportedFileType, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("MoveTo 没返回：对 FIFO 执行了 open（永久阻塞 worker）")
	}
	// 源必须整棵还在（拒绝不能动源）
	if _, err := os.Stat(filepath.Join(src, "ok.txt")); err != nil {
		t.Errorf("拒绝时源被动了: %v", err)
	}
}

// SameFS 本身报错时，绝不能退回"当同盘先试 rename"。
//
// 判不出跨不跨盘还硬走同盘 rename，在某些 fuse 实现上会"跨盘也成功"——
// 于是源没了而校验根本没跑。这种机器上不确定的东西只能靠注入触发：让
// 夹具的 SameFS 直接报错，看 move 会不会照样动手。
func TestMoveAbortsWhenSameFSUnknown(t *testing.T) {
	for _, cross := range []bool{true, false} {
		e := newMoveEnv(t)
		e.sameFS = !cross
		e.sameFSErr = errors.New("挂载表读不到")
		src := filepath.Join(e.fast, "a.bin")
		writeFileN(t, src, 1<<20)
		_, err := e.svc.MoveTo(context.Background(), src, e.slow)
		if err == nil {
			t.Fatal("SameFS 判不出来却仍然执行了移动")
		}
		// 无论同盘跨盘，源都不该被动
		if _, serr := os.Stat(src); serr != nil {
			t.Errorf("判不出跨盘却把源弄丢了: %v", serr)
		}
		if _, derr := os.Lstat(filepath.Join(e.slow, "a.bin")); !errors.Is(derr, os.ErrNotExist) {
			t.Errorf("判不出跨盘时不该在目标写任何东西")
		}
	}
}

// 源**本身**是 FIFO 时（不是"目录里含 FIFO"，那条走 copyTree 已挡）也必须
// 明确失败。moveCrossDisk 里单文件路径的拒绝分支只有这种源才碰得到。
func TestMoveBareFIFORejected(t *testing.T) {
	e := newMoveEnv(t)
	e.sameFS = false
	fifo := filepath.Join(e.fast, "p")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("本机造不出 FIFO: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(fifo) })
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.MoveTo(context.Background(), fifo, e.slow)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnsupportedFileType) {
			t.Errorf("应 ErrUnsupportedFileType, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("MoveTo 没返回：对 FIFO 执行了 open")
	}
	if _, serr := os.Stat(fifo); serr != nil {
		t.Errorf("拒绝时源该还在: %v", serr)
	}
}
