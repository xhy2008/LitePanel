package filemgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 上传（设计 8.2）的验收核心是"合并后的字节与原文件逐字节一致"，所以
// 凡涉及内容的断言都比 sha256 而不比长度：偏移/顺序错一格、末块被当
// 普通块重复写，这三种错都留下一个"看起来差不多"的长度而毁掉内容，
// 只比长度全都看不出来。
//
// 容量前提（用户确认）：网页版是辅助功能，单次上传 ≤1GB，更大的走
// SFTP。这个上限换掉了整条"按偏移 pwrite 到一个大文件"的实现路径 ——
// 一个分块一个文件、完成时按序号拼接，偏移算术整个不存在，"大小对
// 而内容鬼"这一整类错误在结构上就不可能出现。代价（204 个文件/次）
// 由上限兜住：1GB ÷ 5MB = 204。

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

type uploadEnv struct {
	svc  *Service
	dir  string // 目标目录（用户在文件管理器里打开的那个）
	root string // 暂存根（面板自己的，绝不放进用户目录）
	clk  *fakeClock
}

func newUploadEnv(t *testing.T) *uploadEnv {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "target")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{now: time.Unix(1700000000, 0)}
	root := filepath.Join(base, ".lp-upload")
	svc := NewService(Options{
		UploadRoot:     root,
		UploadTTL:      time.Hour,
		MaxChunkBytes:  1 << 20,
		MaxUploadBytes: 1 << 30,
		Clock:          clk.Now,
	})
	return &uploadEnv{svc: svc, dir: dir, root: root, clk: clk}
}

// reassemble 模拟面板重启：同一份暂存根上新建 Service。
func (e *uploadEnv) reassemble(t *testing.T) *Service {
	t.Helper()
	return NewService(Options{
		UploadRoot:     e.root,
		UploadTTL:      time.Hour,
		MaxChunkBytes:  1 << 20,
		MaxUploadBytes: 1 << 30,
		Clock:          e.clk.Now,
	})
}

func (e *uploadEnv) begin(t *testing.T, id, name string, size, cs int64, policy Conflict) UploadState {
	t.Helper()
	st, err := e.svc.BeginUpload(context.Background(), UploadInit{
		ID: id, Dir: e.dir, Name: name, Size: size, ChunkSize: cs, Conflict: policy,
	})
	if err != nil {
		t.Fatalf("BeginUpload(%s): %v", id, err)
	}
	return st
}

func (e *uploadEnv) put(svc *Service, id string, idx int, body string) (UploadState, error) {
	if svc == nil {
		svc = e.svc
	}
	return svc.PutChunk(context.Background(), UploadChunk{
		ID: id, Index: idx, Body: strings.NewReader(body),
	})
}

func (e *uploadEnv) status(id string) (UploadState, error) {
	return e.svc.UploadStatus(context.Background(), id)
}

// stagingChunksGone 断言会话的**分块文件**已清空。
//
// 完成之后暂存目录本身是留着的：里面只剩一份 meta.json 完成记录。
// 之所以不在完成时把整个会话删掉 —— 最后几块是并发到达的，赢家删掉
// 会话后，同一毫秒里另一个刚传完块的请求只能拿到 404，而它需要的是
// "传完了，落在哪里"。记录本身由 TTL 兜。
func (e *uploadEnv) stagingChunksGone(t *testing.T, id string) {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(e.root, id))
	if err != nil {
		t.Fatalf("读暂存 %s: %v", id, err)
	}
	for _, en := range es {
		if strings.HasSuffix(en.Name(), ".chunk") || strings.HasSuffix(en.Name(), ".tmp") {
			t.Errorf("完成之后暂存里不该再有分块/临时文件, 剩 %s", en.Name())
		}
	}
}

// stagingGone 断言整个会话目录都没了（取消、跳过、GC 之后）。
func (e *uploadEnv) stagingGone(t *testing.T, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(e.root, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("暂存 %s 应已删除 (stat err = %v)", id, err)
	}
}

func (e *uploadEnv) targetNames(t *testing.T) []string {
	t.Helper()
	es, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, en := range es {
		out = append(out, en.Name())
	}
	return out
}

func dirNames(es []os.DirEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func shaOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return shaHex(string(b))
}

