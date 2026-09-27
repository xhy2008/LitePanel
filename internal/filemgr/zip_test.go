package filemgr

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zip 打包的验收核心是**条目名**：内容对了但名字错了，解压出来照样是
// 一堆互相覆盖的文件（三个 backup.zip 变一个）。

func zipNames(t *testing.T, data []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytesReader(data), int64(len(data)))
	if err != nil {
		// 中央目录损坏最常见的成因是"流在中途报错断掉了"
		t.Fatalf("zip 读不回: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func bytesReader(b []byte) io.ReaderAt {
	return bytes.NewReader(b)
}

func zipContent(t *testing.T, data []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytesReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Fatalf("zip 里没有 %q", name)
	return ""
}

func TestZipFilesAndDirs(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f1.txt"), "一")
	writeFile(t, filepath.Join(dir, "f2 名.txt"), "二")
	writeFile(t, filepath.Join(dir, "子目录", "内层", "深层.txt"), "三")
	// 链接必须放在**被打包的那棵树里面**：放在 dir 根上而只打包
	// f1/子目录 的话，跳过链接那条分支根本没被执行，测试却在断言
	// "链接不在包里" —— 一个恒真的断言比没有断言更糟，它看起来像覆盖。
	// zip 条目没有可靠的链接语义：NTFS 会把它变成普通文件（等于把
	// 稀疏目标整份复制），某些解压端会照着相对链接往解压目录外写
	// （zip slip 的入口）。跳过是唯一两头都安全的选。
	link := filepath.Join(dir, "子目录", "链到外面")
	if err := os.Symlink("/etc/hosts", link); err != nil {
		t.Skipf("平台不支持符号链接: %v", err)
	}

	var buf strings.Builder
	if err := svc.Zip(context.Background(), []string{
		filepath.Join(dir, "f1.txt"),
		filepath.Join(dir, "f2 名.txt"),
		filepath.Join(dir, "子目录"),
	}, &buf); err != nil {
		t.Fatal(err)
	}
	data := []byte(buf.String())

	got := strings.Join(zipNames(t, data), "|")
	for _, must := range []string{"f1.txt", "f2 名.txt", "子目录/内层/深层.txt"} {
		if !strings.Contains(got, must) {
			t.Errorf("zip 缺条目 %q, got %s", must, got)
		}
	}
	if strings.Contains(got, "链到外面") {
		t.Errorf("符号链接不应进 zip: %s", got)
	}
	if s := zipContent(t, data, "f1.txt"); s != "一" {
		t.Errorf("f1 内容 = %q", s)
	}
	if s := zipContent(t, data, "子目录/内层/深层.txt"); s != "三" {
		t.Errorf("深层内容 = %q", s)
	}
	// hdr.Name 里绝不能有服务器的绝对路径痕迹：包内路径与磁盘路径是两件事
	for _, n := range zipNames(t, data) {
		if strings.HasPrefix(n, "/") || strings.Contains(n, "..") {
			t.Errorf("条目名不安全: %q", n)
		}
	}
	// 目录条目必须以 / 结尾 —— 这是 zip 里"这是个目录"的唯一通用表示法
	// （外部属性字段既非通用也非强制）。漏掉斜杠的话部分解压端会把它
	// 当成一个 0 字节的普通文件："名为目录的空文件"比没有更让人迷惑。
	var dirEntry bool
	for _, n := range zipNames(t, data) {
		if strings.HasSuffix(n, "/") {
			dirEntry = true
		}
	}
	if !dirEntry {
		t.Errorf("目录条目必须以 / 结尾, got %s", strings.Join(zipNames(t, data), "|"))
	}
}

// 打包中途写失败必须报错，而不是交付一个坏 zip。
//
// 这是唯一能测到"收尾失败"这条路径的方式：archive/zip 的中央目录在
// Close() 里写，让 writer 在若干字节后开始失败，就能让前面的文件都写
// 成功、只有收尾失败。吞掉那个错误的话，客户端拿到的是一个下载"成功"
// 却解不开的包，而服务端日志里一个字都没有。
func TestZipWriteFailurePropagates(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "d", "a.txt"), strings.Repeat("A", 4096))
	writeFile(t, filepath.Join(dir, "d", "b.txt"), strings.Repeat("B", 4096))

	// limit=0，也就是**第一次落盘就失败**。不能写成"前 N 字节放过"：
	// archive/zip 自带 64KB 缓冲，条目又小，前面那些 Write 全部命中缓冲、
	// 一次都没到底层 —— limit=600 写出来是个恒不触发的条件（我第一版
	// 就是这么写的，测试假绿）。
	w := &failWriter{}
	err := svc.Zip(context.Background(), []string{filepath.Join(dir, "d")}, w)
	if err == nil {
		t.Fatalf("写失败必须报错（坏 zip 不能当成功交付）；已写出 %d 字节", w.n)
	}
	// 钉住是**收尾**失败：夹具里全部条目加起来远小于 archive/zip 的 64KB
	// 缓冲，所以中途的 Write 全都命中缓冲、必不失败，错误只可能来自
	// Close 写中央目录那一步。不钉这条的话，哪天有人把夹具文件变大，
	// 这个测试会悄悄变成测"中途写失败"，Close 分支又没人看着了。
	if !strings.Contains(err.Error(), "收尾") {
		t.Errorf("应命中收尾(Close)分支, got %v", err)
	}
}

