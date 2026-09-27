package filemgr

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Open 的契约核心不是"能读出字节"，而是头部三元组的来源：
// Size/ModTime 必须与将要流出去的内容同源，否则浏览器拿到
// Content-Length: 1000 却只收到 900 字节，表现为"下载永远失败"
// 或"文件被截断"，而服务端日志里一个错误都没有。

func TestOpenReadsContent(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	p := filepath.Join(dir, "报告 2026.txt")
	writeFile(t, p, "中文内容 abc")

	o, err := svc.Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer o.File.Close()

	if o.Name != "报告 2026.txt" {
		t.Errorf("Name = %q", o.Name)
	}
	b, err := io.ReadAll(o.File)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "中文内容 abc" {
		t.Errorf("内容 = %q", b)
	}
	// Size 必须与实际可读字节数一致。**用字节而不是 rune**：Content-Length
	// 说的是字节，而"中文"两个字在 UTF-8 里是 6 字节 —— 如果 Size 哪天
	// 被换成字符数，头部就会小一半，浏览器报"下载不完整"。
	if int64(len(b)) != o.Size {
		t.Errorf("Size=%d 与实际字节 %d 不符", o.Size, len(b))
	}
	if o.ModTime.IsZero() {
		t.Error("ModTime 为空：Last-Modified 头就填不出来")
	}
}

func TestOpenFollowsSymlink(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	target := filepath.Join(dir, "真身.txt")
	writeFile(t, target, "真内容")
	link := filepath.Join(dir, "链接.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("平台不支持符号链接: %v", err)
	}

	o, err := svc.Open(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	defer o.File.Close()

	b, _ := io.ReadAll(o.File)
	// 下载符号链接给出**目标内容**，不是链接自己那 3 个字节的目标路径字符串。
	// 与 Rename 的选择相反是对的：改名改的是对象本身，下载要的是内容。
	if string(b) != "真内容" {
		t.Errorf("应读到目标内容, got %q", b)
	}
	// Name 取链接名而不是目标名：另存为对话框里用户认得的名字是他点的那个
	if o.Name != "链接.txt" {
		t.Errorf("Name 应为链接名, got %q", o.Name)
	}
	if o.Size != int64(len("真内容")) {
		t.Errorf("Size 应为目标字节数, got %d", o.Size)
	}
}

func TestOpenRejectsDirectory(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	sub := filepath.Join(dir, "子目录")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Open(context.Background(), sub)
	// 必须是 ErrIsDirectory，不能落到 fs.ErrNotExist 或 ErrBadPath：
	// API 层靠它回 400"目录请打包下载"，回成 404 的话用户以为目录被删了
	if !errors.Is(err, ErrIsDirectory) {
		t.Fatalf("应 ErrIsDirectory, got %v", err)
	}
}

func TestOpenMissingAndRelative(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	if _, err := svc.Open(context.Background(), filepath.Join(dir, "没有.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("缺失文件应 fs.ErrNotExist, got %v", err)
	}
	if _, err := svc.Open(context.Background(), "相对.txt"); !errors.Is(err, ErrBadPath) {
		t.Errorf("相对路径应 ErrBadPath, got %v", err)
	}
}

func TestOpenCanceledContext(t *testing.T) {
	svc := NewService(Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 取消必须立刻返回。下载大文件时用户点"停止"，靠的就是这一层；
	// 若只在 Select 层检查，取消的下载会继续把整个文件读进网络缓冲区。
	if _, err := svc.Open(ctx, "/etc/hosts"); !errors.Is(err, context.Canceled) {
		t.Errorf("取消的 ctx 应报错, got %v", err)
	}
}

// 下载一个命名奇怪的文件时，路径里的分隔符不能被名字本身骗过去：
// Name 必须是单个路径段，否则它会被拼进 Content-Disposition，
// 一个含引号/换行的文件名就能注入响应头。
func TestOpenNameIsSingleSegment(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	// 文件名里带空格与 Unicode；换行和引号在多数文件系统上合法但极少见，
	// 这里能测的先测：Name 里绝不能再有 '/'
	p := filepath.Join(dir, "a b ico中.txt")
	writeFile(t, p, "x")
	o, err := svc.Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer o.File.Close()
	if strings.ContainsRune(o.Name, os.PathSeparator) {
		t.Errorf("Name 含路径分隔符: %q", o.Name)
	}
}