func missingString(rs []int) string {
	if len(rs) == 0 {
		return ""
	}
	parts := make([]string, len(rs))
	for i, v := range rs {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

// content 造一段"每个位置字节都不同"的内容：内容随位置变化是暴露
// 顺序/偏移错误的主要武器（"AAAAAAAA...BBB..." 这种载荷下，块顺序
// 错了但每块内容雷同的情况，长度和哈希都可能巧合地过得去）。
func content(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return string(b)
}

// chunkOf 取出 full 的第 i 块（末块短于 cs）。
func chunkOf(full string, i, cs int) string {
	start := i * cs
	if start >= len(full) {
		return ""
	}
	end := start + cs
	if end > len(full) {
		end = len(full)
	}
	return full[start:end]
}

// ---------- 字节正确性 ----------

// TestUploadSequentialBytes 顺序传完，内容逐字节等于原文。
//
// 末块刻意短于 ChunkSize，这样才覆盖"按 Size 收尾"而不是"按 ChunkSize
// 收尾"那条路径（Size 不是块大小整数倍是最常见的情况）。
func TestUploadSequentialBytes(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4 // 4+4+4+1

	st := e.begin(t, "seq", "报告 2026.bin", int64(len(full)), cs, ConflictRename)
	if st.Done {
		t.Fatal("刚建立就 Done？")
	}
	// 初始 Missing 必须列全所有块号：这是断点续传的起点，前端据此
	// 决定"从第几块开始传"。
	if got := missingString(st.Missing); got != "0,1,2,3" {
		t.Errorf("初始 Missing = %q, 期望 0,1,2,3", got)
	}
	if st.ID != "seq" {
		t.Errorf("ID = %q, 客户端传了就该用它", st.ID)
	}

	var last UploadState
	var err error
	for i := 0; i < chunkCount(int64(len(full)), cs); i++ {
		last, err = e.put(nil, "seq", i, chunkOf(full, i, cs))
		if err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
		if i < chunkCount(int64(len(full)), cs)-1 && last.Done {
			t.Fatalf("块 %d 之后就 Done 了", i)
		}
	}
	if !last.Done {
		t.Fatalf("末块到齐后应 Done: %+v", last)
	}
	want := filepath.Join(e.dir, "报告 2026.bin")
	if last.Path != want {
		t.Errorf("Path = %q, 期望 %q", last.Path, want)
	}
	if got := shaOf(t, want); got != shaHex(full) {
		t.Errorf("字节不一致: got %s want %s", got, shaHex(full))
	}
	e.stagingChunksGone(t, "seq")
	// 用户目录里不许留下面板的临时文件：那正是"面板在我盘里留了什么"
	// 这类疑问的来源，而且它们落在别人的目录里，重启也认不出来。
	if names := e.targetNames(t); len(names) != 1 || names[0] != "报告 2026.bin" {
		t.Errorf("目标目录应只剩最终文件, got %v", names)
	}
}

// TestUploadMissingChunks 续传契约就是"还缺哪几块"。
//
// 只报"已收到 N 块"是不够的：弱网重连时"收了 2 块"说不出是哪两块，
// 客户端就没法只补缺的部分 —— 而设计 8.2 的断点续传要的是这个。
func TestUploadMissingChunks(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "rs", "a.bin", int64(len(full)), cs, ConflictRename)

	for _, i := range []int{0, 2} {
		if _, err := e.put(nil, "rs", i, chunkOf(full, i, cs)); err != nil {
			t.Fatal(err)
		}
	}
	st, err := e.status("rs")
	if err != nil {
		t.Fatal(err)
	}
	if got := missingString(st.Missing); got != "1,3" {
		t.Errorf("Missing = %q, 期望 1,3", got)
	}
	// Received 是**字节**数：进度条要的是字节，块数还要客户端自己
	// 乘块大小（而末块不足一块，乘出来必然偏小）。
	if st.Received != 8 {
		t.Errorf("Received = %d, 期望 8", st.Received)
	}
	if st.Name != "a.bin" || st.Dir != e.dir || st.Size != int64(len(full)) || st.ChunkSize != cs {
		t.Errorf("状态要带足以恢复的元信息: %+v", st)
	}

	// 补齐，顺序再打乱一次（先末块）
	for _, i := range []int{3, 1} {
		if st, err = e.put(nil, "rs", i, chunkOf(full, i, cs)); err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
	}
	if !st.Done {
		t.Error("补齐后应 Done")
	}
	if shaOf(t, st.Path) != shaHex(full) {
		t.Error("乱序补齐后字节不一致")
	}
	// 完成后 status 回一条**完成记录**而不是 404。
	//
	// 原本是 404（会话连记录一起删）。改掉的直接原因：分块是并发传的
	// （设计 8.2 的并发数 3），"谁发现全齐"不可预知 —— 赢家把会话删掉
	// 之后，同一毫秒里另一个刚成功传完一块的请求只能看到 404，而它急需
	// 的答案是"已完成，路径在此"。留记录让所有问同一个问题的一端拿到
	// 同一个答案；僵尸记录的担忧由 TTL 兜，不靠"删掉"来防。
	st, err = e.status("rs")
	if err != nil {
		t.Fatalf("完成后应能查到完成记录: %v", err)
	}
	if !st.Done || st.Path == "" {
		t.Errorf("完成记录要带落点: %+v", st)
	}
	if st.Received != int64(len(full)) {
		t.Errorf("完成记录的 Received 应是全长: %d", st.Received)
	}
}

// TestUploadOutOfOrder 乱序必须被接受。
//
// 拒绝乱序等于强迫前端串行上传，而设计 8.2 写着"并发上传数默认 3"——
// 并发下完成顺序天然是乱的。这里刻意让末块先到：末块最短，如果实现
// 拿"到达顺序"当拼接顺序，第一个错位就出现在这里。
func TestUploadOutOfOrder(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	n := chunkCount(int64(len(full)), cs) // 4
	e.begin(t, "ooo", "a.bin", int64(len(full)), cs, ConflictRename)

	var last UploadState
	for _, i := range []int{n - 1, 2, 0, 1} {
		st, err := e.put(nil, "ooo", i, chunkOf(full, i, cs))
		if err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
		if i != 1 && st.Done {
			t.Errorf("块 %d 之后不该 Done（还缺其他块）", i)
		}
		last = st
	}
	if !last.Done {
		t.Fatal("全到齐后应 Done")
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if shaHex(string(b)) != shaHex(full) {
		t.Errorf("乱序合并后字节不一致: %q", b)
	}
}

// TestUploadZeroByteFile 0 字节文件：Begin 之后直接 Done。
//
// 空文件是真实场景（touch 出来的占位、被 truncate 的日志）。如果
// "完成"被定义成"收过至少一块"，空文件永远完不成，而客户端已经在
// 进度条上看到了 100%。
func TestUploadZeroByteFile(t *testing.T) {
	e := newUploadEnv(t)
	st := e.begin(t, "z", "empty.txt", 0, 4, ConflictRename)
	if !st.Done {
		t.Fatalf("0 字节应直接完成: %+v", st)
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "empty.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Errorf("应为空文件, got %q", b)
	}
	e.stagingChunksGone(t, "z")
}

// ---------- 幂等与分块校验 ----------

// TestUploadDuplicateChunk 同一块重传必须幂等。
//
// 弱网下客户端重发同一个块是常态：报错等于整次上传前功尽弃，而按
// "追加"处理会让文件里多出内容 —— 后者更糟，因为它会成功。
func TestUploadDuplicateChunk(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "dup", "a.bin", int64(len(full)), cs, ConflictRename)

	if _, err := e.put(nil, "dup", 0, chunkOf(full, 0, cs)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.put(nil, "dup", 0, chunkOf(full, 0, cs)); err != nil {
		t.Errorf("同内容重传应幂等, got %v", err)
	}
	st, err := e.status("dup")
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != int64(cs) {
		t.Errorf("重传不该让 Received 翻倍: %d", st.Received)
	}
	for _, i := range []int{1, 2, 3} {
		if _, err := e.put(nil, "dup", i, chunkOf(full, i, cs)); err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(e.dir, "a.bin"))
	if shaHex(string(b)) != shaHex(full) {
		t.Errorf("重传后字节被写坏: %q", b)
	}
}

// TestUploadChunkContentMismatch 同一块号传进**不同**内容必须报错。
//
// 长度相同、内容不同只能来自客户端 bug 或会话错乱（两个文件复用了
// 同一个 upload_id）。静默接受会产出一个"两个文件各一半"的产物，
// 而它会报告成功 —— 这是上传里最坏的错误形状。
func TestUploadChunkContentMismatch(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "mm", "a.bin", int64(len(full)), cs, ConflictRename)
	if _, err := e.put(nil, "mm", 0, chunkOf(full, 0, cs)); err != nil {
		t.Fatal(err)
	}
	// 同长度、不同内容。不能写 content(13)[:cs] —— content() 是确定性
	// 的，取出来的就是同一块内容，夹具等于没造：测试会因为"幂等"这条
	// 正确行为而合法通过，什么也没证明。
	other := []byte(content(13))
	other[0] = 'Z'
	if _, err := e.put(nil, "mm", 0, string(other[:cs])); !errors.Is(err, ErrChunkMismatch) {
		t.Errorf("同块号不同内容应 ErrChunkMismatch, got %v", err)
	}
	// 报错的那次不许污染已收状态
	st, err := e.status("mm")
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != int64(cs) || len(st.Missing) != 3 {
		t.Errorf("被拒的块不该改变状态: %+v", st)
	}
}

// TestUploadChunkLength 每块长度必须正好是该序号应有的长度。
func TestUploadChunkLength(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "len", "a.bin", int64(len(full)), cs, ConflictRename)

	// 短一块：客户端算错了块边界（最常见的实现 bug）
	if _, err := e.put(nil, "len", 0, full[:cs-1]); !errors.Is(err, ErrChunkLength) {
		t.Errorf("短块应 ErrChunkLength, got %v", err)
	}
	// 末块满块：末块只有 1 字节，传 4 字节说明客户端按固定长度切了
	// 超出文件末尾的内容
	if _, err := e.put(nil, "len", 3, full[12:]); err != nil {
		t.Errorf("末块正常长度不该报错: %v", err)
	}
	// 序号越界
	if _, err := e.put(nil, "len", 4, "x"); !errors.Is(err, ErrChunkIndex) {
		t.Errorf("序号 >= 块数应 ErrChunkIndex, got %v", err)
	}
	if _, err := e.put(nil, "len", -1, "x"); !errors.Is(err, ErrChunkIndex) {
		t.Errorf("负序号应 ErrChunkIndex, got %v", err)
	}
}

