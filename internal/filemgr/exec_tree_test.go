package filemgr

// 复制目录树。
//
// 难点都不在"把文件一个个拷过去"，而在三件容易做错的事：符号链接不能跟
// 进去、目录的元信息必须在填完之后才设（否则自己写进去的文件会把目录的
// mtime 又改掉）、中途取消要把已经建出来的整棵半棵树收干净。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// treeSpec 是一棵小目录树的描述：键是相对路径，值非空则是文件内容，
// 以 "/" 结尾则是空目录，"@" 开头则是符号链接（指向后面的路径）。
type treeSpec map[string]string

// buildTree 按 spec 建一棵树，回根目录绝对路径。
func buildTree(t testing.TB, root string, spec treeSpec) string {
	t.Helper()
	for rel, content := range spec {
		p := filepath.Join(root, rel)
		switch {
		case strings.HasSuffix(rel, "/"):
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		case strings.HasPrefix(content, "@"):
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(strings.TrimPrefix(content, "@"), p); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// walkTree 把一棵树扁平化成 {相对路径: 描述}，用于比较两棵树是否同构。
// 符号链接记成 "@指向"，目录记成 "/"。
func walkTree(t testing.TB, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			to, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			out[rel] = "@" + to
		case fi.IsDir():
			out[rel] = "/"
		default:
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffTrees(a, b map[string]string) string {
	var b2 []string
	for k, v := range a {
		if bv, ok := b[k]; !ok {
			b2 = append(b2, "只在源里有: "+k)
		} else if bv != v {
			b2 = append(b2, "内容不同: "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			b2 = append(b2, "只在目标里有: "+k)
		}
	}
	return strings.Join(b2, "; ")
}

// ---------- 内容同构 ----------

// 一棵含文件/空目录/嵌套/符号链接的树，复制后必须与原树同构。
func TestCopyTreeIsomorphic(t *testing.T) {
	dir := varTempDir(t)
	src := buildTree(t, filepath.Join(dir, "src"), treeSpec{
		"a.txt":          "A",
		"sub/":           "",
		"sub/b.txt":      "BB",
		"sub/deep/":      "",
		"sub/deep/c.txt": "CCC",
		"link":           "@a.txt",
		"sub/dangle":     "@不存在的地方",
		"中文 文件.txt":      "中文内容",
	})
	dst := filepath.Join(dir, "dst")

	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	got := walkTree(t, dst)
	want := walkTree(t, src)
	if d := diffTrees(want, got); d != "" {
		t.Errorf("复制后的树与原树不同构:\n%s", d)
	}
}

// 链接成环也不能把复制变成无限递归。
//
// 日志目录里这是真实存在的形态（/var/log 里 `ln -s . self` 一类）。因为
// 复制链接而不跟随，环在结构上就走不进去 —— 但"走不进去"必须是**测过**的：
// 一个把链接当文件跟随的实现会在这里 recursion 到栈溢出，把整台面板带走。
func TestCopyTreeSymlinkLoopTerminates(t *testing.T) {
	dir := varTempDir(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "logs", "a.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// logs/self -> logs（自指目录链接）
	if err := os.Symlink(filepath.Join(src, "logs"), filepath.Join(src, "logs", "self")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	s := NewService(Options{})
	// 没有超时的话，跟随链接的实现会永远走下去（栈溢出前不返回）。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.copyTree(ctx, src, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	got := walkTree(t, dst)
	if _, ok := got[filepath.Join("logs", "self")]; !ok {
		t.Error("链接本身该被复制过来")
	}
	// 只该有 3 个条目（logs/、logs/a.log、logs/self），不是无限展开
	if len(got) != 3 {
		t.Errorf("条目数该是 3（链接不展开）, got %d: %v", len(got), got)
	}
}

// 空目录必须能复制（"只有目录没有文件"的树是常见形态：一个刚建好的项目骨架）。
func TestCopyTreeEmptyDirs(t *testing.T) {
	dir := varTempDir(t)
	src := buildTree(t, filepath.Join(dir, "src"), treeSpec{
		"a/":     "",
		"b/c/d/": "",
	})
	dst := filepath.Join(dir, "dst")
	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a", filepath.Join("b", "c", "d")} {
		fi, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("%s 没复制出来: %v", rel, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s 该是目录", rel)
		}
	}
}

// ---------- 目录元信息的时机 ----------

// 目录的权限/时间必须在**填完之后**设置。
//
// 这不是吹毛求疵：往一个目录里写文件会**把该目录的 mtime 改成现在**。所以
// 先 chmod/chtimes 再往里拷文件的实现，拷完看到的 mtime 全是"刚刚"，而
// 权限也会被建子目录时的 umask 影响。用户按时间排序看备份树时，整棵树会
// 显示成同一秒。正确做法是后序（子层填完再设自己）。
func TestCopyTreeDirMtimeAppliedLast(t *testing.T) {
	dir := varTempDir(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1_500_000_000, 0)
	// 先填文件、再设目录时间：源的目录时间就是 old
	for _, p := range []string{src, filepath.Join(src, "sub")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(dir, "dst")
	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".", "sub"} {
		fi, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Truncate(time.Second).Equal(old.Truncate(time.Second)) {
			t.Errorf("%s 的目录 mtime 该是 %v, got %v（目录元信息可能在填内容之前就被覆盖了）",
				rel, old, fi.ModTime())
		}
	}
}

// 目录权限也要带过去（0700 的私有目录复制完不能变成 0755）。
func TestCopyTreePreservesDirMode(t *testing.T) {
	dir := varTempDir(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "private", "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, "private"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("私有目录权限该保留 0700, got %o", fi.Mode().Perm())
	}
}

// ---------- 拒绝覆盖 ----------

// 目标已存在 → ErrExists，且**不得有任何改动**。
func TestCopyTreeRefusesExistingDst(t *testing.T) {
	dir := varTempDir(t)
	src := buildTree(t, filepath.Join(dir, "src"), treeSpec{"a.txt": "新"})
	dst := filepath.Join(dir, "dst")
	buildTree(t, dst, treeSpec{"keep.txt": "别碰"})
	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, dst, nopProgress); !errors.Is(err, ErrExists) {
		t.Fatalf("应 ErrExists, got %v", err)
	}
	got := walkTree(t, dst)
	if string(got["keep.txt"]) != "别碰" {
		t.Errorf("已有内容被改动: %v", got)
	}
	if _, ok := got["a.txt"]; ok {
		t.Error("拒绝覆盖时不该写进任何东西")
	}
}

// ---------- 取消：收干净半棵树 ----------

// 中途取消必须把**已经建出来的那半棵树**整个删掉。
//
// 单层复制留半截文件已经够糟；目录树留半截更糟：用户看到的是一棵"看起来
// 在"的树，里面缺了三分之二，而他以为复制完成了。
func TestCopyTreeCancelRemovesPartialTree(t *testing.T) {
	dir := varTempDir(t)
	spec := treeSpec{}
	for i := 0; i < 60; i++ {
		spec[filepath.Join("d"+itoa(i), "f.txt")] = strings.Repeat("x", 4096)
	}
	src := buildTree(t, filepath.Join(dir, "src"), spec)
	dst := filepath.Join(dir, "dst")

	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	rep := func(doneBytes int64, entries int) error {
		seen++
		if seen >= 3 {
			cancel()
		}
		return nil
	}
	s := NewService(Options{})
	_, err := s.copyTree(ctx, src, dst, rep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应 context.Canceled, got %v", err)
	}
	if _, lerr := os.Lstat(dst); !errors.Is(lerr, os.ErrNotExist) {
		// 报出来时顺手列出还剩什么，否则这个失败几乎没法诊断
		left := map[string]bool{}
		_ = filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				rel, _ := filepath.Rel(dst, p)
				left[rel] = true
			}
			return nil
		})
		t.Errorf("取消后必须删掉整棵半成品树, 残留 %d 项", len(left))
	}
	// 源必须完好：每个 spec 里的文件都还在、内容没动。
	// （不是比条目数：spec 的 60 个键会建出 60 个目录 + 60 个文件，
	// walkTree 把目录也算条目，比长度只会比出一个假错。）
	for rel, want := range spec {
		got, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil || string(got) != want {
			t.Fatalf("复制被取消时把源弄坏了: %s (%v)", rel, err)
		}
	}
}

// 中途单个文件失败（这里是某个子目录不可读）也要把整棵半棵树删掉。
//
// "尽力而为、把成功的留下"在这里是错的方向：用户收到的是一条失败消息，
// 而盘上留着一棵缺东西的树 —— 他既不能当它成功、也不知道少了什么。要么
// 完整要么没有。
func TestCopyTreeFailureRemovesPartialTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位，造不出\"读不了\"的目录")
	}
	dir := varTempDir(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "ok", "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 一个读不进去的子目录：遍历到它必然失败
	blocked := filepath.Join(src, "locked")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	dst := filepath.Join(dir, "dst")
	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, dst, nopProgress); err == nil {
		t.Fatal("有一个目录读不了就该失败")
	}
	if _, lerr := os.Lstat(dst); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("失败后必须删掉整棵半成品树, got stat=%v", lerr)
	}
}

