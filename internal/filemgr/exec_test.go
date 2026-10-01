package filemgr

// M6-T4 执行器：流式复制内核。
//
// 这层是 copy 与 move 的地基（跨盘 move 就是"复制 + 校验 + 删源"），
// 所以先单独钉死：内容一致、取消后不留半成品、进度是累计值、权限/时间戳
// 保留、拒绝覆盖已有目标。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------- 夹具 ----------

// varTempDir 是一个一定可写的临时目录（EvalSymlinks 过，所以拼出来的路径
// 与 os 自己看到的是同一串 —— 本机 /data 上 /tmp 是个符号链接，不解析会
// 让"路径相等"的断言莫名失败）。
func varTempDir(t testing.TB) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// writeFileN 写一个 n 字节的随机文件，回它的 sha256（hex）。
//
// 用随机内容而不是固定文本：随机内容让任何一字节错位、截断、重复都必然
// 反映到哈希上，而重复字符组成的文件有可能"错得看不出来"。
func writeFileN(t testing.TB, path string, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// hashFile 读全文件算 sha256（只在测试里用，实现里绝不这样干）。
func hashFile(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sampleReporter 记录每次上报的累计字节。
type sampleReporter struct {
	samples []int64
	failAt  int   // 第 N 次上报时回错误（0 = 永不）
	err     error // failAt 命中时回的错误
	n       int
}

func (r *sampleReporter) report(doneBytes int64, entries int) error {
	r.n++
	r.samples = append(r.samples, doneBytes)
	if r.failAt > 0 && r.n == r.failAt {
		return r.err
	}
	return nil
}

// ---------- copyStream：流式语义 ----------

// 复制必须是**流式**的：设计 8.4 明写"带缓冲流式复制（1MB buffer）"。
//
// 这条不能靠"读代码看着像"，也不能靠堆内存增量去猜（ReadAll 的 48MB 在
// 测量时可能已被 GC 掉，测不出来）。用一个"能观察下游已写入多少"的读
// 循环直接问流式性本身：源还没读完，目标那边必须已经有数据落下了。
// 整份读入的实现里，第一次写入发生在读完之后 —— 断言立刻失败。
//
// 为什么值得为它单独拆一个 copyStream：整份读入在 3MB 文件上能给出完全
// 正确的哈希（所有正确性测试全绿），然后在用户复制一个 8GB 镜像时把面板
// OOM 掉 —— 而这台机器上还跑着别的服务。
func TestCopyStreamWritesBeforeSourceExhausted(t *testing.T) {
	const total = 8 << 20 // 8MB，若整份读入则这 8MB 会同时活着
	src := &countingReader{left: total}
	dst := &trackingWriter{}
	src.w = dst

	if _, err := copyStream(context.Background(), src, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	if !src.streamed {
		t.Error("copyStream 不是流式的：源读完之前，目标一个字节都没收到")
	}
	if dst.n != total {
		t.Errorf("目标只收到 %d 字节，应为 %d", dst.n, total)
	}
}

type countingReader struct {
	left     int
	streamed bool // 收到过"下游已写入"的信号
	w        *trackingWriter
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	// 问一次下游：已经有数据写进去了吗？若有，说明我们是边读边写。
	if r.w != nil && r.w.n > 0 {
		r.streamed = true
	}
	n := len(p)
	if n > r.left {
		n = r.left
	}
	for i := 0; i < n; i++ {
		p[i] = byte(i)
	}
	r.left -= n
	return n, nil
}

type trackingWriter struct{ n int }

func (w *trackingWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}

// 上报回错误时 copyStream 必须立刻收工（取消就是从这条返回值传回来的）。
//
// 只让执行器自己看 ctx 是不够的：一个只写
// `if err := report(...); err != nil { return err }` 的实现必须在
// report 回错时真的停下，否则取消形同虚设。
func TestCopyStreamStopsWhenReporterErrors(t *testing.T) {
	const total = 5 << 20
	stop := errors.New("别拷了")
	src := &countingReader{left: total}
	dst := &trackingWriter{}
	rep := &sampleReporter{failAt: 2, err: stop}
	if _, err := copyStream(context.Background(), src, dst, rep.report); !errors.Is(err, stop) {
		t.Fatalf("该把上报的错误原样传回, got %v", err)
	}
	if dst.n >= total {
		t.Errorf("第 2 次上报就该停下，却写完了全部 %d 字节", dst.n)
	}
}

// 上下文已取消 → 立刻回 ctx.Err()，一次都不读。
func TestCopyStreamHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &countingReader{left: 1 << 20}
	dst := &trackingWriter{}
	if _, err := copyStream(ctx, src, dst, nopProgress); !errors.Is(err, context.Canceled) {
		t.Fatalf("应 context.Canceled, got %v", err)
	}
	if dst.n != 0 {
		t.Errorf("已取消的上下文不该写任何东西, got %d", dst.n)
	}
}

// ---------- copyFile：内容一致 ----------

// 复制前后内容必须逐字节相同（设计验收⑥：复制 10GB 关浏览器照常完成，
// 前提首先是它真的复制对了）。
func TestCopyFileContentIdentical(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	want := writeFileN(t, src, 3<<20) // 跨过 1MB 缓冲，保证走多块路径
	dst := filepath.Join(dir, "dst.bin")

	s := NewService(Options{})
	n, err := s.copyFile(context.Background(), src, dst, nopProgress)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3<<20 {
		t.Errorf("该回已复制字节数 3MB, got %d", n)
	}
	if got := hashFile(t, dst); got != want {
		t.Errorf("复制后内容不一致\n want %s\n  got %s", want, got)
	}
}

// 空文件也要能复制（0 字节的分支最容易与"读失败"混在一起）。
func TestCopyFileEmpty(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "empty.copy")
	s := NewService(Options{})
	n, err := s.copyFile(context.Background(), src, dst, nopProgress)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("空文件该回 0 字节, got %d", n)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("空文件的副本必须存在: %v", err)
	}
}