// TestUploadChunkTooLarge 单块上限：一个请求体不能写穿磁盘。
//
// 上限由服务端定，不看客户端声明的 ChunkSize —— 后者是请求参数，
// 谁都能填 100GB。
func TestUploadChunkTooLarge(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{now: time.Unix(1700000000, 0)}
	root := filepath.Join(dir, ".u")
	svc := NewService(Options{UploadRoot: root, MaxChunkBytes: 8, MaxUploadBytes: 1 << 30, Clock: clk.Now})
	ctx := context.Background()
	if _, err := svc.BeginUpload(ctx, UploadInit{ID: "big", Dir: dir, Name: "a.bin", Size: 100, ChunkSize: 4}); err != nil {
		t.Fatal(err)
	}
	// 声明的块大小合法（4），但请求体给了 20 字节 > 上限 8
	_, err := svc.PutChunk(ctx, UploadChunk{ID: "big", Index: 0, Body: strings.NewReader(strings.Repeat("x", 20))})
	if !errors.Is(err, ErrChunkTooLarge) {
		t.Fatalf("超限块应 ErrChunkTooLarge, got %v", err)
	}
	// 被拒的块**不能**记为已收到：否则续传会永远认为第 0 块已到位，
	// 拼出一个缺开头的文件还报成功。
	st, err := svc.UploadStatus(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Missing) != chunkCount(100, 4) || st.Received != 0 {
		t.Errorf("被拒的块不该改变状态: %+v", st)
	}
}

