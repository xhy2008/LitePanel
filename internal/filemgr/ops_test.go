package filemgr

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ops.go 的单元测试。API 层的 handlers_files_test.go 已经跑过一遍真实
// filemgr 的状态码映射；这里只管**只有单测能可靠测到**的语义 ——
// 尤其是"操作的是链接本身"这一族，它的失败模式是悄悄改到链接指向的
// 目标上，API 层看状态码根本看不出来。

func TestSplitResolved(t *testing.T) {
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	dir, base, err := SplitResolved(filepath.Join(tmp, "子目录", "文件 名.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if base != "文件 名.txt" {
		t.Errorf("base = %q", base)
	}
	if dir != filepath.Join(real, "子目录") {
		t.Errorf("dir = %q, 期望 %q", dir, filepath.Join(real, "子目录"))
	}

	// 中间层是符号链接时，父目录必须解析到真实位置：用户在链接目录里
	// "新建文件夹"，东西要落进他看到的那个目录，而不是在链接旁边再建一个。
	link := filepath.Join(tmp, "链接")
	into := filepath.Join(tmp, "真实目录")
	if err := os.Mkdir(into, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(into, link); err != nil {
		t.Skipf("本版 Android/Termux 不支持符号链接: %v", err)
	}
	intoReal, err := filepath.EvalSymlinks(into)
	if err != nil {
		t.Fatal(err)
	}
	dir, base, err = SplitResolved(filepath.Join(link, "新文件"))
	if err != nil {
		t.Fatal(err)
	}
	if dir != intoReal || base != "新文件" {
		t.Errorf("链接父目录未解析: dir=%q base=%q want %q", dir, base, intoReal)
	}

	// 根的直接子项：Dir("/") == "/"，不能死循环也不能返回空串。
	// 只断言"父是 /、末段是名字"，不写死解析后的完整路径：/etc 在某些
	// 系统里自己就是符号链接（本机 Termux 把它链到 /system/etc），那并不
	// 影响拆分的正确性 —— 把环境形状写进期望值，测试就会在某些机器上
	// 因为环境恰好不合而变红。
	dir, base, err = SplitResolved("/etc")
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/" || base != "etc" {
		t.Errorf("根下条目: dir=%q base=%q", dir, base)
	}
	if _, _, err := SplitResolved("/"); err != nil {
		t.Errorf("根目录本身必须能拆: %v", err)
	}
}

// Stat 必须看链接本身。
//
// 用 Lstat 而不是 Stat 的理由：用户在文件页看到一个链接，属性对话框里
// 写着 size=20GB（指向的文件）、mtime 是目标的 —— 他会按这个决定要不要
// 删。而 rm 删的是链接（20 字节）。看到的不等于操作的，是文件管理器
// 最坑人的一类不一致。
func TestStatSeesTheLinkNotTarget(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "目标.txt")
	writeFile(t, target, "1234567890")
	link := filepath.Join(tmp, "链接.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("本版 Android/Termux 不支持符号链接: %v", err)
	}
	svc := NewService(Options{})
	e, err := svc.Stat(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if !e.IsSymlink {
		t.Fatal("链接必须标 is_symlink")
	}
	if e.Size != int64(len(target)) {
		t.Errorf("size 应是链接自身长度 %d, got %d", len(target), e.Size)
	}
	if e.Path != link {
		t.Errorf("path 必须是解析过中间层后的链接本身: %q", e.Path)
	}
	// 指向目录的链接：is_dir=false（与 List 一致）
	d := filepath.Join(tmp, "d")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	ld := filepath.Join(tmp, "ld")
	if err := os.Symlink(d, ld); err != nil {
		t.Fatal(err)
	}
	e, err = svc.Stat(context.Background(), ld)
	if err != nil {
		t.Fatal(err)
	}
	if e.IsDir || !e.IsSymlink {
		t.Errorf("指向目录的链接应 is_dir=false: %+v", e)
	}
	// 目录本身照旧
	e, err = svc.Stat(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !e.IsDir || e.IsSymlink {
		t.Errorf("目录应 is_dir=true / is_symlink=false: %+v", e)
	}
}

// 悬空链接必须能 stat 成功。
//
// 它的真实用途不是"显示"而是"能删"：悬空链接常出现在卸载/迁移之后，
// 用户要清掉它。如果 stat 报 ENOENT，前端就认定"这个东西不存在"，
// 删除入口都不给，于是它永远留在目录里。
func TestStatDanglingSymlink(t *testing.T) {
	tmp := t.TempDir()
	l := filepath.Join(tmp, "悬空")
	if err := os.Symlink(filepath.Join(tmp, "没有这个目标"), l); err != nil {
		t.Skipf("本版 Android/Termux 不支持符号链接: %v", err)
	}
	e, err := NewService(Options{}).Stat(context.Background(), l)
	if err != nil {
		t.Fatalf("悬空链接必须能 stat: %v", err)
	}
	if !e.IsSymlink || e.IsDir {
		t.Errorf("悬空链接: %+v", e)
	}
	// 完全不存在的路径仍然 404（fs.ErrNotExist）
	if _, err := NewService(Options{}).Stat(context.Background(), filepath.Join(tmp, "无")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("不存在应报 fs.ErrNotExist, got %v", err)
	}
}

// Mkdir 的三态：新建成功 / 名字被占（含悬空 SYMBOLIC LINK 占名）/ 路径非法。
func TestMkdirStates(t *testing.T) {
	ctx := context.Background()
	svc := NewService(Options{})
	tmp := t.TempDir()

	// 多级新建
	deep := filepath.Join(tmp, "a", "b 目录", "c")
	if err := svc.Mkdir(ctx, deep); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(deep); err != nil || !fi.IsDir() {
		t.Fatalf("没建出来: %v", err)
	}
	// 同名再建 → ErrExists（而不是 MkdirAll 的静默 nil）
	if err := svc.Mkdir(ctx, deep); !errors.Is(err, ErrExists) {
		t.Errorf("重复建目录应 ErrExists, got %v", err)
	}
	// 撞上普通文件
	f := filepath.Join(tmp, "f.txt")
	writeFile(t, f, "x")
	if err := svc.Mkdir(ctx, f); !errors.Is(err, ErrExists) {
		t.Errorf("撞文件应 ErrExists, got %v", err)
	}
	// 悬空符号链接也占着这个名字：MkdirAll 在它上面会得到 EEXIST，
	// 必须翻译成 ErrExists 而不是裸 syscall 错误
	dangling := filepath.Join(tmp, "悬空链接")
	if err := os.Symlink(filepath.Join(tmp, "没这个"), dangling); err == nil {
		if err := svc.Mkdir(ctx, dangling); !errors.Is(err, ErrExists) {
			t.Errorf("悬空链接占名应 ErrExists, got %v", err)
		}
	}
	// 穿过普通文件 → ErrBadPath（AbsClean 的判定，不是 syscall）
	if err := svc.Mkdir(ctx, filepath.Join(f, "子")); !errors.Is(err, ErrBadPath) {
		t.Errorf("穿过文件应 ErrBadPath, got %v", err)
	}
	// 相对路径 → ErrBadPath
	if err := svc.Mkdir(ctx, "相对/目录"); !errors.Is(err, ErrBadPath) {
		t.Errorf("相对路径应 ErrBadPath, got %v", err)
	}
}

// 链接父目录里的 Mkdir：在链接里"新建文件夹"，东西必须落进链接指向的
// 真实目录，而不是在链接旁边冒出一个同名目录。
func TestMkdirThroughSymlinkedParent(t *testing.T) {
	tmp := t.TempDir()
	into := filepath.Join(tmp, "真实目录")
	if err := os.Mkdir(into, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "链接目录")
	if err := os.Symlink(into, link); err != nil {
		t.Skipf("本版 Android/Termux 不支持符号链接: %v", err)
	}
	if err := NewService(Options{}).Mkdir(context.Background(), filepath.Join(link, "新目录")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(into, "新目录")); err != nil {
		t.Fatalf("新目录没落进链接指向的真实目录: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tmp, "新目录")); err == nil {
		t.Fatal("在链接旁边多建了一个同名目录")
	}
}

// Rename 最重要的一条：**改的是链接本身，不是它的目标**。
//
// 这是 Stat/Rename/Delete 全族用 Lstat + 末段原样的原因。用 AbsClean 的
// 完整解析结果去做 rename，等价于 mv 把链接指向的文件搬走了 —— 用户的
// 意图是改个链接名，结果服务器上一个正在被别的服务读的文件换了位置。
// D14 下没有任何系统护栏会拦这一下。
func TestRenameSymlinkItself(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "重要文件.db")
	writeFile(t, target, "不能动我")
	link := filepath.Join(tmp, "旧链接名")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("本版 Android/Termux 不支持符号链接: %v", err)
	}
	newLink := filepath.Join(tmp, "新链接名")
	if err := NewService(Options{}).Rename(context.Background(), link, newLink); err != nil {
		t.Fatal(err)
	}
	// 目标原地不动
	if b, err := os.ReadFile(target); err != nil || string(b) != "不能动我" {
		t.Fatalf("链接的目标被动过了: %v %q", err, b)
	}
	// 旧链接没了、新链接仍指向同一个目标
	if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("旧链接还在: %v", err)
	}
	to, err := os.Readlink(newLink)
	if err != nil {
		t.Fatalf("新链接没建出来: %v", err)
	}
	if to != target {
		t.Errorf("新链接指向变了: %q vs %q", to, target)
	}
}