// ---------- 元信息 ----------

// 权限位必须带过去。
//
// 不保留权限的话，复制一个 0600 私钥会得到默认权限 —— 同机其它账号忽然
// 能读了。这是复制功能**自己**就能造成的安全事故，不能指望用户事后 chmod。
func TestCopyFilePreservesMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(src, []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o600); err != nil { // WriteFile 的 mode 会被 umask 削
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "id_ed25519.copy")
	s := NewService(Options{})
	if _, err := s.copyFile(context.Background(), src, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	have, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if have.Mode().Perm() != 0o600 {
		t.Errorf("权限该保留 0600, got %o", have.Mode().Perm())
	}
}

// 修改时间必须带过去。
//
// 界面里"按时间排序"读的就是 mtime，而"复制一份备份"是复制最常见的用途：
// 复制完一堆备份全变成"刚刚"，用户就没法判断哪个是新的了。
func TestCopyFilePreservesMtime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "backup.tar")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "backup.copy")
	s := NewService(Options{})
	if _, err := s.copyFile(context.Background(), src, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// 文件系统可能只把 mtime 存到秒级，比到秒即可。
	if !fi.ModTime().Truncate(time.Second).Equal(old.Truncate(time.Second)) {
		t.Errorf("mtime 该保留 %v, got %v", old, fi.ModTime())
	}
}

// ---------- 拒绝覆盖 ----------

// 目标已存在必须拒绝（ErrExists）。
//
// os 层只提供"覆盖"这一种行为，而全盘 root（D14）下没有任何系统护栏：
// 默认覆盖等于复制会**吃掉另一个文件**，而用户点的只是"复制粘贴"。同名
// 护栏在 Rename 里已经有一条，这是它在复制侧的对应物；409 让界面能问
// 用户"改名还是先删"。
func TestCopyFileRefusesExistingDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	if err := os.WriteFile(src, []byte("新内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "b")
	if err := os.WriteFile(dst, []byte("别人的重要文件"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewService(Options{})
	_, err := s.copyFile(context.Background(), src, dst, nopProgress)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("应 ErrExists（HTTP 层据此回 409）, got %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "别人的重要文件" {
		t.Errorf("拒绝必须是什么都没做：目标被改写成 %q", got)
	}
}

// 源不存在 → fs.ErrNotExist（404），而不是"生成一个空副本"。
func TestCopyFileMissingSource(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "x")
	s := NewService(Options{})
	_, err := s.copyFile(context.Background(), filepath.Join(dir, "没了"), dst, nopProgress)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("应 fs.ErrNotExist, got %v", err)
	}
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Error("源不存在时不该留下目标文件")
	}
}