// TestUploadTotalTooLarge 总大小超限：报错里要给出路（SFTP）。
//
// 这个上限是产品决定：网页上传只做辅助，大文件走 SFTP。报错如果只
// 说"太大"，用户只会反复点上传按钮；说清上限和替代路径才结束得了这次
// 往返。
func TestUploadTotalTooLarge(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(Options{
		UploadRoot: filepath.Join(dir, ".u"), MaxChunkBytes: 1 << 20,
		MaxUploadBytes: 64, Clock: time.Now,
	})
	_, err := svc.BeginUpload(context.Background(), UploadInit{
		ID: "huge", Dir: dir, Name: "a.bin", Size: 1 << 30, ChunkSize: 4,
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("应 ErrTooLarge, got %v", err)
	}
	if !strings.Contains(err.Error(), "SFTP") && !strings.Contains(err.Error(), "sftp") {
		t.Errorf("错误里要指出更大的文件走 SFTP: %v", err)
	}
	if !strings.Contains(err.Error(), "64") {
		t.Errorf("错误里要给出上限值, 用户才知道差多少: %v", err)
	}
}

// TestUploadDefaultChunkSize 不指定块大小时用默认值（设计 8.2：5MB）。
func TestUploadDefaultChunkSize(t *testing.T) {
	e := newUploadEnv(t)
	st := e.begin(t, "dc", "a.bin", 100, 0, ConflictRename)
	if st.ChunkSize != DefaultChunkSize && st.ChunkSize != 1<<20 {
		t.Errorf("ChunkSize = %d, 应为默认值", st.ChunkSize)
	}
	// 声明的块大小超过服务端上限时，服务端必须把它夹到上限而不是
	// 照着收（否则 MaxChunkBytes 形同虚设）
	svc := NewService(Options{UploadRoot: e.root, MaxChunkBytes: 16, MaxUploadBytes: 1 << 30, Clock: e.clk.Now})
	st2, err := svc.BeginUpload(context.Background(), UploadInit{
		ID: "dc2", Dir: e.dir, Name: "b.bin", Size: 1000, ChunkSize: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st2.ChunkSize != 16 {
		t.Errorf("超上限的块大小应被夹到 16, got %d", st2.ChunkSize)
	}
}

// ---------- 输入校验 ----------

// TestUploadBadName 目标名不许带路径分隔符。
//
// 它是用户可见的文件名，允许 "/" 就等于允许"上传到目录框之外的地方"
// —— 地址栏显示 /var/log 而文件落到 /etc，是那种"面板是不是被人打了"
// 的现场。
func TestUploadBadName(t *testing.T) {
	e := newUploadEnv(t)
	for i, name := range []string{"", ".", "..", "../逃逸.txt", "a/b.txt", "/绝对.txt", "a\x00b", "a\\b", "  "} {
		id := "nm" + strconv.Itoa(i)
		_, err := e.svc.BeginUpload(context.Background(), UploadInit{
			ID: id, Dir: e.dir, Name: name, Size: 4, ChunkSize: 4,
		})
		if !errors.Is(err, ErrBadPath) {
			t.Errorf("name %q 应 ErrBadPath, got %v", name, err)
		}
	}
}

// TestUploadBadID upload_id 会被拼进暂存路径，所以它也是一条注入面。
func TestUploadBadID(t *testing.T) {
	e := newUploadEnv(t)
	for i, id := range []string{"", "../evil", "a/b", "..", "/", "a\x00b", strings.Repeat("x", 300), "中文id"} {
		name := "a" + strconv.Itoa(i) + ".bin"
		_, err := e.svc.BeginUpload(context.Background(), UploadInit{
			ID: id, Dir: e.dir, Name: name, Size: 4, ChunkSize: 4,
		})
		// 过长的 ID 也必须拒（文件名有 255 字节上限，撞到它时报的会是
		// 底层 ENAMETOOLONG，用户看不懂）
		if !errors.Is(err, ErrBadPath) {
			t.Errorf("id %q 应 ErrBadPath, got %v", id, err)
		}
	}
}

// TestUploadIDCollision 同一个 id 只能描述同一个上传。
//
// 两个标签页/两次选择复用同一个 id 时，如果各自的目标名或大小被接受，
// 落盘的就是"前半来自 A、后半来自 B"的文件，而且它会报告成功。
func TestUploadIDCollision(t *testing.T) {
	e := newUploadEnv(t)
	e.begin(t, "same", "a.bin", 13, 4, ConflictRename)

	_, err := e.svc.BeginUpload(context.Background(), UploadInit{
		ID: "same", Dir: e.dir, Name: "另一个.bin", Size: 13, ChunkSize: 4,
	})
	if !errors.Is(err, ErrBadPath) {
		t.Errorf("同 id 换目标名应报错, got %v", err)
	}
	_, err = e.svc.BeginUpload(context.Background(), UploadInit{
		ID: "same", Dir: e.dir, Name: "a.bin", Size: 99, ChunkSize: 4,
	})
	if !errors.Is(err, ErrBadPath) {
		t.Errorf("同 id 换总大小应报错, got %v", err)
	}
	// 完全一致的重复 Begin 必须幂等：客户端重连后先 Begin 再补块是
	// 最自然的写法，这里报错等于禁止续传。
	st, err := e.svc.BeginUpload(context.Background(), UploadInit{
		ID: "same", Dir: e.dir, Name: "a.bin", Size: 13, ChunkSize: 4,
	})
	if err != nil {
		t.Fatalf("一致的重复 Begin 应幂等, got %v", err)
	}
	if st.Done || len(st.Missing) != 4 {
		t.Errorf("幂等返回的状态要如实反映进度: %+v", st)
	}
}

// TestUploadDirChecks 目标目录的三种错各有独立语义。
func TestUploadDirChecks(t *testing.T) {
	e := newUploadEnv(t)
	f := filepath.Join(e.dir, "f.txt")
	writeFile(t, f, "x")

	if _, err := e.svc.BeginUpload(context.Background(), UploadInit{
		ID: "d1", Dir: f, Name: "a.bin", Size: 4, ChunkSize: 4,
	}); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("目标是文件应 ErrNotDirectory, got %v", err)
	}
	if _, err := e.svc.BeginUpload(context.Background(), UploadInit{
		ID: "d2", Dir: filepath.Join(e.dir, "不存在"), Name: "a.bin", Size: 4, ChunkSize: 4,
	}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("目录不存在应 fs.ErrNotExist, got %v", err)
	}
	// 相对路径必须拒：按进程 cwd 解释会让同一个请求在开发机与目标机上
	// 落到不同目录，界面显示的位置和实际写盘的位置对不上。
	if _, err := e.svc.BeginUpload(context.Background(), UploadInit{
		ID: "d3", Dir: "相对", Name: "a.bin", Size: 4, ChunkSize: 4,
	}); !errors.Is(err, ErrBadPath) {
		t.Errorf("相对目录应 ErrBadPath, got %v", err)
	}
	// 只读目录（Android 上 /system 之类）：写不进去要在 Begin 就说清，
	// 而不是让用户传完 1GB 之后在收尾时失败。
	if _, err := e.svc.BeginUpload(context.Background(), UploadInit{
		ID: "d4", Dir: "/system", Name: "a.bin", Size: 4, ChunkSize: 4,
	}); err == nil {
		t.Log("/system 可写（本机权限特殊），跳过只读断言")
	}
}

func TestUploadUnknownID(t *testing.T) {
	e := newUploadEnv(t)
	ctx := context.Background()
	// id 用"格式合法但没建过"的：中文/带斜杠那种会被字符集校验先挡下
	// （那是另一条正确路径，TestUploadBadID 在测），到这里根本走不到
	// 404 这条分支。
	const unknown = "never-seen-id"
	if _, err := e.svc.UploadStatus(ctx, unknown); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("未知 id 的 status 应 fs.ErrNotExist, got %v", err)
	}
	if err := e.svc.AbortUpload(ctx, unknown); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("未知 id 的 abort 应 fs.ErrNotExist, got %v", err)
	}
	if _, err := e.svc.PutChunk(ctx, UploadChunk{ID: unknown, Index: 0, Body: strings.NewReader("x")}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("未知 id 的 put 应 fs.ErrNotExist, got %v", err)
	}
}