// 目标已存在 → ErrExists，且**在调用 os.Rename 之前**就返回。
//
// Unix 的 rename(2) 默认覆盖目标，这是整条重命名链路上唯一能让用户
// "一个文件无声消失"的地方。断言里必须包含"目标内容未被改动"，因为
// 只断 409 的话，"先覆盖再检查"的实现也能通过 —— 那种实现已经造成了
// 数据丢失，返回值再正确也没用。
func TestRenameRefusesExistingTarget(t *testing.T) {
	ctx := context.Background()
	svc := NewService(Options{})
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.txt")
	dst := filepath.Join(tmp, "dst.txt")
	writeFile(t, src, "源")
	writeFile(t, dst, "目标原有内容")

	if err := svc.Rename(ctx, src, dst); !errors.Is(err, ErrExists) {
		t.Fatalf("应 ErrExists, got %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "目标原有内容" {
		t.Fatal("返回错误之前就把目标覆盖了")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("失败的 rename 把源文件弄没了")
	}

	// 目录也是同样一条：rename(2) 允许把一个空目录搬到已存在的空目录上
	// （成功并删掉源），普通文件则报 EISDIR —— 两种 errno 不同但都必须是
	// 409，绝不能一个成一个败
	d1 := filepath.Join(tmp, "目录A")
	d2 := filepath.Join(tmp, "目录B")
	if err := os.Mkdir(d1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(d2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.Rename(ctx, d1, d2); !errors.Is(err, ErrExists) {
		t.Fatalf("目录撞名也应 ErrExists, got %v", err)
	}
	if _, err := os.Stat(d1); err != nil {
		t.Fatal("空目录 A 被 rename 吞掉了")
	}
}

// 目标父目录不存在 → ErrNotDirectory。
//
// os.Rename 在这里给的是 ENOENT，与"源不存在"同一个 errno：直接透传的话
// 前端只能说"找不到文件"，而源明明在用户眼前 —— 一句自相矛盾的报错比
// 不报错更让人怀疑面板坏了。
func TestRenameMissingTargetDir(t *testing.T) {
	ctx := context.Background()
	svc := NewService(Options{})
	tmp := t.TempDir()
	src := filepath.Join(tmp, "f.txt")
	writeFile(t, src, "x")

	err := svc.Rename(ctx, src, filepath.Join(tmp, "没这个目录", "f.txt"))
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("应 ErrNotDirectory, got %v", err)
	}
	if !strings.Contains(err.Error(), "没这个目录") {
		t.Errorf("错误里必须点明是哪个目录不在: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("失败的 rename 弄没了源文件")
	}

	// 目标父是普通文件：这种输入可能在 cleanOnly→resolveExistingPrefix
	// 阶段就被判成"穿过普通文件"（ErrBadPath），也可能落到上面那条显式
	// 检查的 ErrNotDirectory。两者都是 400、都不该是 500，所以两个都接受：
	// 具体走哪条取决于 EvalSymlinks 在哪一层先撞上 ENOTDIR，那是实现细节，
	// 把它写死成唯一期望只会逼实现多跑一次预检查。
	f := filepath.Join(tmp, "f.txt")
	if err := svc.Rename(ctx, src, filepath.Join(f, "x")); !errors.Is(err, ErrNotDirectory) &&
		!errors.Is(err, ErrBadPath) {
		t.Errorf("目标父是文件应 ErrNotDirectory 或 ErrBadPath, got %v", err)
	}
}

// 原地改名（from == to）→ ErrBadPath。
//
// 让它过也能工作（rename 到自己是 no-op），但那是"看起来成功了"：
// 前端的重命名对话框按原值回车时用户以为改了名。报"路径不合法"反而
// 太含糊，所以措辞里带上"源与目标相同"。
func TestRenameSamePath(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "f.txt")
	writeFile(t, f, "x")
	// 用"中间层是链接、末段相同"的形态测：纯字符串相等是便宜的检查，
	// 解析之后相等才是真正的判据
	link := filepath.Join(tmp, "l.txt")
	if err := os.Symlink(f, link); err == nil {
		err := NewService(Options{}).Rename(context.Background(), link, filepath.Join(tmp, "l.txt"))
		if err != nil && !errors.Is(err, ErrBadPath) && !errors.Is(err, ErrExists) {
			t.Errorf("原地改名应 ErrBadPath（或至少 ErrExists）, got %v", err)
		}
		if err == nil {
			t.Error("原地改名不该静默成功")
		}
	}
	if err := NewService(Options{}).Rename(context.Background(), f, f); !errors.Is(err, ErrBadPath) {
		t.Errorf("完全相同的路径应 ErrBadPath, got %v", err)
	}
}

// 跨目录移动成功 + 失败不留半份。
func TestRenameMoveAcrossDirs(t *testing.T) {
	ctx := context.Background()
	svc := NewService(Options{})
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "子目录")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(tmp, "报告 2026.md")
	writeFile(t, src, "内容")
	if err := svc.Rename(ctx, src, filepath.Join(sub, "报告 2026.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("源还在: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(sub, "报告 2026.md")); err != nil || string(b) != "内容" {
		t.Fatalf("移动后内容不对: %v %q", err, b)
	}
	// 整目录移动也支持（rename(2) 本来就能）。
	// MkdirAll 而不是 Mkdir：中间层未必存在，写死顺序的话报出来的是
	// ENOENT 而不是被测的那条语义
	inner := filepath.Join(sub, "内")
	if err := os.MkdirAll(filepath.Join(inner, "更深"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inner, "更深", "x.txt"), "y")
	if err := svc.Rename(ctx, inner, filepath.Join(tmp, "搬走的")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "搬走的", "更深", "x.txt")); err != nil {
		t.Fatalf("目录树没整体搬过来: %v", err)
	}
}

func TestRenameAndStatRejectBadPaths(t *testing.T) {
	ctx := context.Background()
	svc := NewService(Options{})
	for _, bad := range []string{"relative/x", "", "/a\x00b"} {
		if err := svc.Rename(ctx, bad, "/tmp/x"); !errors.Is(err, ErrBadPath) {
			t.Errorf("rename from=%q 应 ErrBadPath, got %v", bad, err)
		}
		if _, err := svc.Stat(ctx, bad); !errors.Is(err, ErrBadPath) {
			t.Errorf("stat path=%q 应 ErrBadPath, got %v", bad, err)
		}
	}
}

func TestOpsHonorsContext(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	svc := NewService(Options{})
	tmp := t.TempDir()
	if err := svc.Mkdir(cancelled, filepath.Join(tmp, "x")); !errors.Is(err, context.Canceled) {
		t.Errorf("mkdir 应报取消, got %v", err)
	}
	if err := svc.Rename(cancelled, filepath.Join(tmp, "a"), filepath.Join(tmp, "b")); !errors.Is(err, context.Canceled) {
		t.Errorf("rename 应报取消, got %v", err)
	}
	if _, err := svc.Stat(cancelled, tmp); !errors.Is(err, context.Canceled) {
		t.Errorf("stat 应报取消, got %v", err)
	}
	// ctx 取消与路径非法同时发生时，报哪个都行但必须是错误 ——
	// 这条存在是为了防止有人为了"优先级"在这里加吞错的分支
	if err := svc.Mkdir(cancelled, "相对路径"); err == nil {
		t.Error("同时取消 + 非法路径时必须报错")
	}
}

// writeFile 在任意路径写一个文件（父目录按需创建）。
// browse_test.go 里的 mkDir 只能往一个目录里铺名字，而 ops 的测试需要
// 精确控制"某个具体路径存在/是文件/有这些字节"。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
