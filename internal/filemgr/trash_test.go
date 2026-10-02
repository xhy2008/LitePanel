package filemgr

// M6-T5：回收站（设计 8.6 / D12，**按盘分置**）。
//
// 夹具注入两个环境查询：FilesystemRoot（这个路径属于哪个盘）与
// TrashRoots（要管哪些盘）。注入不是为了省事，是因为"删除绝不跨盘复制"
// 这条必须在**真的有两个文件系统**时才算测过，而本机唯一可写的第二个盘
// 是 sdcardfs（用户的下载目录），测试往里写等于污染用户数据；目标机上
// 盘的数量更不确定。注入之后夹具可以任意虚构盘数，而真实那两个实现各由
// realFilesystemRoot 与 discoverTrashRoots 的测试单独覆盖。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------- 夹具 ----------

type trashEnv struct {
	svc  *Service
	clk  *fakeClock
	ssd  string // 假"根盘"
	disk string // 假"/DISK"（机械盘）
	home string // 假"/home"（用户目录所在盘）
	// roots 是假挂载表：盘根 -> 盘根。FilesystemRoot 按最长前缀命中它。
	roots []string
}

// newTrashEnv 接受 testing.TB 而不是 *testing.T：同一套夹具也要能被基准
// 复用（"一批删除到底要多久"决定同步端点撑不撑得住，那只能量出来）。
func newTrashEnv(t testing.TB) *trashEnv {
	t.Helper()
	base := t.TempDir()
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	e := &trashEnv{clk: &fakeClock{now: time.Unix(1700000000, 0)}}
	for _, n := range []string{"ssd", "disk", "home"} {
		d := filepath.Join(real, n)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		switch n {
		case "ssd":
			e.ssd = d
		case "disk":
			e.disk = d
		case "home":
			e.home = d
		}
	}
	e.roots = []string{e.ssd, e.disk, e.home}
	e.svc = e.assemble()
	return e
}

// assemble 用当前夹具重建一个 Service（模拟面板重启：没有任何进程内状态
// 传过去，回收站的一切必须能从盘上认出来）。
func (e *trashEnv) assemble() *Service {
	return NewService(Options{
		ProcDir: filepath.Join("testdata", "proc_server"),
		Clock:   e.clk.Now,
		FilesystemRoot: func(path string) (string, error) {
			best := ""
			for _, r := range e.roots {
				if (path == r || strings.HasPrefix(path, r+"/")) && len(r) > len(best) {
					best = r
				}
			}
			if best == "" {
				return "", fmt.Errorf("假挂载表里没有 %s 这个路径", path)
			}
			return best, nil
		},
		TrashRoots: func(context.Context) ([]string, error) {
			out := make([]string, 0, len(e.roots))
			for _, r := range e.roots {
				out = append(out, r)
			}
			return out, nil
		},
	})
}

// root 把某个盘里的相对路径拼出来。
func (e *trashEnv) root(which, rel string) string {
	var base string
	switch which {
	case "ssd":
		base = e.ssd
	case "disk":
		base = e.disk
	case "home":
		base = e.home
	default:
		panic("未知盘 " + which)
	}
	return filepath.Join(base, rel)
}