// 目标是目录（不是文件）→ ErrExists，不是 ErrNotDirectory 也不是崩。
func TestCopyFileDstIsDir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "已有目录")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewService(Options{})
	if _, err := s.copyFile(context.Background(), src, dst, nopProgress); !errors.Is(err, ErrExists) {
		t.Fatalf("目标是已存在的目录，该 ErrExists, got %v", err)
	}
}

// 目标的父目录不存在 → 明确失败，且**不留下半个目录或文件**。
//
// 复制不该顺手创建目标的父目录：那是 copyTree（复制目录树）的职责。单层
// 复制里悄悄建目录，会把"目标路径写错了"变成"在奇怪的地方冒出一堆目录"。
func TestCopyFileMissingDstParent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewService(Options{})
	_, err := s.copyFile(context.Background(), src, filepath.Join(dir, "没有这个目录", "a"), nopProgress)
	if err == nil {
		t.Fatal("父目录不存在该失败")
	}
	if _, lerr := os.Lstat(filepath.Join(dir, "没有这个目录")); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("不该创建目标的父目录: %v", lerr)
	}
}

// ---------- 取消：清理半成品 ----------

// 取消后必须**不留任何残骸**，并报 context.Canceled。
//
// 设计 8.4 明写"worker 在块边界检查并清理半成品"。不清理的后果不是"多个
// 垃圾文件"这么轻：一个 30% 大小的 zip 或 tar 看起来是个完整文件，用户
// 下次拿它去解压才发现是坏的 —— 比"明确没有这个文件"危险得多。
func TestCopyFileCancelRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	writeFileN(t, src, 6<<20) // 6MB / 1MB 缓冲：保证能在中途被取消
	dst := filepath.Join(dir, "big.copy")

	ctx, cancel := context.WithCancel(context.Background())
	first := true
	rep := func(doneBytes int64, entries int) error {
		if first {
			first = false
			cancel() // 第一块拷完就取消
		}
		return nil
	}
	s := NewService(Options{})
	_, err := s.copyFile(ctx, src, dst, rep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消该报 context.Canceled, got %v", err)
	}
	if _, lerr := os.Lstat(dst); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("取消后必须删掉半成品, got stat=%v", lerr)
	}
	// 全心全意：源必须完好无损。取消复制不该动源。
	if _, err := os.Stat(src); err != nil {
		t.Errorf("源被动过: %v", err)
	}
}

// 上报回错误也要走同一条清理路径（两个入口共用一次 Remove）。
func TestCopyFileReporterErrorRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	writeFileN(t, src, 4<<20)
	dst := filepath.Join(dir, "big.copy")
	rep := &sampleReporter{failAt: 1, err: errors.New("别拷了")}
	s := NewService(Options{})
	if _, err := s.copyFile(context.Background(), src, dst, rep.report); err == nil {
		t.Fatal("上报回错误时该报错")
	}
	if _, lerr := os.Lstat(dst); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("出错后必须清掉半成品, got stat=%v", lerr)
	}
}

// ---------- 符号链接 ----------

// 复制一个符号链接要**复制链接本身**，不能跟进去。
//
// 跟进去有两个恶果：/data 那种指向大目录的链接会被整份展开（用户复制
// 100MB 实际拷了 50GB）；而日志目录里常见的自指/环状链接会让遍历永不结束。
// 整个文件管理器的语义都是"操作链接本身"（ls -l 显示链接、mv 改链接、
// rm 删链接），复制不能是唯一的例外。
func TestCopyFileOnSymlinkCopiesLinkItself(t *testing.T) {
	dir := varTempDir(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "link.copy")
	s := NewService(Options{})
	if _, err := s.copyFile(context.Background(), link, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("该复制成符号链接, got mode=%v", fi.Mode())
	}
	to, err := os.Readlink(dst)
	if err != nil {
		t.Fatal(err)
	}
	if to != target {
		t.Errorf("链接指向变了: %q → %q", target, to)
	}
	// 目标本体必须还是原样、只有一份
	if got, _ := os.ReadFile(target); string(got) != "内容" {
		t.Errorf("目标被改写: %q", got)
	}
}