// ---------- 进度 ----------

// 树复制要同时报字节与**条目数**：界面上一条"复制 node_modules"的进度显示
// "已处理 1200/8000 个文件"，只有字节数时用户完全不知道还要多久。
func TestCopyTreeReportsBytesAndEntries(t *testing.T) {
	dir := varTempDir(t)
	spec := treeSpec{}
	const n = 12
	for i := 0; i < n; i++ {
		spec["f"+itoa(i)] = strings.Repeat("y", 2048)
	}
	src := buildTree(t, filepath.Join(dir, "src"), spec)
	dst := filepath.Join(dir, "dst")

	var lastBytes int64
	var lastEntries int
	samples := 0
	rep := func(doneBytes int64, entries int) error {
		samples++
		lastBytes, lastEntries = doneBytes, entries
		return nil
	}
	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, dst, rep); err != nil {
		t.Fatal(err)
	}
	if lastEntries < n {
		t.Errorf("条目数该至少 %d, got %d", n, lastEntries)
	}
	if lastBytes < n*2048 {
		t.Errorf("累计字节该至少 %d, got %d", n*2048, lastBytes)
	}
	if samples == 0 {
		t.Error("一次都没上报进度")
	}
}

// 复制一棵树的过程中，目标必须**只写不读回**：也就是失败/取消的清理不能
// 依赖"再遍历一遍源"。这条通过"源在复制中途被改"来间接验证语义边界：
// 已经拷过的条目不受源变动影响。
func TestCopyTreeResultMatchesSnapshot(t *testing.T) {
	dir := varTempDir(t)
	src := buildTree(t, filepath.Join(dir, "src"), treeSpec{"a.txt": "原内容"})
	dst := filepath.Join(dir, "dst")
	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	// 复制完再改源：副本必须停在复制那一刻的样子
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("改了"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "原内容" {
		t.Errorf("副本被源的事后改动影响了: %q", got)
	}
}

// ---------- 复制进自己的子目录 ----------

// 把一棵树复制进它自己的子目录里必须拒绝。
//
// 这是资源管理器里点两下就能做出来的操作（选中 src，粘贴到 src/backup）。
// 不拒绝的话：要么边遍历边写自己（ReadDir 与写入交错，条目数在跑的过程中
// 一直涨），要么直接递归爆炸。报错是唯一合理的答复，而它必须是**明确**的
// 错误而不是一个诡异的失败。
func TestCopyTreeRefusesCopyIntoOwnSubtree(t *testing.T) {
	dir := varTempDir(t)
	src := buildTree(t, filepath.Join(dir, "src"), treeSpec{"a.txt": "A", "sub/": ""})
	dst := filepath.Join(src, "backup")
	s := NewService(Options{})
	_, err := s.copyTree(context.Background(), src, dst, nopProgress)
	if err == nil {
		t.Fatal("复制进自己的子目录该被拒绝")
	}
	if !errors.Is(err, ErrBadPath) {
		t.Errorf("应 ErrBadPath（界面能给出\"不能复制到自身之内\"的文案）, got %v", err)
	}
	// 拒绝必须是什么都没做
	if _, lerr := os.Lstat(dst); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("拒绝时不该建出目标: %v", lerr)
	}
}