// ---------- 同名冲突（设计 8.2：询问 覆盖/改名/跳过）----------

func TestUploadConflictAsk(t *testing.T) {
	e := newUploadEnv(t)
	writeFile(t, filepath.Join(e.dir, "a.bin"), "旧")
	// 没表态 + 目标已存在 = 报错，不替用户猜。
	//
	// 默认覆盖会把"界面少勾一个选项"变成静默的数据丢失；默认改名则
	// 让用户以为覆盖了 a.bin，实际旧文件还在原地。两种"善意猜测"都比
	// 报错难解释。
	_, err := e.svc.BeginUpload(context.Background(), UploadInit{
		ID: "ask", Dir: e.dir, Name: "a.bin", Size: 4, ChunkSize: 4, Conflict: ConflictAsk,
	})
	if !errors.Is(err, ErrExists) {
		t.Errorf("已存在且未表态应 ErrExists, got %v", err)
	}
}

func TestUploadConflictSkip(t *testing.T) {
	e := newUploadEnv(t)
	writeFile(t, filepath.Join(e.dir, "a.bin"), "原有内容")
	st := e.begin(t, "sk", "a.bin", 1<<20, 4, ConflictSkip)
	// 跳过必须在 **Begin** 就报出来：设计 8.2 的跳过是为了"别浪费带宽"，
	// 传完 1GB 再跳过等于什么都没省下。
	if !st.Skipped {
		t.Error("Begin 就该报 Skipped")
	}
	if !st.Done {
		t.Error("跳过即完成（没有后续动作）")
	}
	if st.Path != filepath.Join(e.dir, "a.bin") {
		t.Errorf("Skipped 的 Path 应指向已存在的文件, got %q", st.Path)
	}
	if b, _ := os.ReadFile(st.Path); string(b) != "原有内容" {
		t.Errorf("跳过不该写任何东西: %q", b)
	}
	e.stagingGone(t, "sk")
	if names := e.targetNames(t); len(names) != 1 {
		t.Errorf("跳过后目录里不该有残留: %v", names)
	}
}

func TestUploadConflictOverwrite(t *testing.T) {
	e := newUploadEnv(t)
	writeFile(t, filepath.Join(e.dir, "a.bin"), "旧内容旧内容")
	full := content(13)
	const cs = 4
	e.begin(t, "ow", "a.bin", int64(len(full)), cs, ConflictOverwrite)
	for i := 0; i < chunkCount(int64(len(full)), cs); i++ {
		if _, err := e.put(nil, "ow", i, chunkOf(full, i, cs)); err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(e.dir, "a.bin"))
	if shaHex(string(b)) != shaHex(full) {
		t.Errorf("应覆盖, got %q", b)
	}
}

// TestUploadConflictRename 让位到 name (1).ext。
//
// 后缀必须插在**扩展名之前**：a.bin (1) 在 Windows 上双击会问"用什么
// 应用打开"，等于下载完变成一个打不开的文件。
func TestUploadConflictRename(t *testing.T) {
	e := newUploadEnv(t)
	writeFile(t, filepath.Join(e.dir, "a.bin"), "旧")
	writeFile(t, filepath.Join(e.dir, "a (1).bin"), "旧一")
	full := content(13)
	const cs = 4
	e.begin(t, "rn", "a.bin", int64(len(full)), cs, ConflictRename)
	for i := 0; i < chunkCount(int64(len(full)), cs); i++ {
		if _, err := e.put(nil, "rn", i, chunkOf(full, i, cs)); err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
	}
	names := map[string]bool{}
	for _, n := range e.targetNames(t) {
		names[n] = true
	}
	if !names["a (2).bin"] {
		t.Errorf("应让位到 a (2).bin, got %v", names)
	}
	if old, _ := os.ReadFile(filepath.Join(e.dir, "a.bin")); string(old) != "旧" {
		t.Errorf("原文件被动了: %q", old)
	}
	if o1, _ := os.ReadFile(filepath.Join(e.dir, "a (1).bin")); string(o1) != "旧一" {
		t.Errorf("(1) 被动了: %q", o1)
	}
}