func (e *trashEnv) mk(t *testing.T, which, rel, body string) string {
	t.Helper()
	p := e.root(which, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *trashEnv) mkdir(t *testing.T, which, rel string) string {
	t.Helper()
	p := e.root(which, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *trashEnv) del(t *testing.T, path string) TrashItem {
	t.Helper()
	it, err := e.svc.Delete(context.Background(), path)
	if err != nil {
		t.Fatalf("删除 %s: %v", path, err)
	}
	return it
}

func (e *trashEnv) list(t *testing.T) []TrashItem {
	t.Helper()
	items, err := e.svc.ListTrash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// inoOf 取 inode 号。inode 在删除前后**必须相同** —— 这是"移过去而不是
// 复制一份再删源"最硬的证据：复制会得到新 inode，而"复制"正是用户明确
// 不要的那种无意义磁盘擦写。（本机实测 sdcardfs 与 f2fs 上 rename 都
// 保持 inode，所以这条断言在目标机上也成立。）
func inoOf(t *testing.T, p string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return st.Ino
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// countTree 数一棵树里的条目（含目录自身）。
func countTree(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(_ string, _ fs.DirEntry, e error) error {
		// 错误是从 walkFn 的**参数**里进来的（WalkDir 自身返回 nil 并
		// 继续），所以必须在回调里判 —— 在 WalkDir 的返回值上判 ErrNotExist
		// 永远不成立，而"目录不存在"会被算成 1 个条目，于是"别的盘上
		// 没有副本"这条断言在最该报警的时候反而通过。
		if errors.Is(e, fs.ErrNotExist) {
			return filepath.SkipAll // 不存在 == 0 个条目
		}
		if e != nil {
			return e
		}
		n++
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return n
}

func findItem(items []TrashItem, origin string) (TrashItem, bool) {
	for _, it := range items {
		if it.Origin == origin {
			return it, true
		}
	}
	return TrashItem{}, false
}

// ---------- 移入本盘回收站 ----------

// 删除一个文件要移进**它所在盘**的回收站，而且必须是移动不是复制。
//
// 三件事分别钉住：落点在那个盘的 .trash 里（不是别处）、源路径消失
// （不是"复制一份留着原件"）、inode 相同（不是"复制一份再删源"）。
// 第三条是唯一能区分"rename"与"copy+unlink"的观测：两者从文件列表上看
// 完全一样，而磁盘擦写量差着整个文件大小。
func TestDeleteGoesToOwnDiskTrash(t *testing.T) {
	e := newTrashEnv(t)
	p := e.mk(t, "disk", "projects/报告 2026.txt", "内容")
	before := inoOf(t, p)

	it := e.del(t, p)

	if exists(p) {
		t.Error("源路径还在：删除没有真的移走")
	}
	if it.Mount != e.disk {
		t.Errorf("条目该记着属于哪个盘: %+v", it)
	}
	if !strings.HasPrefix(it.Path, filepath.Join(e.disk, DefaultTrashDirName)+string(os.PathSeparator)) {
		t.Errorf("条目该落在本盘回收站里, got %q", it.Path)
	}
	if it.Name != "报告 2026.txt" {
		t.Errorf("Name 该是**原**文件名（界面主列用它）, got %q", it.Name)
	}
	if it.Origin != p {
		t.Errorf("Origin 该是原绝对路径（还原要用）: got %q", it.Origin)
	}
	if it.Size != int64(len("内容")) {
		t.Errorf("Size = %d, 期望 %d", it.Size, len("内容"))
	}
	if it.DeletedAt != e.clk.now.Unix() {
		t.Errorf("DeletedAt 该用注入时钟: got %d want %d", it.DeletedAt, e.clk.now.Unix())
	}
	if it.IsDir {
		t.Error("文件不该被标成目录")
	}
	if after := inoOf(t, it.Path); after != before {
		t.Errorf("inode 变了 %d→%d：这是复制不是移动，等于一次无意义的整文件擦写", before, after)
	}
	// 内容必须完好
	b, err := os.ReadFile(it.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "内容" {
		t.Errorf("内容变了: %q", b)
	}
}

// 同一个盘上的两个文件进同一个回收站，各占一个条目。
//
// 平铺而不是按原路径重建目录树：重建会让"回收站里长得跟原来一样"，
// 用户以为没删（尤其在文件列表显示隐藏文件时），而条目 ID 也需要唯一。
func TestDeleteTwoFilesSameDisk(t *testing.T) {
	e := newTrashEnv(t)
	a := e.mk(t, "disk", "a.txt", "A")
	b := e.mk(t, "disk", "sub/a.txt", "BB")
	e.del(t, a)
	e.del(t, b)
	items := e.list(t)
	if len(items) != 2 {
		t.Fatalf("应有两条, got %d: %+v", len(items), items)
	}
	if items[0].ID == items[1].ID {
		t.Errorf("同名的两个文件必须有两个 ID: %+v", items)
	}
	// 同 basename 的两个文件，Name 一样而 Origin 不一样（界面靠 Origin
	// 消歧，否则两行"报告.txt"分不开）
	if items[0].Name == items[1].Name {
		if items[0].Origin == items[1].Origin {
			t.Error("Origin 必须能区分两条")
		}
	}
}

// 目录整棵树进回收站，条目数一致。
//
// 这条看着像重复，实际守的是"条目是个目录时，遍历不能把 .meta.json 自己
// 也算进 Size/条目里"，以及"移的是目录本身而不是它的内容"。
func TestDeleteDirectoryKeepsTree(t *testing.T) {
	e := newTrashEnv(t)
	dir := e.mkdir(t, "disk", "tree")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(filepath.Join(dir, "sub", fmt.Sprintf("f%02d.bin", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := countTree(t, dir)
	it := e.del(t, dir)
	if !it.IsDir {
		t.Error("目录该标 IsDir")
	}
	// Path 必须**就是**被移动的那棵树：条目布局是
	// <trash>/<ID>（载荷本身）+ <trash>/<ID>.meta.json（同级边文件）。
	// 如果实现改成"条目是一个装了两样东西的目录"，这里的计数会多 1，
	// 而那多出来的 1 会让"移动不改变树的大小"这条断言失去意义。
	if after := countTree(t, it.Path); after != before {
		t.Errorf("树里条目数变了 %d→%d", before, after)
	}
	if exists(dir) {
		t.Error("原目录还在")
	}
}

// 盘根写不进去时**必须失败**，绝不退化成跨盘复制。
//
// 这是整个设计的要害，也是用户裁定的原文所在（"避免无意义的磁盘擦写"）。
// 技术上最"贴心"的实现是：这个盘的回收站建不起来，就找一个能建的盘放 ——
// 那正好把一次 rename 变成一次整文件跨盘复制，而用户按的是"删除"。
// 更糟的是它不会报错，只会很慢。所以这里既要失败，还要失败得没有副作用：
// 源文件完好、别处没有多出副本。
func TestDeleteFailsWhenTrashUnwritable(t *testing.T) {
	e := newTrashEnv(t)
	// 待删文件放在子目录里，盘根本身 chmod 成 0500：于是"把文件移出去"
	// 这一步是允许的（子目录可写），而"在盘根建回收站"必然 EACCES
	// （实测）。如果直接把文件放在盘根下，rename 自己就先失败了，
	// 测到的是权限而不是这条分支。
	parent := e.mkdir(t, "home", "sub")
	p := filepath.Join(parent, "重要数据.bin")
	if err := os.WriteFile(p, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.home, 0o755) })

	_, err := e.svc.Delete(context.Background(), p)
	if !errors.Is(err, ErrTrashUnwritable) {
		t.Fatalf("应 ErrTrashUnwritable, got %v", err)
	}
	if !exists(p) {
		t.Error("失败时源文件必须完好")
	}
	// 别的盘上不许出现副本（那正是"悄悄跨盘复制"的证据）
	for _, other := range []string{e.ssd, e.disk} {
		n := countTree(t, filepath.Join(other, DefaultTrashDirName))
		if n != 0 {
			t.Errorf("副本出现在别的盘的回收站里（%d 条目）：那正是被禁止的跨盘复制", n)
		}
	}
	// 文案要给出路：用户此刻能做的只有永久删除
	if !strings.Contains(err.Error(), "永久") && !strings.Contains(err.Error(), "SFTP") {
		t.Errorf("失败文案该给替代动作, got %q", err.Error())
	}
}

// 不存在的文件回 fs.ErrNotExist（404），而不是"已移入回收站"。
//
// 后者才是这里真正要防的：条目建了、源没了，回收站里就多一条指向虚空
// 的记录，而还原它必然失败。
func TestDeleteMissingSource(t *testing.T) {
	e := newTrashEnv(t)
	_, err := e.svc.Delete(context.Background(), e.root("disk", "没这个文件.txt"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("应 fs.ErrNotExist, got %v", err)
	}
	if items := e.list(t); len(items) != 0 {
		t.Errorf("失败的删除不该留下条目: %+v", items)
	}
}

// 不许把回收站自己（或它的一个祖先）丢进回收站。
//
// 少这一条会出套娃：条目被移进它自己所在的树里，遍历与还原都会看到
// 一条自我包含的路径，而那种条目谁也清不掉。
func TestDeleteRefusesTrashItself(t *testing.T) {
	e := newTrashEnv(t)
	// 先造一个条目，确保 .trash 存在
	e.del(t, e.mk(t, "disk", "seed.txt", "x"))
	trash := filepath.Join(e.disk, DefaultTrashDirName)
	if _, err := e.svc.Delete(context.Background(), trash); !errors.Is(err, ErrBadPath) {
		t.Errorf("删回收站自身应 ErrBadPath, got %v", err)
	}
	// 盘的根也不能删（它是锚点，删了盘就"没了"）
	if _, err := e.svc.Delete(context.Background(), e.disk); !errors.Is(err, ErrBadPath) {
		t.Errorf("删盘根应 ErrBadPath, got %v", err)
	}
}

// ctx 取消时不许留下"条目建了一半"的中间态。
func TestDeleteHonorsContext(t *testing.T) {
	e := newTrashEnv(t)
	e.mk(t, "disk", "a.txt", "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.svc.Delete(ctx, e.root("disk", "a.txt")); !errors.Is(err, context.Canceled) {
		t.Errorf("应 context.Canceled, got %v", err)
	}
	if !exists(e.root("disk", "a.txt")) {
		t.Error("取消的删除不该动源文件")
	}
	if items := e.list(t); len(items) != 0 {
		t.Errorf("取消的删除不该留条目: %+v", items)
	}
}

// ---------- 条目名：有界且唯一 ----------

// 深层中文路径的条目名必须有界。
//
// 单个路径名 255 字节是文件系统硬上限（本机实测 360 字节的扁平化名直接
// ENAMETOOLONG）。"扁平化整个原路径"看着方便，实际会在某个用户不小心
// 删掉一个深目录里的长中文名文件时报一个他看不懂的错误。
func TestEntryIDBounded(t *testing.T) {
	e := newTrashEnv(t)
	deep := filepath.Join(strings.Repeat("很长的目录名/", 12), strings.Repeat("中文文件名", 8)+".txt")
	p := e.mk(t, "disk", deep, "x")
	it := e.del(t, p)
	if len(it.ID) > 255 {
		t.Errorf("条目名 %d 字节，超过路径名上限: %q", len(it.ID), it.ID)
	}
	if len(filepath.Base(it.Path)) > 255 {
		t.Errorf("条目目录名超过上限: %q", filepath.Base(it.Path))
	}
	// 中文名不能变成乱码/问号（回收站界面要显示原名）
	if !strings.Contains(it.Name, "中文文件名") {
		t.Errorf("Name 该保住原文: %q", it.Name)
	}
}

// 同一路径不可能有两个条目，而不同路径的同名文件必须有两个不同 ID。
//
// ID 只拿"相对路径 + 时间戳"拼的话，两台盘同一秒删掉同名文件会撞；
// 撞了以后还原其中一个会带走另一个。所以 ID 必须带全局可辨的成分。
func TestEntryIDUniqueAcrossDisks(t *testing.T) {
	e := newTrashEnv(t)
	// 三个盘、同一个相对路径、同一个假时钟时刻
	ids := map[string]string{}
	for _, w := range []string{"ssd", "disk", "home"} {
		it := e.del(t, e.mk(t, w, "报告.txt", w))
		if prev, ok := ids[it.ID]; ok {
			t.Fatalf("ID 撞了：%s 与 %s 都是 %s", prev, w, it.ID)
		}
		ids[it.ID] = w
	}
	if len(ids) != 3 {
		t.Fatalf("应有 3 个不同 ID, got %d", len(ids))
	}
	if got := len(e.list(t)); got != 3 {
		t.Errorf("应列出 3 条, got %d", got)
	}
}

// ---------- 列举 ----------

// 列举要跨所有盘，并按盘标注来源。
//
// 按盘分置换来的代价就是这里要遍历；"这个条目在哪个盘上"必须显示出来，
// 否则用户在回收站里看到两个 a.txt 而其中一个在已经拔掉的移动硬盘上，
// 他唯一的解读是面板出了 bug。
func TestListTrashAcrossDisks(t *testing.T) {
	e := newTrashEnv(t)
	e.del(t, e.mk(t, "disk", "a.txt", "1"))
	e.clk.advance(time.Hour)
	e.del(t, e.mk(t, "home", "b.txt", "22"))
	items := e.list(t)
	if len(items) != 2 {
		t.Fatalf("应 2 条, got %d", len(items))
	}
	a, ok := findItem(items, e.root("disk", "a.txt"))
	if !ok {
		t.Fatalf("缺 disk 盘上 a.txt 那条: %+v", items)
	}
	if _, ok := findItem(items, e.root("home", "b.txt")); !ok {
		t.Fatalf("缺 home 盘上 b.txt 那条: %+v", items)
	}
	if a.Mount != e.disk {
		t.Errorf("a.txt 该标在 disk 盘: %+v", a)
	}
	// 顺序必须有确定性：抽屉里每次刷新都换序，用户会以为条目在变。
	// 按删除时间倒序（最新的在最前）是设计里"最近删掉"的直觉。
	if items[0].DeletedAt < items[1].DeletedAt {
		t.Errorf("应按删除时间倒序: %+v %+v", items[0], items[1])
	}
}

// 空回收站回空列表，不是错误。
//
// 盘根还没有 .trash 目录是全新机器的正常状态；这里报错会让回收站抽屉
// 一打开就红着。
func TestListTrashEmptyIsNotError(t *testing.T) {
	e := newTrashEnv(t)
	items, err := e.svc.ListTrash(context.Background())
	if err != nil {
		t.Fatalf("空回收站不该报错: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("应空, got %+v", items)
	}
}

// 回收站里"人手工放进去的东西"不许被当成条目处理。
//
// 这是回收站与普通目录唯一的区别所在，而它靠的是"认不认得出元信息"。
// 认错的结果是把用户自己的文件当成过期条目删掉 —— 那是数据丢失，
// 而它发生在"清理回收站"这个本应最安全的动作里。
func TestListTrashIgnoresForeignEntries(t *testing.T) {
	e := newTrashEnv(t)
	e.del(t, e.mk(t, "disk", "real.txt", "x"))
	trash := filepath.Join(e.disk, DefaultTrashDirName)
	// 一个不像条目的目录 + 一个散文件
	if err := os.MkdirAll(filepath.Join(trash, "我自己放的目录"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trash, "notes.txt"), []byte("my"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := e.list(t)
	if len(items) != 1 {
		t.Fatalf("只该认出面板自己放的 1 条, got %d: %+v", len(items), items)
	}
	if !exists(filepath.Join(trash, "我自己放的目录")) || !exists(filepath.Join(trash, "notes.txt")) {
		t.Error("列举不该改动任何东西")
	}
	// 清理与清空同样不许碰它们（三个动作共用同一个"认条目"的判断）
	if _, err := e.svc.CleanTrash(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.EmptyTrash(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(trash, "我自己放的目录")) || !exists(filepath.Join(trash, "notes.txt")) {
		t.Error("清理/清空把用户自己放的东西删了 —— 这是数据丢失")
	}
}

// ---------- 还原 ----------

// 还原回到原路径，内容不变，条目消失。
func TestRestoreBackToOrigin(t *testing.T) {
	e := newTrashEnv(t)
	p := e.mk(t, "disk", "docs/报告.txt", "原内容")
	it := e.del(t, p)
	got, err := e.svc.RestoreTrash(context.Background(), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Errorf("应回到原路径, got %q", got)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "原内容" {
		t.Errorf("内容变了: %q", b)
	}
	if items := e.list(t); len(items) != 0 {
		t.Errorf("还原后条目该消失: %+v", items)
	}
}

// 原路径已被占用时拒绝还原，而条目必须留在回收站里。
//
// 409 是这条流程的护栏：还原时静默改名会造出"报告 (1).txt"，用户以为
// 找回来了，实际原件还在原地；静默覆盖则是把别人的新文件吃掉。
// 而条目必须还在 —— 报一个"失败"然后把条目也删掉，等于把这次还原
// 变成真正的删除。
func TestRestoreRefusesWhenOccupied(t *testing.T) {
	e := newTrashEnv(t)
	p := e.mk(t, "disk", "a.txt", "旧")
	it := e.del(t, p)
	e.mk(t, "disk", "a.txt", "别人新建的")

	_, err := e.svc.RestoreTrash(context.Background(), it.ID)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("应 ErrExists, got %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "别人新建的" {
		t.Error("还原失败不该动占位的文件")
	}
	if items := e.list(t); len(items) != 1 {
		t.Errorf("还原失败条目必须还在: %+v", items)
	}
}

// 原目录已经没了要补建父目录。
//
// "删了整个文件夹、后来想找回其中一个文件"是回收站最常见的用法。要求
// 用户先手工把目录建回来，等于让还原在最需要它的场景里失效。
func TestRestoreCreatesMissingParents(t *testing.T) {
	e := newTrashEnv(t)
	e.mkdir(t, "disk", "parent")
	p := e.mk(t, "disk", "parent/child/深.txt", "x")
	it := e.del(t, p)
	// 连父目录一起删掉（这次是正常删除，会进回收站）
	e.del(t, filepath.Dir(filepath.Dir(p)))
	if err := os.RemoveAll(filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RestoreTrash(context.Background(), it.ID); err != nil {
		t.Fatalf("父目录不存在时还原该补建: %v", err)
	}
	if !exists(p) {
		t.Error("文件没回到原路径")
	}
}

// 未知 id 回 404。
//
// 中文 id 会被前面的字符集校验挡下，测不到 404 分支（上一轮踩过），
// 所以这里用 ASCII。
func TestRestoreUnknownID(t *testing.T) {
	e := newTrashEnv(t)
	e.del(t, e.mk(t, "disk", "a.txt", "x"))
	if _, err := e.svc.RestoreTrash(context.Background(), "never-seen"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("未知 id 应 fs.ErrNotExist, got %v", err)
	}
}

// 条目所在盘不在回收站清单里（移动硬盘被拔了）时，不能靠猜来还原。
//
// 猜的依据只有条目里记着的 Origin 字符串 —— 照着它 mkdir -p 会在**别的盘**
// 上造出一棵同名目录树，用户以为找回来了，实际文件还在拔掉的那块盘上。
func TestRestoreWhenDiskGone(t *testing.T) {
	e := newTrashEnv(t)
	it := e.del(t, e.mk(t, "disk", "a.txt", "x"))
	// 换掉清单：disk 盘"不在了"
	saved := e.roots
	e.roots = []string{e.ssd, e.home}
	e.svc = e.assemble()
	if _, err := e.svc.RestoreTrash(context.Background(), it.ID); err == nil {
		t.Fatal("盘不在清单里时不该假装还原成功")
	}
	e.roots = saved
}

// ---------- 永久删除与清空 ----------

func TestPurgeRemovesOnlyThatEntry(t *testing.T) {
	e := newTrashEnv(t)
	a := e.del(t, e.mk(t, "disk", "a.txt", "1"))
	b := e.del(t, e.mk(t, "disk", "b.txt", "2"))
	if err := e.svc.PurgeTrash(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	items := e.list(t)
	if len(items) != 1 || items[0].ID != b.ID {
		t.Errorf("只该删掉一条: %+v", items)
	}
	if err := e.svc.PurgeTrash(context.Background(), "never-seen"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("未知 id 应 fs.ErrNotExist, got %v", err)
	}
}

// 清空跨所有盘，返回删掉的条数（界面要显示"已清空 N 项"）。
func TestEmptyTrashAllDisks(t *testing.T) {
	e := newTrashEnv(t)
	for _, w := range []string{"ssd", "disk", "home"} {
		e.del(t, e.mk(t, w, "a.txt", w))
	}
	n, err := e.svc.EmptyTrash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("应报 3 条, got %d", n)
	}
	if items := e.list(t); len(items) != 0 {
		t.Errorf("清空后不该有残留: %+v", items)
	}
	// 盘本身与盘上其它文件没事
	for _, w := range []string{"ssd", "disk", "home"} {
		if err := os.MkdirAll(e.root(w, "留着"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !exists(e.root(w, "留着")) {
			t.Errorf("%s 盘上的其它文件被清了", w)
		}
	}
}

// ---------- 过期清理（设计 8.6：保留 3 天）----------

// 超过保留期的条目永久删除，期内的保留 —— 用注入时钟，不真等 3 天。
func TestCleanTrashByAge(t *testing.T) {
	e := newTrashEnv(t)
	old := e.del(t, e.mk(t, "disk", "old.txt", "o"))
	e.clk.advance(DefaultTrashRetain + time.Minute)
	fresh := e.del(t, e.mk(t, "disk", "new.txt", "n"))

	n, err := e.svc.CleanTrash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应清掉 1 条, got %d", n)
	}
	items := e.list(t)
	if len(items) != 1 || items[0].ID != fresh.ID {
		t.Errorf("该留下没过期那条: %+v", items)
	}
	if exists(old.Path) {
		t.Error("过期条目该被永久删除（不是移去别处）")
	}
}

// 恰好等于保留期时**保留**。
//
// 边界方向必须有测试钉住：写成 >= 的话，一个"刚好 3 天"的文件会在用户
// 第三次打开回收站时消失，而他会坚持说他昨天看还在。这类"差一个等号"
// 的差别在真实使用中无法与 bug 区分。
func TestCleanTrashBoundaryKeepsExactlyAtLimit(t *testing.T) {
	e := newTrashEnv(t)
	it := e.del(t, e.mk(t, "disk", "a.txt", "x"))
	e.clk.advance(DefaultTrashRetain) // 正好到点
	if n, err := e.svc.CleanTrash(context.Background()); err != nil || n != 0 {
		t.Fatalf("正好到保留期该保留: n=%d err=%v", n, err)
	}
	if items := e.list(t); len(items) != 1 || items[0].ID != it.ID {
		t.Errorf("条目没了: %+v", items)
	}
	e.clk.advance(time.Second)
	if n, err := e.svc.CleanTrash(context.Background()); err != nil || n != 1 {
		t.Fatalf("过点一秒该清掉: n=%d err=%v", n, err)
	}
}

// 保留期被夹进 [1,90] 天。
//
// 装配层夹边界而不是报错：NewService 没有错误返回。而"传了 10 秒结果
// 真的 10 秒就删用户文件"是不可接受的 —— 设置页的输入框归 M7-T3 管，
// 装配层这一道是最后防线。
func TestTrashRetainClamped(t *testing.T) {
	cases := []struct {
		name     string
		set      time.Duration
		advance  time.Duration
		wantGone bool
	}{
		{"零值取默认 3 天", 0, 48 * time.Hour, false},
		{"零值超过 3 天则清", 0, 73 * time.Hour, true},
		{"过短夹到 1 天", 10 * time.Second, time.Hour, false},
		{"1 天零 1 秒真清", 10 * time.Second, 24*time.Hour + time.Second, true},
		{"过长夹到 90 天", 400 * 24 * time.Hour, 91 * 24 * time.Hour, true},
		{"90 天内保留", 400 * 24 * time.Hour, 89 * 24 * time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTrashEnv(t)
			e.svc = NewService(Options{
				Clock:          e.clk.Now,
				TrashRetain:    tc.set,
				TrashRoots:     func(context.Context) ([]string, error) { return e.roots, nil },
				FilesystemRoot: e.svc.fsRoot,
			})
			e.del(t, e.mk(t, "disk", "a.txt", "x"))
			e.clk.advance(tc.advance)
			n, err := e.svc.CleanTrash(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := n == 1; got != tc.wantGone {
				t.Errorf("清掉=%v 期望=%v（set=%v advance=%v）", got, tc.wantGone, tc.set, tc.advance)
			}
		})
	}
}

// ---------- 跨重启 ----------

// 面板重启后条目必须还在、还能还原。
//
// 回收站的一切状态都在盘上（条目目录 + 元信息），没有任何进程内记忆。
// 这条用"重建 Service"模拟升级/OOM/systemd 拉起：如果元信息其实记在
// 内存或数据库里，这里就会红。
func TestTrashSurvivesRestart(t *testing.T) {
	e := newTrashEnv(t)
	p := e.mk(t, "disk", "a.txt", "内容")
	e.del(t, p)

	e.svc = e.assemble() // 全新实例，同一批盘
	items := e.list(t)
	if len(items) != 1 {
		t.Fatalf("重启后条目该还在, got %+v", items)
	}
	if items[0].Origin != p || items[0].Name != "a.txt" {
		t.Errorf("元信息不全: %+v", items)
	}
	if _, err := e.svc.RestoreTrash(context.Background(), items[0].ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "内容" {
		t.Errorf("重启后还原的内容不对: %q", b)
	}
}

// ---------- 真实实现的两条环境查询 ----------

// 真实锚点查找：靠设备号，而不是靠"看起来像挂载点"。
//
// 用 /tmp 与 $HOME 这两个必然存在、且在本机同盘的目录，加一个必然不同盘
// 的路径来跑。这里不注入：被测的正是"真的去问文件系统"这件事，注入它
// 就等于什么都没测。
func TestRealFilesystemRootMatchesMount(t *testing.T) {
	tmp := t.TempDir()
	got, err := realFilesystemRoot(filepath.Join(tmp, "子目录", "文件.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || strings.Contains(got, "子目录") {
		t.Errorf("锚点不该带路径后半段: %q", got)
	}
	// 同一个目录树里的两个点必须给同一个锚点
	deep := filepath.Join(tmp, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	g2, err := realFilesystemRoot(deep)
	if err != nil {
		t.Fatal(err)
	}
	if g2 != got {
		t.Errorf("同盘的两个点锚点不同: %q vs %q", got, g2)
	}
	// 与真实 / 比较：必须是个真实存在的目录
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Errorf("锚点 %q 不是真实目录: %v", got, err)
	}
}

// 不存在的文件也要能定锚点（它的父目录在就行）。
//
// 删除一个刚被别处删掉的文件时，我们仍然需要知道"它本来在哪个盘"，
// 否则连报错都不知道该往哪个回收站看。
func TestRealFilesystemRootMissingTail(t *testing.T) {
	tmp := t.TempDir()
	a, err := realFilesystemRoot(filepath.Join(tmp, "不存在.txt"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := realFilesystemRoot(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("尾巴不存在时锚点漂了: %q vs %q", a, b)
	}
}

// ---------- 真实"管哪些盘"的枚举 ----------

// 盘的枚举必须复用 metrics 的过滤，且一个设备只留一个锚点。
//
// 这里换成真实的 discoverTrashRoots（不注入 TrashRoots），喂一份服务器
// 挂载表夹具。夹具里 /DISK 与 /mnt/disk-mirror 同为 /dev/sda1 —— 去重
// 没做对的话同一个盘会有两个回收站，"清空回收站"只清了一个，用户在
// 界面上看到的另一半条目永远清不掉。伪文件系统、loop、/boot 也必须不在。
func TestDiscoverTrashRootsDedupesByDevice(t *testing.T) {
	svc := NewService(Options{ProcDir: filepath.Join("testdata", "proc_server")})
	got, err := svc.discoverTrashRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"/": true, "/DISK": true, "/home": true}
	if len(got) != len(want) {
		t.Fatalf("应恰好 %v, got %v", keysOf(want), got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("多出来的盘 %q（伪文件系统/loop/boot/同设备重复挂载都不该出现）", g)
		}
	}
}

// 一个真盘都没枚举到时必须兜底 "/"。
//
// 与 Roots 同一条理由：容器里跑面板时挂载表可能全是 overlay。没有兜底，
// "删除"在这台机器上会**永远**失败，而报错写的是"回收站不可写"，用户
// 无从知道面板其实根本没找到盘。
func TestDiscoverTrashRootsFallbackRoot(t *testing.T) {
	for _, dir := range []string{"proc_empty", "proc_container"} {
		svc := NewService(Options{ProcDir: filepath.Join("testdata", dir)})
		got, err := svc.discoverTrashRoots(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if len(got) != 1 || got[0] != "/" {
			t.Errorf("%s 应兜底成 [/], got %v", dir, got)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------- 符号链接 ----------

// 删一个符号链接要删**链接本身**，不能顺着它把目标搬进回收站。
//
// 与 Rename 同一个陷阱（TestRenameSymlinkItself 抓过一次）：只要 Delete
// 用了"整条路径解析符号链接"的那条路径，删链接就会变成删链接指向的文件。
// 这里让链接指向另一个盘上的文件 —— 真发生了顺链删除，目标会被复制到
// 链接所在盘的回收站里，同时踩中两个禁忌。
func TestDeleteSymlinkItself(t *testing.T) {
	e := newTrashEnv(t)
	target := e.mk(t, "ssd", "真实文件.txt", "重要")
	link := filepath.Join(e.disk, "快捷方式")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("这个文件系统不支持符号链接: %v", err)
	}
	it := e.del(t, link)
	if !exists(target) {
		t.Fatal("链接指向的文件被搬走了：删链接应该只删链接")
	}
	if it.IsDir {
		t.Error("符号链接不该被当成目录")
	}
	// 条目内容必须还是个符号链接（还原回去才是原来那个链接）
	fi, err := os.Lstat(it.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("条目不再是符号链接: mode=%v", fi.Mode())
	}
}

// 指向不存在目标的坏链接也能删。
//
// 这类链接是 rm 都常被绊倒的东西；面板至少不能因为"stat 跟丢了目标"
// 就报一个"文件不存在"然后什么都不做。
func TestDeleteDanglingSymlink(t *testing.T) {
	e := newTrashEnv(t)
	e.mkdir(t, "disk", "d")
	link := filepath.Join(e.disk, "d", "坏链接")
	if err := os.Symlink(e.root("disk", "压根没有.txt"), link); err != nil {
		t.Skipf("这个文件系统不支持符号链接: %v", err)
	}
	it := e.del(t, link)
	if exists(link) {
		t.Error("坏链接还在")
	}
	if it.Name != "坏链接" {
		t.Errorf("Name = %q", it.Name)
	}
}

// ---------- 条目 id 是个用户可控的字符串 ----------

// 恶意 id 必须被拒，而且不能碰盘。
//
// id 从 URL 路径参数来，而它会被拼成 <盘根>/.trash/<id> 去读、去 RemoveAll。
// 没有字符集校验，一个带 ".." 或斜杠的 id 就能把"永久删除一个回收站
// 条目"变成"永久删除回收站外面的任何东西" —— 那是一条从 HTTP 通往任意
// 路径的路，而接口是带 CSRF 头就够的登录态。
//
// 三个吃 id 的方法都要测：校验只写在其中一处时，另外两处照样是开的，
// 而代码看起来"已经防过了"。
func TestTrashIDRejectsTraversal(t *testing.T) {
	e := newTrashEnv(t)
	e.del(t, e.mk(t, "disk", "a.txt", "x"))
	// 先在外面放一个"如果被删了就能看出来"的牺牲品
	victim := e.mk(t, "disk", "victim.txt", "别删我")
	for _, id := range []string{"../../victim.txt", "../a.txt.meta.json", "..", ".", "...", "a/b", "a\\b", "", strings.Repeat("x", 300)} {
		if _, err := e.svc.RestoreTrash(context.Background(), id); !errors.Is(err, ErrBadPath) {
			t.Errorf("RestoreTrash(%q) 应 ErrBadPath, got %v", id, err)
		}
		if err := e.svc.PurgeTrash(context.Background(), id); !errors.Is(err, ErrBadPath) {
			t.Errorf("PurgeTrash(%q) 应 ErrBadPath, got %v", id, err)
		}
		if !exists(victim) {
			t.Fatalf("牺牲品被 %q 删掉了：id 拼进了路径而没有校验", id)
		}
	}
	// 条目也还在（一次失败的查找不该有任何副作用）
	if items := e.list(t); len(items) != 1 {
		t.Errorf("恶意 id 的查找动到了条目: %+v", items)
	}
}

// 目录条目的 Size 恒为 0 —— 这是决定，不是漏实现。
//
// 算一个目录有多大要遍历整棵树，而"删掉 node_modules / .git / 一个备份
// 目录"就会让删除变成 O(文件数) 的遍历，正好摧毁"删除必须瞬间完成"这
// 条本设计的立身之本。列举侧（browse.go）对目录同样给 0，两边一致。
//
// 写下来是为了让下一个人看到 size:0 时不去"顺手补上"：补上之后测试会红，
// 而红了的人需要知道红的是决定，不是 bug。
func TestTrashDirectorySizeIsZeroByDecision(t *testing.T) {
	e := newTrashEnv(t)
	dir := e.mkdir(t, "disk", "big")
	var total int
	for i := 0; i < 5; i++ {
		body := strings.Repeat("y", 1000)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		total += len(body)
	}
	it := e.del(t, dir)
	if !it.IsDir {
		t.Fatal("应是目录条目")
	}
	if it.Size != 0 {
		t.Errorf("目录条目的 Size 必须是 0（见本测试注释），got %d（文件总数 5，共 %d 字节）", it.Size, total)
	}
	// 而文件条目的 Size 是真大小 —— 两者不同的唯一解释就是目录那一头
	// 是"不算"而不是"算错"
	f := e.del(t, e.mk(t, "disk", "small.bin", strings.Repeat("z", 1234)))
	if f.Size != 1234 {
		t.Errorf("文件大小该是真的, got %d", f.Size)
	}
}

// ---------- 变异测试逼出来的四条 ----------

// 原路径被一个**坏符号链接**占住时，还原必须拒绝。
//
// 占用检查用 Lstat 而不是 Stat：Stat 会跟着链接去找目标，目标不存在就
// 报 ErrNotExist，于是实现认为"位置空着"，接着 rename 把这个链接悄悄
// 换成真文件 —— 用户原本那条（暂时的、目标稍后会挂载回来的）链接从此
// 消失，而还原界面上写着"已还原到 xxx"。
func TestRestoreRefusesDanglingSymlinkAtOrigin(t *testing.T) {
	e := newTrashEnv(t)
	p := e.mk(t, "disk", "a.txt", "旧内容")
	it := e.del(t, p)
	// 原位置现在被一个指向虚空（将来会挂回来）的链接占着
	if err := os.Symlink(e.root("disk", "等会儿才存在.img"), p); err != nil {
		t.Skipf("这个文件系统不支持符号链接: %v", err)
	}
	if _, err := e.svc.RestoreTrash(context.Background(), it.ID); !errors.Is(err, ErrExists) {
		t.Fatalf("坏符号链接也算占用, got %v", err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("那个链接被还原悄悄替换掉了")
	}
}

// 同一秒内"删掉→重建→再删同一个路径"必须得到两个条目。
//
// 条目名带原路径哈希 + 秒级时间戳，所以这种序列会**造出同名条目**：
// 日志被程序秒级重建、或者用户删了又还原又删，都会撞上。占位靠的是
// O_EXCL，换成 O_TRUNC 的话第二次删除会直接改写第一个条目的元信息、
// 并把新载荷盖到旧载荷上 —— 第一个条目无声消失，界面上从没出现过
// "两次删除"，那是货真价实的数据丢失。
func TestDeleteSamePathTwiceInSameSecond(t *testing.T) {
	e := newTrashEnv(t)
	first := e.mk(t, "disk", "app.log", "第一天的内容")
	e.del(t, first)
	// 假时钟不动 = 同一秒；程序把同名文件重建出来
	if err := os.WriteFile(first, []byte("第二天的内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.del(t, first)

	items := e.list(t)
	if len(items) != 2 {
		t.Fatalf("两次删除该有两个条目（各自内容都在），got %d: %+v", len(items), items)
	}
	if items[0].ID == items[1].ID {
		t.Fatalf("两条同 ID，其中一条必然覆盖了另一条: %s", items[0].ID)
	}
	seen := map[string]bool{}
	for _, it := range items {
		b, err := os.ReadFile(it.Path)
		if err != nil {
			t.Fatal(err)
		}
		seen[string(b)] = true
	}
	for _, want := range []string{"第一天的内容", "第二天的内容"} {
		if !seen[want] {
			t.Errorf("内容 %q 丢了（同名条目互相覆盖）", want)
		}
	}
}

// 回收站目录必须是 0700。
//
// 回收站里装的正是用户"以为自己删掉了"的东西 —— 日志、备份、导出的
// 配置，里面常常带着令牌和他人数据。放宽成 0755 的话，同一台机器上
// 任何账号都能把"已删除"的文件翻出来，而用户看到的界面写着已删除。
// （多用户机器上面板以 root 跑，这条更是唯一防线。）
func TestTrashDirIsNotWorldReadable(t *testing.T) {
	e := newTrashEnv(t)
	it := e.del(t, e.mk(t, "disk", "secret.txt", "token=abc"))
	fi, err := os.Stat(filepath.Dir(it.Path)) // .trash 自己
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("回收站目录权限 %v 允许同机其它账号读已删除内容", fi.Mode().Perm())
	}
}

// 元信息里 Origin 不是绝对路径的条目必须当"不是条目"。
//
// 回收站是个用户能手工往里放东西的目录。认条目只认"旁边有个能解析的
// JSON"是不够的：一个相对 Origin 会让还原时在当前工作目录下建目录树，
// 而"清理"会把这整个东西当条目删掉。宁可跳过 —— 跳过最多让一个坏条目
// 赖在回收站里，误认则删用户文件。
func TestTrashSkipsEntriesWithRelativeOrigin(t *testing.T) {
	e := newTrashEnv(t)
	e.del(t, e.mk(t, "disk", "real.txt", "x"))
	trash := filepath.Join(e.disk, DefaultTrashDirName)
	// 手工造一个"像条目"的目录 + 相对 Origin 的 meta
	bad := filepath.Join(trash, "9999-伪造条目-abcdef12")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"我的备份","origin":"相对/路径","is_dir":true,"size":0,"deleted_at":1}`
	if err := os.WriteFile(bad+".meta.json", []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	items := e.list(t)
	for _, it := range items {
		if it.ID == "9999-伪造条目-abcdef12" {
			t.Error("元信息不完整的目录被当成条目了")
		}
	}
	if _, err := e.svc.CleanTrash(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(bad) {
		t.Error("清理把用户自己放的目录删了 —— 数据丢失")
	}
}

// ---------- 锚点行走的内核（合成文件系统表）----------

// 锚点必须停在真正换了设备的那一层。
//
// 真实 syscall 版本只能在本机测，而本机可能整台只有一个文件系统 ——
// 那时"往上多走了一层"（把 /data 上的文件认成 / 上的）在真实环境的
// 测试里根本显现不出来：锚点对不对取决于跑测试的机器长什么样。有了
// 这张合成表，多盘、单盘、读不到的父目录都是任何机器上都能钉住的用例。
func TestWalkToAnchorSynthetic(t *testing.T) {
	// devOf 用一张假表：不在表里的路径 = 明确不存在
	table := func(devs map[string]uint64) func(string) (uint64, pathState) {
		return func(p string) (uint64, pathState) {
			d, ok := devs[p]
			if !ok {
				return 0, pathGone
			}
			if d == 0 {
				return 0, pathUnknown // 约定：表里写 0 表示"判不出来"
			}
			return d, pathHere
		}
	}
	cases := []struct {
		name string
		devs map[string]uint64
		path string
		want string
	}{
		{
			// 典型：/data 挂在设备上，父目录 / 是另一个设备
			"挂载点内", map[string]uint64{"/": 1, "/data": 7, "/data/a": 7, "/data/a/b": 7},
			"/data/a/b/c/d.txt", "/data",
		},
		{
			// 尾巴不存在也能定锚点（刚被别处删掉的文件）
			"尾巴不存在", map[string]uint64{"/": 1, "/data": 7, "/data/a": 7},
			"/data/a/没有这个.txt", "/data",
		},
		{
			// 整台机器只有一个文件系统：锚点是 /，不能报错也不能漂
			"单机一盘", map[string]uint64{"/": 1, "/a": 1, "/a/b": 1},
			"/a/b/c", "/",
		},
		{
			// 父目录判不出来（0700 挡路）：停在能确认的最深层，
			// 而不是继续往上把文件认到别的盘上去
			"父目录读不到", map[string]uint64{"/": 1, "/locked": 0, "/locked/x": 9},
			"/locked/x/f", "/locked/x",
		},
		{
			// 深嵌套挂载：/DISK 与 /DISK/sub 是两块盘（内层挂载遮蔽外层）
			"挂载遮蔽", map[string]uint64{"/": 1, "/DISK": 3, "/DISK/sub": 5, "/DISK/sub/deep": 5},
			"/DISK/sub/deep/f", "/DISK/sub",
		},
		{
			// 整条路径都不存在到根：明确的错误而不是静默给个锚点
			"全都不存在", map[string]uint64{}, "/x/y", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := walkToAnchor(tc.path, table(tc.devs))
			if tc.want == "" {
				if err == nil {
					t.Fatalf("该报错, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("锚点 = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

// 真实设备上：/ 自己必须在 "/" 定锚。
//
// 这条与上一条互补：合成表测的是行走逻辑，这条确认 lstat→设备号那层
// 接线没错（把 devOf 接反成恒 pathUnknown 时，合成表照样全绿）。
func TestRealFilesystemRootAnchorsAtRoot(t *testing.T) {
	got, err := realFilesystemRoot("/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/" {
		t.Errorf("/ 的锚点必须是 /，got %q", got)
	}
}

// ---------- PreflightTrash（受理时的回收站可写性检查）----------

// addDisk 往假挂载表里再加一块盘。
//
// 不用重建 Service：夹具里的 FilesystemRoot/TrashRoots 是两个每次调用都
// 现读 e.roots 的闭包，所以 append 立刻对已存在的 svc 生效。
func (e *trashEnv) addDisk(dir string) {
	e.roots = append(e.roots, dir)
}

// 盘根写不进回收站时，**受理时**就要报 ErrTrashUnwritable。
//
// 删除改走队列之后，这个错误本来只会出现在任务执行时（抽屉里一条红色）。
// 那丢掉了一条有意的产品保证：界面上"这个盘只能永久删除"那个按钮，靠的是
// HTTP 拿到 ErrTrashUnwritable 这个**哨兵**（errors.Is）才能亮起来；错误
// 只能在任务里看到的话，前端就只剩"对中文错误文本做子串匹配"这一条路，
// 而这条路在本项目里被明令禁止（改一句文案就会把 422 变成没有）。
//
// 所以这里把它提前到受理时。这不是"顺手 stat 一下源"那种提前校验 ——
// 源存在性会过期（检查完到执行之间文件可能被人删了），而"这块盘的回收站
// 建得起来"是**盘的性质**，稳定得多；且执行时的检查照样会跑，提前检查只是
// 多加一次提前的坏消息，不会让任何判定失效。
func TestPreflightTrashRejectsUnwritableRoot(t *testing.T) {
	e := newTrashEnv(t)
	p := e.mk(t, "disk", "a.txt", "x")
	// 先造好文件**再**锁盘：反过来会在 mk 这一步就 permission denied，
	// 测到的是夹具而不是预检。
	if err := os.Chmod(e.disk, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.disk, 0o755) })
	err := e.svc.PreflightTrash(context.Background(), []string{p}, false)
	if !errors.Is(err, ErrTrashUnwritable) {
		t.Fatalf("盘根只读该报 ErrTrashUnwritable, got %v", err)
	}
	// 提示里要说得出下一步：只能永久删除，或改用 SFTP
	if !strings.Contains(err.Error(), "永久删除") {
		t.Errorf("错误该指出唯一出路, got %q", err)
	}
}

// permanent=true 不需要回收站，因此不该拦。
//
// 这条方向反过来就是死锁：盘根写不进去 + 唯一可行的删除方式被自己的前置
// 检查拒掉 = 用户在这个盘上一个文件都删不了。
func TestPreflightTrashSkippedForPermanent(t *testing.T) {
	e := newTrashEnv(t)
	p := e.mk(t, "disk", "a.txt", "x")
	if err := os.Chmod(e.disk, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.disk, 0o755) })
	if err := e.svc.PreflightTrash(context.Background(), []string{p}, true); err != nil {
		t.Errorf("永久删除不该要求回收站可写: %v", err)
	}
}

// 多个盘里只有一个写不进去：要报出来，且说的是**那块盘**。
//
// 只报"回收站不可写"而不说是哪块盘的话，用户会在能写的那块盘上反复重试。
func TestPreflightTrashNamesTheOffendingDisk(t *testing.T) {
	e := newTrashEnv(t)
	good := e.mk(t, "disk", "b.txt", "x")
	bad := e.mk(t, "home", "a.txt", "x")
	// home 盘只读，disk 盘保持正常（在文件造好之后才锁）
	if err := os.Chmod(e.home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.home, 0o755) })
	err := e.svc.PreflightTrash(context.Background(), []string{good, bad}, false)
	if !errors.Is(err, ErrTrashUnwritable) {
		t.Fatalf("该报 ErrTrashUnwritable, got %v", err)
	}
	if !strings.Contains(err.Error(), e.home) {
		t.Errorf("错误该指出是哪块盘, got %q", err)
	}
	// 不能把可写的那块盘也一起拒了：否则一个只读盘会让所有删除都做不了
	if strings.Contains(err.Error(), e.disk) {
		t.Errorf("不该把可写的盘也说成有问题: %q", err)
	}
}

// 路径不存在时**不在这里**报错：那是执行时的事。
//
// 受理阶段的职责只有"这个请求能不能做"，而"做的时候东西还在不在"归执行
// （那里还得再判一次，因为中间可能已经变了）。把两种错混在一处，界面就
// 分不清"你选的东西没了"与"这块盘不行"，而两者给用户的下一步完全不同。
func TestPreflightTrashIgnoresMissingPaths(t *testing.T) {
	e := newTrashEnv(t)
	if err := e.svc.PreflightTrash(context.Background(),
		[]string{filepath.Join(e.disk, "不存在.txt")}, false); err != nil {
		t.Errorf("源不存在不该由预检报错: %v", err)
	}
}

// 回收站已经建好时，预检必须**不动盘**（不 chmod、不改时间）。
//
// 预检是每次删除都要跑的热路径。已存在的 .trash 每次都重刷权限的话，
// 一次框选删除会顺带改动回收站目录的 ctime —— 用户会因为"只是想删个文件"
// 看到回收站目录的元信息在变。更要紧的是：ensureTrashDir 本身就有收紧
// 权限的副作用，预检复制它等于把副作用做两遍。
func TestPreflightTrashDoesNotTouchExistingTrash(t *testing.T) {
	e := newTrashEnv(t)
	p := e.mk(t, "disk", "a.txt", "x")
	// 先做一次真删除，把 .trash 建起来
	if _, err := e.svc.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	trash := filepath.Join(e.disk, e.svc.TrashDirName())
	st1, err := os.Stat(trash)
	if err != nil {
		t.Fatal(err)
	}
	// 换个时间戳，若预检碰了这个目录就会被看到
	soon := time.Unix(1600000000, 0)
	if err := os.Chtimes(trash, soon, soon); err != nil {
		t.Fatal(err)
	}
	q := e.mk(t, "disk", "b.txt", "y")
	if err := e.svc.PreflightTrash(context.Background(), []string{q}, false); err != nil {
		t.Fatalf("回收站可用时预检不该报错: %v", err)
	}
	st2, err := os.Stat(trash)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(soon) {
		t.Errorf("预检动了回收站目录: mtime %v -> %v", soon, st2.ModTime())
	}
	if st1.Mode() != st2.Mode() {
		t.Errorf("预检改了回收站权限: %v -> %v", st1.Mode(), st2.Mode())
	}
}

// 一批同盘路径只回一个盘的错。
//
// 这条**不**声称在验证"按盘去重"。去重只改变系统调用次数，而预检一发现
// 不可写就立刻返回 —— 观测面上两种实现一字不差（实测把 `seen[mount]`
// 整个关掉，全仓测试仍然全绿）。要真钉住"查几次"就得给 trashWritable 开
// 一个计数注入口，为一个纯成本护栏往生产代码里加接缝不值。
//
// 所以这条钉的是能观测的那一半：50 个路径配一块坏盘，错误里只出现那块盘
// 一次。少了它会退化成"同一个盘的同一个问题被写进错误五十遍"，那才是
// 用户在抽屉里真正看到的东西。
func TestPreflightTrashReportsOneDiskOnce(t *testing.T) {
	e := newTrashEnv(t)
	var paths []string
	for i := 0; i < 50; i++ {
		paths = append(paths, e.mk(t, "disk", "f"+itoa(i), "x"))
	}
	// 文件都造好之后再锁盘
	if err := os.Chmod(e.disk, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.disk, 0o755) })
	err := e.svc.PreflightTrash(context.Background(), paths, false)
	if !errors.Is(err, ErrTrashUnwritable) {
		t.Fatalf("该报错, got %v", err)
	}
	if n := strings.Count(err.Error(), e.disk); n != 1 {
		t.Errorf("同一块盘该只说一次, got %d 次: %q", n, err)
	}
}
