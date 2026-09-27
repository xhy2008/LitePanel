package filemgr

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 路径清洗（设计 8.1 + D14）。
//
// D14 明确**不做根目录限制**：面板以 root 全盘访问是需求本身，不是漏洞。
// 所以这个包里的函数只负责两件事：把用户给的字符串变成一个确定的绝对路径、
// 以及把"根本不是一个路径"的输入挡在外面。任何"这个路径不该被访问"的判断
// 都不在这里做 —— 做了就是偷偷加了个围栏，而用户会以为那是个 bug。

func TestAbsCleanAbsolute(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/DISK/a/b", "/DISK/a/b"},
		{"/DISK/a//b", "/DISK/a/b"},                // 多余斜杠
		{"/DISK/a/./b", "/DISK/a/b"},               // 单点
		{"/DISK/a/b/../c", "/DISK/a/c"},            // 上跳在清洗阶段就消掉
		{"/DISK/a/b/", "/DISK/a/b"},                // 尾斜杠
		{"/", "/"},                                 // 根目录本身是合法路径
		{"/DISK/中文 目录/x.txt", "/DISK/中文 目录/x.txt"}, // 中文与空格原样保留
		// 上跳冲出挂载点：Clean 在**绝对路径**上会把越过根的部分折叠掉，
		// 两种写法结果都是 /b。这不是围栏（D14 不做限制）：真要访问根，
		// 直接写 "/" 就行。折叠只是 filepath 的既定语义，测试把它钉下来
		// 是为了将来换实现时不会悄悄变成 "/DISK/../b" 这种半截形态。
		{"/DISK/a/../../b", "/b"},
		{"/DISK/a/../../../b", "/b"},
	}
	for _, c := range cases {
		got, err := AbsClean(c.in)
		if err != nil {
			t.Errorf("AbsClean(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("AbsClean(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// 相对路径必须拒绝，而不是按进程 cwd 解释。
//
// 按 cwd 解释的后果：同一个请求参数在开发机（cwd=仓库）和目标机
// （cwd=/）指向完全不同的目录，前端显示的列表和实际操作的目录能对不上。
// 地址栏里用户写的是"我要这个路径"，不是"相对你此刻在哪"。
func TestAbsCleanRejectsRelative(t *testing.T) {
	for _, in := range []string{"a/b", "./a", "../a", "DISK/a", "相对目录"} {
		if _, err := AbsClean(in); !errors.Is(err, ErrBadPath) {
			t.Errorf("相对路径 %q 必须报 ErrBadPath, got %v", in, err)
		}
	}
}

// 空串同相对路径：没有"当前目录"这个默认值可以给。
// 上层（API）要显式决定默认目录（可配置的起始目录），不能在这里猜。
func TestAbsCleanRejectsEmpty(t *testing.T) {
	if _, err := AbsClean(""); !errors.Is(err, ErrBadPath) {
		t.Fatalf("空路径应报 ErrBadPath, got %v", err)
	}
}

// 含 NUL 字节的路径：filepath 层面看是合法字符串，但任何一个 os.Open 都会
// 报 "invalid argument"。让它在这里以"路径不对"的形式失败，而不是变成
// 一个看不懂的 syscall 错误 —— 这类输入通常来自构造的请求，报错文案
// 是给人看的。
func TestAbsCleanRejectsNUL(t *testing.T) {
	if _, err := AbsClean("/DISK/a\x00b"); !errors.Is(err, ErrBadPath) {
		t.Fatalf("应报 ErrBadPath, got %v", err)
	}
}

// 符号链接要解析到真实路径（设计 8.1：符号链接解析后的合法性检查）。
// 目的不是圈禁，而是让"重命名/删除"作用在用户眼睛看到的那个东西上：
// 前端列出的是链接指向的目录内容，操作的对象也必须是它。
func TestAbsCleanResolvesSymlink(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("本机不支持符号链接: %v", err)
	}
	got, err := AbsClean(link)
	if err != nil {
		t.Fatal(err)
	}
	// tmpdir 本身可能是符号链接（macOS /var -> /private/var），比解析后的结果
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("符号链接应解析到目标: got %q want %q", got, want)
	}
}

// 末段不存在时必须保持原样返回：这是 mkdir/rename 的正常输入。
// 用 EvalSymlinks 一步到位会在这里报错，于是"新建文件"永远做不了。
// 只解析存在的前缀，是这条与上一条能同时成立的做法。
func TestAbsCleanKeepsMissingTail(t *testing.T) {
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := AbsClean(filepath.Join(tmp, "sub", "新文件.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "sub", "新文件.txt")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 符号链接成环：EvalSymlinks 会报 ELOOP。必须转成 ErrBadPath，
// 不能让它冒到 API 层变成 500 —— 环是用户能自己造出来的状态
// （ln -s a a），而且是可以解释的输入错误。
func TestAbsCleanSymlinkLoop(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	if err := os.Symlink(filepath.Join(tmp, "b"), a); err != nil {
		t.Skipf("本机不支持符号链接: %v", err)
	}
	if err := os.Symlink(a, filepath.Join(tmp, "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := AbsClean(a); !errors.Is(err, ErrBadPath) {
		t.Fatalf("符号链接环应报 ErrBadPath, got %v", err)
	}
}

// 错误信息里要带上原始输入。不带的话，用户在地址栏粘了一个奇怪的路径
// 之后收到的"路径不合法"完全无法定位 —— 他不知道面板把它理解成了什么。
func TestAbsCleanErrorCarriesInput(t *testing.T) {
	_, err := AbsClean("乱来的路径")
	if err == nil || !strings.Contains(err.Error(), "乱来的路径") {
		t.Fatalf("错误里必须含原始输入, got %v", err)
	}
}

// 穿过一个普通文件的路径（ENOTDIR）。和符号链接环归成一类：都不是
// "能用的路径"。这一条存在的意义是把实现从"只处理 ELOOP"逼出来 ——
// 否则 EvalSymlinks 的其他失败会漏到上层变成 500。
func TestAbsCleanPathThroughFile(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AbsClean(filepath.Join(f, "sub")); !errors.Is(err, ErrBadPath) {
		t.Fatalf("穿过普通文件应报 ErrBadPath, got %v", err)
	}
}