// failWriter 是个一写就坏的下游（模拟磁盘满、连接被切断、对端关闭）。
type failWriter struct{ n int }

func (w *failWriter) Write(p []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

// 目录条目必须**保留顶层目录名**。压掉一层的话，把 "备份/" 打包下载后
// 解压"到当前目录"会把整棵树直接倒进用户目录 —— 这是典型的"解压前
// 看一眼还好，手快就完蛋"的事故形状。
func TestZipKeepsTopLevelDir(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "备份", "a.txt"), "a")

	var buf strings.Builder
	if err := svc.Zip(context.Background(), []string{filepath.Join(dir, "备份")}, &buf); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(zipNames(t, []byte(buf.String())), "|")
	if !strings.Contains(got, "备份/a.txt") {
		t.Errorf("顶层目录名必须保留: %s", got)
	}
}

// 不同目录下的同名文件：zip 规范允许重名条目，但读端只会留下最后一个
// —— 静默丢文件。后缀 " (1)" 与上传冲突策略同形（8.2），用户在那里
// 见过的规则在这里也认得。
func TestZipCollisionSuffix(t *testing.T) {
	svc := NewService(Options{})
	a := t.TempDir()
	b := t.TempDir()
	writeFile(t, filepath.Join(a, "backup.zip"), "来自A")
	writeFile(t, filepath.Join(b, "backup.zip"), "来自B")

	var buf strings.Builder
	if err := svc.Zip(context.Background(), []string{
		filepath.Join(a, "backup.zip"),
		filepath.Join(b, "backup.zip"),
	}, &buf); err != nil {
		t.Fatal(err)
	}
	data := []byte(buf.String())
	names := strings.Join(zipNames(t, data), "|")
	if !strings.Contains(names, "backup.zip") || !strings.Contains(names, "backup (1).zip") {
		t.Errorf("重名应加后缀而不是互相覆盖: %s", names)
	}
	// 内容必须与名字对得上：后缀只改名字不改配对的话测试抓不到
	if s := zipContent(t, data, "backup.zip"); s != "来自A" {
		t.Errorf("先出现的应保原名, got %q", s)
	}
	if s := zipContent(t, data, "backup (1).zip"); s != "来自B" {
		t.Errorf("backup (1) 内容 = %q, 应为来自B", s)
	}
}

func TestZipErrors(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	ctx := context.Background()
	if err := svc.Zip(ctx, nil, io.Discard); !errors.Is(err, ErrBadPath) {
		t.Errorf("空选择应 ErrBadPath, got %v", err)
	}
	if err := svc.Zip(ctx, []string{"相对"}, io.Discard); !errors.Is(err, ErrBadPath) {
		t.Errorf("相对路径应 ErrBadPath, got %v", err)
	}
	if err := svc.Zip(ctx, []string{filepath.Join(dir, "没有")}, io.Discard); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("缺失顶层应 fs.ErrNotExist, got %v", err)
	}
	// 顶层之外也要能报：一个 40GB 的目录在打包到一半时撞上 EIO，
	// 客户端拿到的是坏 zip（下载管理器判失败），这比悄悄少一个文件好
}