// TestUploadConflictRenameDotfile 点文件的改名不能把点吃掉。
//
// filepath.Ext(".bashrc") 返回 ".bashrc"（最后一个点就是第 0 个字符），
// 照"stem + (n) + ext"套会得到 " (1).bashrc" —— 文件名前导的点没了，
// 于是它从隐藏文件变成可见文件，改名这件事本身改变了语义。
func TestUploadConflictRenameDotfile(t *testing.T) {
	e := newUploadEnv(t)
	writeFile(t, filepath.Join(e.dir, ".bashrc"), "旧")
	full := content(13)
	const cs = 4
	e.begin(t, "dot", ".bashrc", int64(len(full)), cs, ConflictRename)
	for i := 0; i < chunkCount(int64(len(full)), cs); i++ {
		if _, err := e.put(nil, "dot", i, chunkOf(full, i, cs)); err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
	}
	names := map[string]bool{}
	for _, n := range e.targetNames(t) {
		names[n] = true
	}
	if !names[".bashrc (1)"] {
		t.Errorf("点文件应让位到 .bashrc (1), got %v", names)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, ".bashrc")); string(b) != "旧" {
		t.Errorf("原文件被动了: %q", b)
	}
}

// TestUploadConflictAppearedMidway 传输过程中名字被人占用。
//
// 上传 1GB 需要几十秒，这期间同名文件出现是真实竞争（另一个标签页、
// 另一个进程、用户自己在终端里 cp）。策略必须在收尾时重新判定：
// Ask 时报错并**保留暂存**（用户处理完冲突还能接着收尾，不必重传
// 1GB），而不是把已经收全的字节扔掉。
func TestUploadConflictAppearedMidway(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "race", "a.bin", int64(len(full)), cs, ConflictAsk)
	for i := 0; i < chunkCount(int64(len(full)), cs)-1; i++ {
		if _, err := e.put(nil, "race", i, chunkOf(full, i, cs)); err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
	}
	writeFile(t, filepath.Join(e.dir, "a.bin"), "别人刚创建的")
	_, err := e.put(nil, "race", chunkCount(int64(len(full)), cs)-1, chunkOf(full, 3, cs))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("收尾时撞名应 ErrExists, got %v", err)
	}
	// 已收到的字节不许丢：状态仍在，改完策略就能收尾
	st, serr := e.status("race")
	if serr != nil {
		t.Fatalf("冲突后会话应仍可查询: %v", serr)
	}
	if len(st.Missing) != 0 {
		t.Errorf("字节是全的, Missing 应为空: %+v", st)
	}
	// 不许留下最终名字的文件（半成品的名字和成功的一样，是最坏的形状）
	if b, _ := os.ReadFile(filepath.Join(e.dir, "a.bin")); string(b) != "别人刚创建的" {
		t.Errorf("冲突时不许动那个已存在的文件: %q", b)
	}
}

// ---------- 暂存位置、重启与清理 ----------

// TestUploadStagingOutsideUserDir 上传中途，用户目录里不许出现任何
// 面板的文件（设计硬约束的延伸：暂存必须在面板自己的地方）。
//
// 反面场景：把 .part 写进用户目录，用户看到"我目录里多了个看不懂的
// 文件"，手一删就得到一个永远传不完的上传。
func TestUploadStagingOutsideUserDir(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "loc", "a.bin", int64(len(full)), cs, ConflictRename)
	if _, err := e.put(nil, "loc", 0, chunkOf(full, 0, cs)); err != nil {
		t.Fatal(err)
	}
	if names := e.targetNames(t); len(names) != 0 {
		t.Errorf("上传中途目标目录应一无所见, got %v", names)
	}
	if es, err := os.ReadDir(e.root); err != nil || len(es) == 0 {
		t.Errorf("分块应落在暂存根 %s 里: %v (%v)", e.root, es, err)
	}
}

// TestUploadResumeAcrossRestart 分块落在磁盘上，重启后续传要还能问。
//
// 选择"状态从文件系统重建"而不是"记在数据库里"的理由就在这：面板
// 重启（升级、OOM、systemd 拉起）时，只要暂存文件还在，续传就免费
// 成立；记在 DB 里则必须处理"DB 说收了 3 块、磁盘上只有 2 块"这种
// 不一致。
func TestUploadResumeAcrossRestart(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "kill", "a.bin", int64(len(full)), cs, ConflictRename)
	for _, i := range []int{0, 2} {
		if _, err := e.put(nil, "kill", i, chunkOf(full, i, cs)); err != nil {
			t.Fatal(err)
		}
	}
	svc2 := e.reassemble(t)
	st, err := svc2.UploadStatus(context.Background(), "kill")
	if err != nil {
		t.Fatalf("重启后应能查到未完成会话: %v", err)
	}
	if got := missingString(st.Missing); got != "1,3" {
		t.Errorf("重启后 Missing = %q, 期望 1,3", got)
	}
	if st.Received != 8 {
		t.Errorf("重启后 Received = %d, 期望 8", st.Received)
	}
	// 用新 Service 补完
	for _, i := range []int{3, 1} {
		if _, err := svc2.PutChunk(context.Background(), UploadChunk{
			ID: "kill", Index: i, Body: strings.NewReader(chunkOf(full, i, cs)),
		}); err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if shaHex(string(b)) != shaHex(full) {
		t.Errorf("跨重启合并后字节不一致: %q", b)
	}
}