// 悬空链接也要能复制成链接（指向不存在的东西是链接的正常状态）。
//
// 复制一份"以后会存在"的链接是常见做法（构建产物、挂载点占位）。跟随
// 目标的实现在这里会直接 404，用户既复制不了也没得到解释。
func TestCopyFileDanglingSymlink(t *testing.T) {
	dir := varTempDir(t)
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "永远不存在"), link); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dangling.copy")
	s := NewService(Options{})
	if _, err := s.copyFile(context.Background(), link, dst, nopProgress); err != nil {
		t.Fatalf("悬空链接该能复制: %v", err)
	}
	if _, err := os.Readlink(dst); err != nil {
		t.Errorf("副本该是符号链接: %v", err)
	}
}

// ---------- 进度 ----------

// 上报必须是**累计**字节数且单调不减。
//
// 界面用它画进度条、算剩余时间，还用两次样本之差算速度。报"本块大小"会
// 让进度条来回跳、速度算出负数 —— 一个看起来在动的错数字比一个静止
// 的数字更难被发现。
func TestCopyFileReportsCumulativeBytes(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	const size = 5 << 20
	writeFileN(t, src, size)
	dst := filepath.Join(dir, "dst.bin")

	rep := &sampleReporter{}
	s := NewService(Options{})
	total, err := s.copyFile(context.Background(), src, dst, rep.report)
	if err != nil {
		t.Fatal(err)
	}
	if total != size {
		t.Errorf("总字节数该是 %d, got %d", size, total)
	}
	if len(rep.samples) == 0 {
		t.Fatal("一次都没上报：进度条会永远停在 0")
	}
	if last := rep.samples[len(rep.samples)-1]; last != total {
		t.Errorf("最后一次上报该等于总字节数: last=%d total=%d", last, total)
	}
	for i := 1; i < len(rep.samples); i++ {
		if rep.samples[i] < rep.samples[i-1] {
			t.Fatalf("进度不是单调不减: %v", rep.samples)
		}
	}
	if len(rep.samples) < 2 {
		t.Errorf("5MB 只在进度上打了 %d 个点，用户看不到它在动", len(rep.samples))
	}
}

// ---------- 校验 ----------

// 内容相同 → 一致。跨盘 move 要靠它决定"能不能删源"。
func TestVerifyFilesMatch(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.bin")
	writeFileN(t, a, 1<<20)
	b := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(b, mustRead(t, a), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewService(Options{})
	ok, err := s.verifyFilesMatch(context.Background(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("内容相同的两个文件应判为一致")
	}
}

// 大小相同而内容不同 → 必须判为不一致。
//
// 这是校验的全部意义所在：只比大小的实现（os.Stat 两下）能过掉上面那条
// 测试，然后在跨盘 move 里把一个内容错乱、长度恰好相同的副本当成成功，
// 接着删掉源 —— 那是数据丢失，而且是不可逆的。sha256 是这里的底线。
func TestVerifyFilesMatchSameSizeDifferentContent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, bytes.Repeat([]byte("A"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, bytes.Repeat([]byte("B"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewService(Options{})
	ok, err := s.verifyFilesMatch(context.Background(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("大小相同而内容不同：不能判为一致")
	}
}

// 大小不同 → 不一致（而且不必读内容，省下整轮磁盘擦写）。
func TestVerifyFilesMatchDifferentSize(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, bytes.Repeat([]byte("A"), 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, bytes.Repeat([]byte("A"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewService(Options{})
	ok, err := s.verifyFilesMatch(context.Background(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("大小不同不该判为一致")
	}
}

// 校验也要能被取消：sha256 一个 10GB 文件要几分钟，用户在这期间点了取消，
// 面板不该把剩下的时间继续花在"确认一份马上要被丢掉的文件"上。
func TestVerifyFilesMatchHonorsContext(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	writeFileN(t, a, 4<<20)
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(b, mustRead(t, a), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewService(Options{})
	if _, err := s.verifyFilesMatch(ctx, a, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("应 context.Canceled, got %v", err)
	}
}

// ---------- 夹具辅助 ----------

func mustRead(t testing.TB, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