func TestZipCanceledContext(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "d", "a.txt"), "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := svc.Zip(ctx, []string{filepath.Join(dir, "d")}, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("取消应中断打包, got %v", err)
	}
}

// 两条方向相反的规则：读不了的**子目录**跳过、读不了的**文件**必须报错。
//
// 为什么相反：多选打包常见于"整个 /var/log"这类目标，里面必然有 root
// 才读得动的子目录 —— 为一个权限盲区让整个包失败，用户什么也拿不到；
// 而文件是用户明确勾中的内容，悄悄少一个文件的包看起来是成功的。
//
// 两条规则各自都只在"另一半被破坏"时才可见，所以必须同一夹具里
// 两种都放：只测跳过的话，把 addFile 也改成 return nil 没人发现，
// 而那正是最危险的静默丢数据。
func TestZipUnreadableDirSkipsButUnreadableFileFails(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	lockedDir := filepath.Join(dir, "d", "root专用")
	if err := os.MkdirAll(lockedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(lockedDir, "秘密.txt"), "看不见")
	lockedFile := filepath.Join(dir, "d", "另一个私有文件.txt")
	writeFile(t, lockedFile, "内容")

	// 权限夹具必须真的生效：以 root 跑测试时 0o000 照样读得到，
	// 那种情况下这个测试会给出假绿（本机 uid 非 0，正常）。
	if err := os.Chmod(lockedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockedFile, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 不还权限的话 t.TempDir 的清理会失败，报出来是一堆无关的
		// "directory not empty"，看着像 Zip 的问题
		os.Chmod(lockedDir, 0o700)
		os.Chmod(lockedFile, 0o644)
	})
	if _, err := os.ReadDir(lockedDir); err == nil {
		t.Skip("当前用户读得了 0o000 目录（以 root 跑测试），夹具无效")
	}

	var buf strings.Builder
	err := svc.Zip(context.Background(), []string{filepath.Join(dir, "d")}, &buf)
	// 文件读不了 → 必须报错
	if err == nil {
		t.Fatal("勾中的文件读不了必须报错，不能悄悄少一个文件")
	}
	if !strings.Contains(err.Error(), "另一个私有文件") {
		t.Errorf("错误里要指名是哪个文件读不了: %v", err)
	}
}

// 只含"读不了的子目录"的树：打包必须**成功**（跳过），但少掉的那一层
// 必须留下日志。
//
// 这条路径返回 nil，也就是说除了那一行 logx，没有任何地方知道少了一棵
// 树。上一版测试只断言"成功且没包含秘密文件"，把 logx.Info 整行删掉
// 也照样绿 —— 断言了一个看不见的东西。日志可见性单独用 debug 构建的
// zip_log_test.go 把守（release 构建里 logx 编译为空操作，那是 D9 的
// 设计，不是这里的漏测）。
func TestZipSkipsUnreadableSubDir(t *testing.T) {
	svc := NewService(Options{})
	dir := t.TempDir()
	locked := filepath.Join(dir, "d", "root专用")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(locked, "秘密.txt"), "看不见")
	writeFile(t, filepath.Join(dir, "d", "可读.txt"), "看得见")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("以 root 跑测试，0o000 夹具无效")
	}

	var buf strings.Builder
	if err := svc.Zip(context.Background(), []string{filepath.Join(dir, "d")}, &buf); err != nil {
		t.Fatalf("读不了的子目录应跳过而不是让整个包失败: %v", err)
	}
	got := strings.Join(zipNames(t, []byte(buf.String())), "|")
	if strings.Contains(got, "秘密") {
		t.Errorf("盲区内容不该在包里: %s", got)
	}
	// 可读的那部分必须完整：跳过一条分支不该顺手吃掉兄弟节点
	if !strings.Contains(got, "可读.txt") {
		t.Errorf("兄弟节点被吃掉了: %s", got)
	}
	// 注意**没有** root专用/ 这个目录条目：读不出内容就不写目录条目，
	// 解压端于是不会留下一个建不出内容的空目录（那比没有更让人以为数据在）
	if strings.Contains(got, "root专用") {
		t.Errorf("读不了的目录不该留下空条目: %s", got)
	}
}