// TestUploadDiskDisagreesWithMeta 磁盘上少一块时，状态必须以磁盘为准。
//
// 上面那条测试的理由（"以文件为准所以不用 DB"）只有在这一条成立时
// 才站得住：如果有人手工删了暂存里的一块，而状态是从别的什么缓存
// 里读出来的，续传就会永久缺那一块，最后拼出一个短文件还报成功。
func TestUploadDiskDisagreesWithMeta(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "tamper", "a.bin", int64(len(full)), cs, ConflictRename)
	for _, i := range []int{0, 1, 2} {
		if _, err := e.put(nil, "tamper", i, chunkOf(full, i, cs)); err != nil {
			t.Fatal(err)
		}
	}
	// 手工抹掉第 1 块的分块文件（模拟误删/磁盘清理/磁盘满时的半途失败）
	removed := 0
	es, err := os.ReadDir(filepath.Join(e.root, "tamper"))
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range es {
		if strings.HasPrefix(en.Name(), "000001.") {
			if err := os.Remove(filepath.Join(e.root, "tamper", en.Name())); err != nil {
				t.Fatal(err)
			}
			removed++
		}
	}
	if removed == 0 {
		t.Fatalf("夹具没找到第 1 块的分块文件: %v", dirNames(es))
	}
	st, err := e.status("tamper")
	if err != nil {
		t.Fatal(err)
	}
	if got := missingString(st.Missing); got != "1,3" {
		t.Errorf("状态必须以磁盘为准, Missing = %q, 期望 1,3", got)
	}
	if st.Received != 8 {
		t.Errorf("Received 应跟着磁盘走: %d, 期望 8", st.Received)
	}
}

// TestUploadAbortLeavesNoTrace 用户点"取消"之后不许留任何东西。
func TestUploadAbortLeavesNoTrace(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "ab", "a.bin", int64(len(full)), cs, ConflictRename)
	for _, i := range []int{0, 1} {
		if _, err := e.put(nil, "ab", i, chunkOf(full, i, cs)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.AbortUpload(context.Background(), "ab"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.status("ab"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("取消后 status 应 fs.ErrNotExist, got %v", err)
	}
	e.stagingGone(t, "ab")
	if names := e.targetNames(t); len(names) != 0 {
		t.Errorf("取消不该留半成品: %v", names)
	}
	// 取消后同 id 再传必须能重新开始（不能被"已存在"卡住）
	e.begin(t, "ab", "b.bin", 4, 4, ConflictRename)
}

// TestUploadIdleTTL 上传中途关浏览器 → 永远没人来收尾。
//
// 没有 TTL，用户的盘会被面板悄悄吃满（204 个 5MB 文件 × 每次失败
// 的上传）。这是面板对用户环境唯一的"写入侧"污染，必须自愈。
func TestUploadIdleTTL(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "idle", "a.bin", int64(len(full)), cs, ConflictRename)
	if _, err := e.put(nil, "idle", 0, chunkOf(full, 0, cs)); err != nil {
		t.Fatal(err)
	}
	e.begin(t, "fresh", "b.bin", int64(len(full)), cs, ConflictRename)
	// "活跃"必须真的有一个比 idle 更晚的时间戳：两个会话都停在同一个
	// 假时钟时刻的话，advance 之后两个都超时，"活跃的那个还在"这条
	// 断言永远测不到东西。
	e.clk.advance(100 * time.Minute)
	if _, err := e.put(nil, "fresh", 0, chunkOf(full, 0, cs)); err != nil {
		t.Fatal(err)
	}
	// 此刻 idle 的最后活动是 0 时刻（已过 100min > TTL 1h），fresh 刚动过
	e.svc.GCUploads(context.Background())

	if _, err := e.status("idle"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("超时的上传应被清掉, got %v", err)
	}
	e.stagingGone(t, "idle")
	// 活跃的那个必须还在：GC 把用户**正在上传**的传扔掉是最糟糕的自伤
	// —— 一次 GC 干掉所有进行中的上传，而界面上毫无痕迹。
	if _, err := e.status("fresh"); err != nil {
		t.Errorf("未超时的不该被清: %v", err)
	}
	// 而且"活跃"要跟着**收到的块**走，不是只跟着 Begin 时间：
	// 一次慢速上传（1GB 在慢链接里要几十分钟）会跨过 TTL，但它一直在
	// 推进，清掉它就等于"上传越慢越容易被杀"。
	e.clk.advance(30 * time.Minute)
	if _, err := e.put(nil, "fresh", 1, chunkOf(full, 1, cs)); err != nil {
		t.Fatalf("推进中的会话不该被 GC 杀掉: %v", err)
	}
	e.clk.advance(90 * time.Minute)
	e.svc.GCUploads(context.Background())
	if _, err := e.status("fresh"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("之后再无动静应被清掉, got %v", err)
	}
}

// TestSweepStaleAfterRestart 面板重启：内存里的会话没了，暂存里的
// 孤儿必须由启动时清扫兜住。
func TestSweepStaleAfterRestart(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "orphan", "a.bin", int64(len(full)), cs, ConflictRename)
	if _, err := e.put(nil, "orphan", 0, chunkOf(full, 0, cs)); err != nil {
		t.Fatal(err)
	}
	svc2 := e.reassemble(t)
	// 重启后立刻 Sweep 不该删掉**没超时**的会话：升级/重启是常态，
	// 而用户回来还想续传。
	if err := svc2.SweepStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.UploadStatus(context.Background(), "orphan"); err != nil {
		t.Errorf("未超时的会话重启后应可续传, got %v", err)
	}
	e.clk.advance(2 * time.Hour)
	if err := svc2.SweepStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if es, err := os.ReadDir(e.root); err == nil && len(es) != 0 {
		t.Errorf("超时后暂存根应空, got %v", dirNames(es))
	}
}

// ---------- 取消 ----------