// 同理，源与目标相同也要拒绝（ErrBadPath，与 Rename 里那条一致）。
func TestCopyTreeRefusesSelfToSame(t *testing.T) {
	dir := varTempDir(t)
	src := buildTree(t, filepath.Join(dir, "src"), treeSpec{"a.txt": "A"})
	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, src, nopProgress); !errors.Is(err, ErrBadPath) {
		t.Fatalf("源与目标相同该 ErrBadPath, got %v", err)
	}
}

// ---------- 源目录自己不可写 ----------

// 复制一个 0500（可读可进、**不可写**）的目录树必须成功。
//
// 这是 dirWritable 的唯一证明：0700 的源目录本来就对自己可写，用它测
// "建目标时先加写位"什么也证明不了。而 0500 这种模式在服务器上非常常见
// （/usr/share 下的数据目录、跑完就被 chmod 成只读的发布目录）。如果建
// 目标时照搬源权限，目录一建成 0500 就把自己锁死在里面 —— 往它下面建
// 子项必然 EACCES，而这一步之后没有任何补救途径（除了再 chmod，那正是
// 本来就该最后做的事）。
func TestCopyTreeSourceDirNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位，造不出\"写不进\"的目录")
	}
	dir := varTempDir(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "data.txt"), []byte("只读树"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 先填完内容，再把整棵树设成 0500：源自己就是"读得懂、写不进"
	if err := os.Chmod(filepath.Join(src, "sub"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(src, "sub"), 0o755)
		_ = os.Chmod(src, 0o755)
	})

	dst := filepath.Join(dir, "dst")
	// 副本会被还原成 0500，而 0500 的目录里没有写位 → 删不掉里面的条目。
	// t.TempDir 自己那份 RemoveAll 一定会失败，所以先把目标树放开。
	// （注册在 varTempDir 之后→按 LIFO 先跑，顺序是对的。）
	t.Cleanup(func() {
		_ = filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	s := NewService(Options{})
	if _, err := s.copyTree(context.Background(), src, dst, nopProgress); err != nil {
		t.Fatalf("复制一个只读源树该成功: %v", err)
	}
	got := walkTree(t, dst)
	if got[filepath.Join("sub", "data.txt")] != "只读树" {
		t.Errorf("内容没拷过来: %v", got)
	}
	// 权限最终仍要还原成源的样子（建树期间的临时加写位必须撤掉）
	for _, rel := range []string{".", "sub"} {
		p := filepath.Join(dst, rel)
		if rel == "." {
			p = dst
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o500 {
			t.Errorf("%s 权限该还原成 0500, got %o", rel, fi.Mode().Perm())
		}
	}
}

// ---------- 特殊文件 ----------

// 含 FIFO 的树必须**明确失败**，而不是永久挂住。
//
// 这条是整套复制里唯一"做错了就不是错、而是死"的地方：对 FIFO 执行
// open(O_RDONLY) 在没有另一端时会**永远不返回**。在 worker 里那就是一条
// 永不结束的任务，还占着一个并发名额（默认一共才 2 个）——两个 FIFO 就
// 能把整个任务队列彻底卡死，而界面上只会显示两个"进行中"。
//
// 所以断言重点是"有超时也一定会返回"，错误类型是次要的。
func TestCopyTreeRejectsFIFO(t *testing.T) {
	dir := varTempDir(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "ok.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(src, "myfifo")
	if err := mkfifoForTest(t, fifo); err != nil {
		t.Skipf("本机造不出 FIFO: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(fifo) })

	dst := filepath.Join(dir, "dst")
	s := NewService(Options{})
	// 硬超时：实现对 FIFO 走了 open 的话，这里会是超时而不是错误。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.copyTree(ctx, src, dst, nopProgress)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("含 FIFO 的树该失败（悄悄跳过也算错：用户以为整棵都拷了）")
		}
		if !errors.Is(err, ErrUnsupportedFileType) {
			t.Errorf("应 ErrUnsupportedFileType, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("copyTree 没有返回：FIFO 上执行了 open（会永久阻塞 worker）")
	}
	// 半棵树同样要清干净
	if _, lerr := os.Lstat(dst); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("拒绝时不该留下目标树, got stat=%v", lerr)
	}
}

// mkfifoForTest 用 syscall 造一个 FIFO（os 包没有这个入口）。
func mkfifoForTest(t testing.TB, p string) error {
	t.Helper()
	return syscall.Mkfifo(p, 0o644)
}