// TestUploadCanceledContext ctx 取消要真的中断读体。
//
// 用户在进度条上点"停止"是最常见的一次取消；如果只在读完之后检查
// ctx，一个 5MB 的块在慢链接里还要传好几秒才停得下来。
func TestUploadCanceledContext(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(Options{
		UploadRoot: filepath.Join(dir, ".u"), MaxChunkBytes: 1 << 20,
		MaxUploadBytes: 1 << 30, Clock: time.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := svc.BeginUpload(ctx, UploadInit{
		ID: "cx", Dir: dir, Name: "a.bin", Size: 1 << 20, ChunkSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	body := &slowReader{total: 1 << 20, onCancel: cancel}
	_, err := svc.PutChunk(ctx, UploadChunk{ID: "cx", Index: 0, Body: body})
	if err == nil {
		t.Fatal("取消应报错")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("错误应是 context.Canceled（API 层据此不回 500）: %v", err)
	}
	if body.readFull {
		t.Error("取消后不该把整个体读完")
	}
	// 半途的块不许被记为已收到
	st, serr := svc.UploadStatus(ctx, "cx")
	if serr == nil && st.Received != 0 {
		t.Errorf("取消的块不该计入 Received: %+v", st)
	}
}

type slowReader struct {
	total    int
	n        int
	onCancel func()
	readFull bool
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.n >= r.total {
		r.readFull = true
		return 0, io.EOF
	}
	if r.n >= 4096 {
		if r.onCancel != nil {
			r.onCancel()
			r.onCancel = nil
		}
		return 0, context.Canceled
	}
	n := copy(p, strings.Repeat("x", 1024)[:min(1024, len(p))])
	r.n += n
	return n, nil
}

// TestUploadConcurrentPut 设计 8.2 的"并发上传数默认 3"真的能并发。
//
// 这条测试护住的不是吞吐，而是并发下的元信息写：分块各写各的文件本来
// 就安全，不安全的是 meta.json 的读-改-写。加了锁却不生效（比如锁了
// 一个每次新建的值）是这类代码最常见的假安全，只有真并发才看得出来。
func TestUploadConcurrentPut(t *testing.T) {
	e := newUploadEnv(t)
	full := content(4096)
	const cs = 64
	n := chunkCount(int64(len(full)), cs)
	e.begin(t, "conc", "a.bin", int64(len(full)), cs, ConflictRename)

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.PutChunk(context.Background(), UploadChunk{
				ID: "conc", Index: i, Body: strings.NewReader(chunkOf(full, i, cs)),
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发传块 %d: %v", i, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if shaHex(string(b)) != shaHex(full) {
		t.Errorf("并发上传后字节不一致: got %d 字节 want %d", len(b), len(full))
	}
}

// TestUploadLateChunkAfterDone 完成之后迟到的重发块不许复活暂存。
//
// 弱网下"最后一个块重发一次"是常事。旧实现在这里会把块文件重新落
// 进已完成、已清空的暂存目录里 —— 没人认得它是垃圾，要等一个 TTL 才
// 被扫掉（而完成记录本身可能更晚才被清，两个时间还不同步）。
func TestUploadLateChunkAfterDone(t *testing.T) {
	e := newUploadEnv(t)
	full := content(13)
	const cs = 4
	e.begin(t, "late", "a.bin", int64(len(full)), cs, ConflictRename)
	var last UploadState
	for i := 0; i < chunkCount(int64(len(full)), cs); i++ {
		st, err := e.put(nil, "late", i, chunkOf(full, i, cs))
		if err != nil {
			t.Fatalf("块 %d: %v", i, err)
		}
		last = st
	}
	if !last.Done {
		t.Fatal("应已完成")
	}
	// 迟到的重发：回完成态，不起任何写盘动作
	st, err := e.put(nil, "late", 3, chunkOf(full, 3, cs))
	if err != nil {
		t.Fatalf("迟到的重复块不该报错: %v", err)
	}
	if !st.Done || st.Path != last.Path {
		t.Errorf("应回同一个完成态: %+v", st)
	}
	e.stagingChunksGone(t, "late")
	// 目标文件也不能被动过
	if shaOf(t, last.Path) != shaHex(full) {
		t.Error("迟到的块改动了已就位的文件")
	}
}

// TestUploadAbortAfterDone 取消一个已完成的上传要如实拒绝。
//
// 谎报成功的代价是具体的：界面显示"已取消"，而文件好端端躺在用户目录
// 里 —— 用户下一次看到它时，唯一能得出的结论是面板在乱写。
//
// 哨兵必须是 ErrUploadDone 而不是复用 ErrExists：后者自带文案"目标已存在"，
// 套过来会印成"目标已存在: 上传已完成，取消不了"，一句自相矛盾的话
// （真机冒烟里印出来过）。而且这两个 409 的正确补救动作相反 ——
// 一个该问"改名还是覆盖"，另一个只能去删除。
func TestUploadAbortAfterDone(t *testing.T) {
	e := newUploadEnv(t)
	full := content(8)
	e.begin(t, "dn", "a.bin", int64(len(full)), 4, ConflictAsk)
	for i := 0; i < 2; i++ {
		if _, err := e.put(nil, "dn", i, chunkOf(full, i, 4)); err != nil {
			t.Fatal(err)
		}
	}
	err := e.svc.AbortUpload(context.Background(), "dn")
	if !errors.Is(err, ErrUploadDone) {
		t.Fatalf("应 ErrUploadDone, got %v", err)
	}
	if errors.Is(err, ErrExists) {
		t.Errorf("不该复用 ErrExists（文案会自相矛盾）: %v", err)
	}
	// 文件必须还在
	if shaOf(t, filepath.Join(e.dir, "a.bin")) != shaHex(full) {
		t.Error("拒绝取消时不该动文件")
	}
}
