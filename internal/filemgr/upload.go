package filemgr

// 上传（设计 8.2：分块 + 断点续传）。
//
// 形状由两条约束决定：
//
//  1. 容量上限。网页版是辅助功能，单次上传 ≤1GB；更大的文件让用户走
//     SFTP（ErrTooLarge 的文案里给出路）。这个上限换掉了整条"按偏移
//     pwrite 进一个大文件"的实现：改成**一个分块一个文件、完成时按
//     序号拼接**，偏移算术整个不存在 —— "下载下来的文件大小对而内容
//     鬼"这一整类错误在结构上就不可能出现。代价是每次最多 204 个暂存
//     文件（1GB ÷ 5MB），由上限兜住。
//
//  2. 状态从文件系统重建，不写数据库。分块文件本身就是记录：面板
//     重启（升级、OOM、systemd 拉起）之后，只要暂存还在，续传免费
//     成立。记在 DB 里则必须处理"DB 说收了 3 块、盘上只有 2 块"这种
//     不一致 —— 而那种不一致一定会发生（磁盘满、人工清理、半途 kill）。
//     所以 UploadStatus 每次都重新读目录，磁盘是唯一的真相。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"litepanel/internal/logx"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Conflict 是同名冲突策略（设计 8.2：询问 覆盖/重命名/跳过）。
type Conflict string

const (
	// ConflictAsk 是"没有表态"。目标已存在时报 ErrExists，而不是替
	// 用户猜。
	//
	// 不默认覆盖：默认覆盖把"界面少勾一个选项"变成静默的数据丢失。
	// 也不静默改名：用户以为覆盖了 a.bin，实际旧文件还在原地，而新
	// 文件叫 a (1).bin。两种"善意的猜测"都比老实报错难解释。
	ConflictAsk Conflict = ""
	// ConflictOverwrite 覆盖同名文件。
	ConflictOverwrite Conflict = "overwrite"
	// ConflictRename 让位到 name (1).ext。
	ConflictRename Conflict = "rename"
	// ConflictSkip 跳过：一个字节都不传。
	ConflictSkip Conflict = "skip"
)

func (c Conflict) valid() bool {
	switch c {
	case ConflictAsk, ConflictOverwrite, ConflictRename, ConflictSkip:
		return true
	}
	return false
}

// 上限默认值。数字来自设计 8.2（默认 5MB/块）。
const (
	// DefaultChunkSize 是分块大小默认值。
	DefaultChunkSize = 5 << 20
	// DefaultMaxChunkBytes 是单块请求体的硬上限。服务端说了算：客户端
	// 声明的块大小是请求参数，谁都能填 100GB。
	DefaultMaxChunkBytes = 8 << 20
	// DefaultMaxUploadBytes 是单次上传的总大小上限（≈1GB，见包注释）。
	DefaultMaxUploadBytes = 1 << 30
	// DefaultUploadTTL 是未完成会话的闲置上限。
	DefaultUploadTTL = 24 * time.Hour
	// maxUploadIDLen 限制 upload_id 长度：它会被拼进暂存路径，撞到
	// 255 字节的文件名上限时报的是底层 ENAMETOOLONG，用户看不懂。
	maxUploadIDLen = 64
)

// 上传相关的哨兵错误。API 层按这些映射状态码，所以每条都自带"该回什么"：
//
//	ErrTooLarge      413（并给出 SFTP 出路）
//	ErrChunkTooLarge 413
//	ErrChunkIndex    400
//	ErrChunkLength   400
//	ErrChunkMismatch 409
//	ErrExists        409
//	fs.ErrNotExist   404（未知 id）
//	context.Canceled 499/静默（客户端取消）
var (
	// ErrUploadDone 表示"这个会话已经收尾成功了"。
	//
	// 不复用 ErrExists：那个哨兵自己的文案是"目标已存在"，套到
	// "取消一个已完成的上传"上会得到"目标已存在: 上传已完成，取消不了"
	// —— 一句自相矛盾的话（实测冒烟里就是这么印出来的）。而且前端要
	// 分得开这两种 409：一种要问"换个名字还是覆盖"，另一种要告诉用户
	// "文件已经在了，要撤就用删除功能"。
	ErrUploadDone    = errors.New("上传已完成")
	ErrTooLarge      = errors.New("文件超过网页上传上限")
	ErrChunkTooLarge = errors.New("分块超过单块上限")
	ErrChunkIndex    = errors.New("分块序号越界")
	ErrChunkLength   = errors.New("分块长度与序号不符")
	ErrChunkMismatch = errors.New("同一分块传入了不同内容")
)

// UploadInit 是建立（或恢复）一个上传会话的请求。
type UploadInit struct {
	// ID 由客户端生成（一个 UUID 即可），在会话期间保持不变。
	ID string
	// Dir 是目标目录（绝对路径）。
	Dir string
	// Name 是目标文件名（不含目录）。
	Name string
	// Size 是文件总字节数。0 合法（空文件）。
	Size int64
	// ChunkSize 是客户端选用的分块大小；0 = DefaultChunkSize。
	// 超过服务端单块上限时被夹到上限。
	ChunkSize int64
	// Conflict 是同名策略。
	Conflict Conflict
}

// UploadState 是会话状态，同时充当三个端点的响应体。
type UploadState struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	Path      string `json:"path,omitempty"`
	Size      int64  `json:"size"`
	ChunkSize int64  `json:"chunk_size"`
	Received  int64  `json:"received"`
	// Missing 是还缺哪些分块序号（升序）。断点续传的全部契约就是它：
	// 只报"已收到 N 块"说不出是哪几块，客户端就没法只补缺的部分。
	Missing []int `json:"missing,omitempty"`
	Done    bool  `json:"done"`
	Skipped bool  `json:"skipped,omitempty"`
}

// UploadChunk 是一个分块。
type UploadChunk struct {
	ID    string
	Index int
	Body  io.Reader
}

// sessionMeta 是暂存目录里的元信息文件。
//
// 存的是"客户端声明的意图"（目标目录、名字、总大小、块大小、策略），
// 这些无法从分块文件本身推出来。反过来，"收了哪些块"**绝不**记在这
// 里 —— 那必须由目录列举说了算，否则磁盘与元信息会漂移（见包注释 2）。
type sessionMeta struct {
	ID       string    `json:"id"`
	Dir      string    `json:"dir"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Chunk    int64     `json:"chunk"`
	Conflict Conflict  `json:"conflict"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	// Done/Path 记"已经收尾成功"以及放到了哪里。
	//
	// 为什么完成之后还留着这条记录而不是把整个会话删掉：最后几块是
	// **并发**到达的（设计 8.2 的并发上传数 3），谁抢到"发现全齐"不可
	// 预知。赢家把会话连元信息一起删掉的话，同一毫秒里的另一个请求
	// 只能看到 404 —— 而它刚成功传完一块。留一条完成记录，让所有问
	// "传完了吗"的一端都能拿到同一个答案（tus 的做法：offset==size
	// 即完成，资源留到过期）。代价由 GCUploads 兜：完成记录同样按
	// TTL 清，不会攴成僵尸。
	Done bool   `json:"done,omitempty"`
	Path string `json:"path,omitempty"`
}

const metaFile = "meta.json"

// BeginUpload 建立会话，或者幂等地恢复一个已存在的会话。
//
// 幂等是必须的：客户端重连后"先 Begin 再补缺块"是最自然的写法，这里
// 报错等于禁止续传。参数与既有会话不一致时报 ErrBadPath —— 同一个 id
// 描述两个不同的上传，只会得到"前半来自 A、后半来自 B"的产物，而它
// 会报告成功。
func (s *Service) BeginUpload(ctx context.Context, in UploadInit) (UploadState, error) {
	if err := ctx.Err(); err != nil {
		return UploadState{}, err
	}
	if !in.Conflict.valid() {
		// 未知策略必须报错而不是退回默认：拼错的策略名静默按默认执行，
		// 用户以为自己在"跳过"，实际写了盘。
		return UploadState{}, fmt.Errorf("%w: 未知的冲突策略 %q", ErrBadPath, in.Conflict)
	}
	id, err := cleanUploadID(in.ID)
	if err != nil {
		return UploadState{}, err
	}
	name, err := cleanUploadName(in.Name)
	if err != nil {
		return UploadState{}, err
	}
	dir, err := AbsClean(in.Dir)
	if err != nil {
		return UploadState{}, err
	}
	st, err := s.statTargetDir(ctx, dir)
	if err != nil {
		return UploadState{}, err
	}
	_ = st
	if in.Size < 0 {
		return UploadState{}, fmt.Errorf("%w: 负的文件大小 %d", ErrBadPath, in.Size)
	}
	if in.Size > s.maxUpload {
		return UploadState{}, fmt.Errorf("%w: 上限 %s，更大的文件请用 SFTP/scp 传输",
			ErrTooLarge, humanBytes(s.maxUpload))
	}
	chunk := in.ChunkSize
	if chunk <= 0 {
		chunk = DefaultChunkSize
	}
	if chunk > s.maxChunk {
		// 夹到上限而不是报错：客户端只是选了个更大的分块，服务端按自己能
		// 接受的粒度回一个数，客户端据此重切即可。报错则用户要自己猜多大能用。
		chunk = s.maxChunk
	}

	uploadDir := filepath.Join(s.uploadRoot, id)
	existing, err := s.loadMeta(uploadDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return UploadState{}, err
	}
	if existing != nil {
		if err := sameIntent(*existing, dir, name, in.Size, chunk); err != nil {
			return UploadState{}, err
		}
		// 幂等：如实回当前进度，不重置任何东西。
		return s.state(ctx, *existing, uploadDir)
	}

	meta := sessionMeta{
		ID: id, Dir: dir, Name: name, Size: in.Size, Chunk: chunk,
		Conflict: in.Conflict, Created: s.clock(), Updated: s.clock(),
	}
	// 跳过策略在 Begin 就了断：设计 8.2 的"跳过"是为了别浪费带宽，
	// 传完 1GB 再跳过等于什么都没省下。
	if in.Conflict == ConflictSkip {
		if exists, err := targetExists(dir, name); err != nil {
			return UploadState{}, err
		} else if exists {
			return UploadState{
				ID: id, Name: name, Dir: dir, Path: filepath.Join(dir, name),
				Size: in.Size, ChunkSize: chunk, Skipped: true, Done: true,
			}, nil
		}
	}
	// 未表态 + 已存在 = ErrExists（见 ConflictAsk 的注释）。
	if in.Conflict == ConflictAsk {
		if exists, err := targetExists(dir, name); err != nil {
			return UploadState{}, err
		} else if exists {
			return UploadState{}, fmt.Errorf("%w: %s", ErrExists, filepath.Join(dir, name))
		}
	}
	if err := s.saveMeta(uploadDir, meta); err != nil {
		return UploadState{}, err
	}
	// 0 字节文件没有任何块可传，Begin 之后直接 Done。
	// 空文件是真实场景（touch 的占位、被 truncate 的日志）：如果把
	// "完成"定义成"收过至少一块"，它永远完不成，而进度条已经 100%。
	if in.Size == 0 {
		// finish 会删掉整个暂存目录，所以之后再查状态必然是 404 ——
		// 直接如实回一个完成态。
		target, err := s.resolveTarget(meta)
		if err != nil {
			return UploadState{}, err
		}
		s.uploadMu.Lock()
		err = s.finish(ctx, meta, uploadDir, nil)
		s.uploadMu.Unlock()
		if err != nil {
			return UploadState{}, err
		}
		return UploadState{
			ID: id, Name: name, Dir: dir, Path: target,
			Size: 0, ChunkSize: chunk, Done: true,
		}, nil
	}
	return s.state(ctx, meta, uploadDir)
}

// PutChunk 收一个分块。
func (s *Service) PutChunk(ctx context.Context, c UploadChunk) (UploadState, error) {
	id, err := cleanUploadID(c.ID)
	if err != nil {
		return UploadState{}, err
	}
	uploadDir := filepath.Join(s.uploadRoot, id)
	meta, err := s.loadMeta(uploadDir)
	if err != nil {
		return UploadState{}, err
	}
	if meta.Done {
		// 迟到的重复块（重发的最后一块）直接回完成态。写下去的话会在
		// 已完成的暂存里重新落一个 chunk 文件 —— 没有谁会说它是垃圾，
		// 它要等一个 TTL 才被扫掉。
		st, err := s.state(ctx, *meta, uploadDir)
		if err != nil {
			return UploadState{}, err
		}
		return st, nil
	}
	n := chunkCount(meta.Size, meta.Chunk)
	if c.Index < 0 || c.Index >= n {
		return UploadState{}, fmt.Errorf("%w: %d（共 %d 块）", ErrChunkIndex, c.Index, n)
	}
	want := int64(chunkLen(meta.Size, meta.Chunk, c.Index))
	have, err := s.writeChunk(ctx, uploadDir, meta.ID, c.Index, want, c.Body)
	if err != nil {
		return UploadState{}, err
	}
	if have != want {
		return UploadState{}, fmt.Errorf("%w: 第 %d 块应为 %d 字节，实收 %d 字节",
			ErrChunkLength, c.Index, want, have)
	}
	// 元信息只在成功之后更新：半途失败的块如果已经记进状态，续传会
	// 永远认为它到位了，最后拼出一个缺段的文件还报成功。
	s.uploadMu.Lock()
	meta.Updated = s.clock()
	err = s.saveMeta(uploadDir, *meta)
	s.uploadMu.Unlock()
	if err != nil {
		return UploadState{}, err
	}

	// "全齐了就收尾"必须在锁内**重新判定一次**：最后几块是并发到达的，
	// 锁外看到的进度可能已被别的请求推进甚至收尾完成。少了这次重算，
	// 两个请求会各自装配一遍 —— 第二个读到的是赢家刚删掉的分块，
	// 报一个"文件不存在"给一次其实成功的上传。
	s.uploadMu.Lock()
	st, err := s.state(ctx, *meta, uploadDir)
	if err != nil {
		s.uploadMu.Unlock()
		return UploadState{}, err
	}
	if st.Done || len(st.Missing) != 0 {
		s.uploadMu.Unlock()
		return st, nil
	}
	err = s.finish(ctx, *meta, uploadDir, &st)
	s.uploadMu.Unlock()
	if err != nil {
		// 收尾失败必须传出去：吞掉它的话客户端拿到一个 Done=false、
		// Path="" 的"成功"响应，而界面上进度条已经 100%。
		return UploadState{}, err
	}
	return st, nil
}

// UploadStatus 返回会话进度。
func (s *Service) UploadStatus(ctx context.Context, id string) (UploadState, error) {
	cid, err := cleanUploadID(id)
	if err != nil {
		return UploadState{}, err
	}
	uploadDir := filepath.Join(s.uploadRoot, cid)
	meta, err := s.loadMeta(uploadDir)
	if err != nil {
		return UploadState{}, err
	}
	return s.state(ctx, *meta, uploadDir)
}

// AbortUpload 丢掉一个会话及其全部暂存。
func (s *Service) AbortUpload(ctx context.Context, id string) error {
	cid, err := cleanUploadID(id)
	if err != nil {
		return err
	}
	uploadDir := filepath.Join(s.uploadRoot, cid)
	meta, err := s.loadMeta(uploadDir)
	if err != nil {
		return err
	}
	if meta.Done {
		// 撤销不了的已经就位：文件已经在用户目录里，删掉记录只会让用户
		// 以为"取消成功"而磁盘上多一个文件。明确拒绝，前端提示"已完成，
		// 如需删除请用删除功能"。
		return fmt.Errorf("%w，取消不了（用删除功能移除 %s）", ErrUploadDone, meta.Path)
	}
	if err := os.RemoveAll(uploadDir); err != nil {
		return fmt.Errorf("清理上传暂存 %s: %w", uploadDir, err)
	}
	// 上一次 finish 崩在半路的话，目标目录里还有个隐藏临时名；这里
	// 一并带走，否则它会比会话活得久。
	_ = os.Remove(partPath(*meta))
	return nil
}

// GCUploads 清掉闲置超过 TTL 的会话。
//
// 上传中途关浏览器是常态而非常态之外的意外：没有这一条，面板会在
// 别人的磁盘上攒一堆看不见的 5MB 文件，而且它们落在面板自己的暂存根
// 里，用户找不到也删不掉（面板重启也认不出哪些是垃圾）。
//
// 判定用 Updated（最后一次**成功收到块**的时间）而不是 Created：一次
// 慢速上传在慢链接里本来就要几十分钟，按开始时间算等于"上传越慢越
// 容易被杀"。
func (s *Service) GCUploads(ctx context.Context) {
	if s.uploadRoot == "" {
		return
	}
	es, err := os.ReadDir(s.uploadRoot)
	if err != nil {
		return
	}
	now := s.clock()
	for _, e := range es {
		if ctx.Err() != nil {
			return
		}
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(s.uploadRoot, e.Name())
		meta, _ := s.loadMeta(path)
		if meta == nil {
			// 认不出来的目录（没有元信息、元信息写坏、建目录后立刻崩）
			// 同样要清：留着没人负责，而暂存根是面板自己的地盘。没有
			// 元信息就只能拿目录自身的 mtime 当"最后活动时间"。
			if fi, ierr := e.Info(); ierr == nil && now.Sub(fi.ModTime()) > s.uploadTTL {
				_ = os.RemoveAll(path)
			}
			continue
		}
		// 完成记录与未完成会话同样按 TTL 清：它的价值只在"刚传完那几秒
		// 内给并发/轮询的请求一个答案"，留久了就是设计里说的僵尸记录。
		if now.Sub(meta.Updated) > s.uploadTTL {
			_ = os.RemoveAll(path)
		}
	}
}

// SweepStale 是启动时的清扫：面板重启后内存里的会话没了，暂存里
// 超时的那批必须清掉（没超时的留着让用户续传 —— 升级/重启是常态）。
func (s *Service) SweepStale(ctx context.Context) error {
	if s.uploadRoot == "" {
		return nil
	}
	es, err := os.ReadDir(s.uploadRoot)
	if err != nil {
		return nil
	}
	// 重启之后不存在"正在进行"的 finish：meta 记着的目录里凡是本会话
	// 命名的 lp-part 都是上次崩溃留下的尸体，无论会话有没有超时都要清。
	// （放在 GCUploads 之前：先清尸体，再把整个过期的会话连 meta 拿掉。）
	for _, e := range es {
		if !e.IsDir() {
			continue
		}
		if meta, _ := s.loadMeta(filepath.Join(s.uploadRoot, e.Name())); meta != nil {
			_ = os.Remove(partPath(*meta))
		}
	}
	s.GCUploads(ctx)
	return nil
}

// ---------- 内部 ----------

// writeChunk 把一个分块落成独立文件，返回实收字节数。
//
// 三步（临时名 → Sync → Rename）不是仪式感：半途失败的块必须能被
// 认出来并丢弃。如果直接把块写成最终名字，一个被磁盘满截断的块在
// 下次列举时就是"已收到"，续传永远不会补它 —— 得到的是一个缺段的
// 文件而上传报告成功。
//
// fsync 在 rename 之前：目标机的供电/重启不可信，没有它掉电后会留下
// 一个名字在、内容全零的块，而那正是上面那个 bug 的磁盘版本。
func (s *Service) writeChunk(ctx context.Context, uploadDir, id string, index int, want int64, body io.Reader) (int64, error) {
	if err := os.MkdirAll(uploadDir, 0o700); err != nil {
		return 0, fmt.Errorf("创建上传暂存目录 %s: %w", uploadDir, err)
	}
	final := chunkPath(uploadDir, index)
	// 同块号重复上传：内容相同则幂等（弱网重发是常态，报错等于整次
	// 上传前功尽弃），内容不同则是客户端 bug 或会话错乱（两个文件复用
	// 了同一个 id），静默接受会产出"两个文件各一半"的产物。
	if old, err := os.ReadFile(final); err == nil {
		if int64(len(old)) != want {
			return 0, fmt.Errorf("%w: 第 %d 块已收 %d 字节，本次 %d 字节", ErrChunkMismatch, index, len(old), want)
		}
		buf, err := readCapped(ctx, body, s.maxChunk)
		if err != nil {
			return 0, err
		}
		if int64(len(buf)) != want {
			return 0, fmt.Errorf("%w: 第 %d 块应为 %d 字节，实收 %d 字节",
				ErrChunkLength, index, want, len(buf))
		}
		// 长度对上之后只剩一种分歧：内容。同块号不同内容只能来自客户端
		// bug 或会话错乱（两个文件复用了同一个 id）—— 静默接受会得到
		// "前半来自 A、后半来自 B"的文件，而它报告成功。
		if string(buf) != string(old) {
			return 0, fmt.Errorf("%w: 第 %d 块内容与已收到的不一致", ErrChunkMismatch, index)
		}
		return int64(len(buf)), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("读已存在的分块 %s: %w", final, err)
	}

	tmp := filepath.Join(uploadDir, tmpChunkName(index))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("创建分块文件 %s: %w", tmp, err)
	}
	n, werr := copyCapped(ctx, f, body, s.maxChunk)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return n, werr
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return n, fmt.Errorf("分块就位 %s: %w", final, err)
	}
	return n, nil
}

// finish 按序号拼接分块并放到目标位置，然后清掉暂存。
//
// 顺序拼（而不是并发读 204 个文件）是有意的：瓶颈在磁盘顺序写，
// 并发只会把顺序 IO 打散。
//
// 目标名在这里**重新判定一次**冲突策略：上传 1GB 要几十秒，这期间
// 同名文件出现是真实竞争（另一个标签页、用户自己在终端里 cp）。
// Ask 时报错并**保留暂存**：用户处理完冲突还能直接收尾，不必重传 1GB。
// 调用方必须已持有 s.uploadMu（sync.Mutex 不可重入，finish 内部再锁
// 就是自锁死）。
func (s *Service) finish(ctx context.Context, meta sessionMeta, uploadDir string, st *UploadState) error {
	target, err := s.resolveTarget(meta)
	if err != nil {
		return err
	}
	// 装配直接写进**目标目录里的隐藏临时名**，不是最终名，也不是暂存
	// 目录：
	//  - 不写最终名：面板在装配最后几秒被升级/重启打断时，旧写法会在
	//    用户目录留下一个"最终名字 + 截断内容"的文件 —— 它会看起来
	//    像一次成功的上传。临时名 + 就位动作把这类损坏变成显眼的残留
	//    （由 SweepStale 兜底清理）。
	//  - 不写暂存目录再跨目录搬：暂存根与目标几乎必然不在同一个挂载点
	//    （/DATA 上的面板 vs /DISK 上的仓库），跨设备 rename 会退化成
	//    一次无声的全量拷贝，等于白写两遍。
	part := partPath(meta)
	if err := assemble(ctx, uploadDir, meta.Size, meta.Chunk, part); err != nil {
		_ = os.Remove(part)
		return err
	}
	// 就位分两种语义，两者都必须是**原子**的：
	//  - 覆盖策略：rename 直接替换。同目录内的 rename 是原子的，观察者
	//    要么看到旧文件要么看到新文件，永远不会看到半个 —— 这正是
	//    "直接往最终名写"做不到的。
	//  - 其余策略：renameat2(RENAME_NOREPLACE)。目标已存在时内核拒绝，
	//    于是"不存在才创建"由文件系统保证，没有检查-再写的窗口。
	//
	// 为什么不用 os.Link（它同样在目标存在时失败）：**Android 的
	// sdcardfs/FUSE 与部分挂载策略禁止 hardlink**，实测在 /data、
	// /tmp、/storage 三处全部 EACCES —— 本机跑不过去，目标机上也不该赌。
	// 为什么不用 O_EXCL：它保护的是 open，而 open 之后还要写几十秒，
	// 真正的原子点是"就位"这一步。
	if err := place(part, target, meta.Conflict == ConflictOverwrite); err != nil {
		_ = os.Remove(part)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, target)
		}
		return err
	}
	// 顺序是刻意的：先落最终文件（原子的），再写完成记录，最后删分块。
	// 反过来的任何一步崩溃都只会留下"还能重做收尾"的状态，而不会让
	// 一次已经成功的上传查不到结果。
	// 不锁：调用方持有 uploadMu（见函数注释）。
	meta.Done = true
	meta.Path = target
	meta.Updated = s.clock()
	merr := s.saveMeta(uploadDir, meta)
	if merr != nil {
		// 文件已经就位。这里报错会让一次成功的上传显示为失败，而重试
		// 反而会因为目标已存在而 409 —— 报"失败"比重试更糟。
		logx.Info("上传完成记录写入失败 %s: %v（文件已就位 %s）", uploadDir, merr, target)
	}
	if err := s.dropChunks(uploadDir); err != nil {
		logx.Info("上传分块清理失败 %s: %v（文件已就位 %s）", uploadDir, err, target)
	}
	if st != nil {
		st.Path = target
		st.Done = true
	}
	return nil
}

// dropChunks 只删分块与装配产物，留下 meta.json 作为完成记录。
func (s *Service) dropChunks(uploadDir string) error {
	es, err := os.ReadDir(uploadDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range es {
		if e.IsDir() || e.Name() == metaFile {
			continue
		}
		if err := os.Remove(filepath.Join(uploadDir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// place 把装配好的临时名放到最终名字上。
//
// noReplace=true 时走 renameat2(RENAME_NOREPLACE)：目标已存在则失败，
// 内核保证原子。它需要一次裸系统调用（Go 标准库没有封装 x/sys 之外的
// 入口），这是本包里唯一一处 syscall —— 因为"不覆盖别人的文件"这件事
// 没法用任何用户态组合安全地做到。
func place(part, target string, overwrite bool) error {
	if overwrite {
		if err := os.Rename(part, target); err != nil {
			return fmt.Errorf("覆盖就位 %s: %w", target, err)
		}
		return nil
	}
	if err := unix.Renameat2(unix.AT_FDCWD, part, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("%w: 就位时目标已存在", os.ErrExist)
		}
		// EBUSY/EXDEV/EPERM：挂载点、只读、权限。原样带出去，不猜原因。
		return fmt.Errorf("就位 %s: %w", target, err)
	}
	return nil
}

// partPath 是装配期间在**目标目录**里的临时名。
//
// 以 upload_id 命名而不是"目标名.part"：(1) 不跟随用户文件名，长度
// 有界（id 已夹到 64 字节），不会把 255 字节的名撑爆；(2) 前导点让它
// 在列表里默认不可见；(3) SweepStale 拿 meta 里的 Dir+ID 就能精确算出
// 路径，不需要通配扫描。
func partPath(meta sessionMeta) string {
	return filepath.Join(meta.Dir, "."+meta.ID+".lp-part")
}

// resolveTarget 按策略决定最终路径。
func (s *Service) resolveTarget(meta sessionMeta) (string, error) {
	base := filepath.Join(meta.Dir, meta.Name)
	switch meta.Conflict {
	case ConflictOverwrite, ConflictAsk:
		// Ask 在 Begin 已经查过；这里不再查，交给 O_EXCL 兜住竞争。
		return base, nil
	case ConflictSkip:
		return base, nil
	case ConflictRename:
		return conflictName(base)
	default:
		return "", fmt.Errorf("%w: 未知的冲突策略 %q", ErrBadPath, meta.Conflict)
	}
}

// conflictName 生成 name (1).ext 形式的不冲突名字。
//
// 后缀插在**扩展名之前**：a.bin (1) 在 Windows 上双击会问"用什么应用
// 打开"，等于传完变成一个打不开的文件。
//
// 点文件要单独处理：filepath.Ext(".bashrc") 返回 ".bashrc"（最后一个
// 点就在第 0 位），照 stem+ext 套会得到 " .bashrc (1)" 之类的前导空格
// 名，或者把点弄丢 —— 而"点开头"正是它隐藏的原因，改名不该改变可见性。
func conflictName(base string) (string, error) {
	// 先试原名：策略叫"冲突时改名"，不是"一律加后缀"。少了这一步，
	// 上传一个全新的文件也会得到 a (1).bin，而用户会以为面板把名字
	// 改错了。
	if _, err := os.Lstat(base); errors.Is(err, fs.ErrNotExist) {
		return base, nil
	} else if err != nil {
		return "", fmt.Errorf("检查目标 %s: %w", base, err)
	}
	dir, leaf := filepath.Split(base)
	for i := 1; ; i++ {
		var cand string
		if strings.HasPrefix(leaf, ".") {
			cand = dir + fmt.Sprintf("%s (%d)", leaf, i)
		} else {
			ext := filepath.Ext(leaf)
			stem := strings.TrimSuffix(leaf, ext)
			cand = dir + fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		if _, err := os.Lstat(cand); errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		} else if err != nil {
			return "", fmt.Errorf("检查候选名 %s: %w", cand, err)
		}
		if i > 10000 {
			return "", fmt.Errorf("%w: %s 的一万个候选名全被占用", ErrExists, base)
		}
	}
}

// assemble 把分块按序号拼进 out。
func assemble(ctx context.Context, uploadDir string, size, chunk int64, out string) error {
	dst, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("创建装配文件 %s: %w", out, err)
	}
	n := chunkCount(size, chunk)
	var written int64
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			dst.Close()
			return ctx.Err()
		}
		p := chunkPath(uploadDir, i)
		b, err := os.ReadFile(p)
		if err != nil {
			dst.Close()
			return fmt.Errorf("读分块 %s: %w", p, err)
		}
		if got, want := int64(len(b)), chunkLen(size, chunk, i); got != want {
			dst.Close()
			return fmt.Errorf("分块 %s 长度 %d 与声明的 %d 不符", p, got, want)
		}
		if _, err := dst.Write(b); err != nil {
			dst.Close()
			return fmt.Errorf("装配写入: %w", err)
		}
		written += int64(len(b))
	}
	if written != size {
		dst.Close()
		return fmt.Errorf("装配后 %d 字节，声明 %d 字节", written, size)
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return fmt.Errorf("装配落盘: %w", err)
	}
	return dst.Close()
}

// state 从磁盘重建会话状态。磁盘是唯一真相（见包注释 2）。
func (s *Service) state(ctx context.Context, meta sessionMeta, uploadDir string) (UploadState, error) {
	if err := ctx.Err(); err != nil {
		return UploadState{}, err
	}
	if meta.Done {
		// 分块文件已经删了，用磁盘重算会得到"一块都没收"。完成与否是
		// 一个事实，不是从残留文件推出来的。
		return UploadState{
			ID: meta.ID, Name: meta.Name, Dir: meta.Dir, Path: meta.Path,
			Size: meta.Size, ChunkSize: meta.Chunk,
			Received: meta.Size, Done: true,
		}, nil
	}
	n := chunkCount(meta.Size, meta.Chunk)
	got := make(map[int]int64, n)
	es, err := os.ReadDir(uploadDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return UploadState{}, fmt.Errorf("列举暂存 %s: %w", uploadDir, err)
	}
	for _, e := range es {
		if e.IsDir() || e.Name() == metaFile {
			continue
		}
		name := e.Name()
		idx, ok := parseChunkName(name)
		if !ok {
			continue // .tmp 之类的半途文件，不算已收到
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		got[idx] = fi.Size()
	}
	var received int64
	var missing []int
	for i := 0; i < n; i++ {
		want := chunkLen(meta.Size, meta.Chunk, i)
		if sz, ok := got[i]; ok && sz == want {
			received += sz
			continue
		}
		missing = append(missing, i)
	}
	// Done 恒为 false 从这里返回：调用方（PutChunk）看到 Missing 为空
	// 才会去 finish，而 finish 成功之后暂存就没了。状态查询本身永远
	// 不该报告"完成" —— 报告完成却没人把文件放到目标位置，是客户端
	// 唯一无法自救的错误形状。
	return UploadState{
		ID: meta.ID, Name: meta.Name, Dir: meta.Dir,
		Size: meta.Size, ChunkSize: meta.Chunk,
		Received: received, Missing: missing,
	}, nil
}

// ---------- 元信息 ----------

func (s *Service) loadMeta(uploadDir string) (*sessionMeta, error) {
	b, err := os.ReadFile(filepath.Join(uploadDir, metaFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: 上传会话 %s", fs.ErrNotExist, filepath.Base(uploadDir))
		}
		return nil, fmt.Errorf("读上传元信息 %s: %w", uploadDir, err)
	}
	var m sessionMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("解析上传元信息 %s: %w", uploadDir, err)
	}
	return &m, nil
}

func (s *Service) saveMeta(uploadDir string, m sessionMeta) error {
	if err := os.MkdirAll(uploadDir, 0o700); err != nil {
		return fmt.Errorf("创建上传暂存目录 %s: %w", uploadDir, err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("编码上传元信息: %w", err)
	}
	tmp := filepath.Join(uploadDir, metaFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写上传元信息: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(uploadDir, metaFile)); err != nil {
		return fmt.Errorf("上传元信息就位: %w", err)
	}
	return nil
}

// ---------- 纯函数与小工具 ----------

// chunkCount 是 size 字节按 chunk 大小切成的块数（向上取整）。
func chunkCount(size, chunk int64) int {
	if size <= 0 {
		return 0
	}
	n := size / chunk
	if size%chunk != 0 {
		n++
	}
	return int(n)
}

// chunkLen 是第 i 块应有的长度（末块可能短）。
func chunkLen(size, chunk int64, i int) int64 {
	start := int64(i) * chunk
	if size-start < chunk {
		return size - start
	}
	return chunk
}

// chunkPath 是分块的最终名字。
//
// 序号补零到 6 位：这样目录列举出来的字典序就是序号序，装配时不必
// 排序（超过 100 万块会破序，但 1GB ÷ 5MB = 204，远不到）。
func chunkPath(dir string, i int) string {
	return filepath.Join(dir, fmt.Sprintf("%06d.chunk", i))
}

func tmpChunkName(i int) string {
	return fmt.Sprintf("%06d.chunk.tmp", i)
}

// parseChunkName 认出"最终名"的分块并把序号取回来。
func parseChunkName(name string) (int, bool) {
	if !strings.HasSuffix(name, ".chunk") {
		return 0, false
	}
	digits := strings.TrimSuffix(name, ".chunk")
	if len(digits) == 0 {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}

// cleanUploadID 校验 upload_id。
//
// id 会被拼进暂存路径，所以它是一条真实的注入面："../evil" 能把分块
// 写到暂存根之外，"a/b" 能穿过去碰别人的目录。字符集夹到 [A-Za-z0-9_-]
// 是这里唯一可靠的防线 —— 用 filepath.Clean 之后再比一次那种做法挡不住
// 绝对路径（Clean("/x") == "/x"，看着合法，拼起来就跑了）。
func cleanUploadID(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: upload_id 为空", ErrBadPath)
	}
	if len(id) > maxUploadIDLen {
		return "", fmt.Errorf("%w: upload_id 超过 %d 字节", ErrBadPath, maxUploadIDLen)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", fmt.Errorf("%w: upload_id 含非法字符 %q", ErrBadPath, r)
		}
	}
	return id, nil
}

// cleanUploadName 校验目标文件名。
//
// 分隔符必须挡住：允许 "/" 就等于允许"上传到目录框之外的地方"——
// 地址栏显示 /var/log 而文件落到 /etc，是那种"面板是不是被人打了"的
// 现场。
func cleanUploadName(name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: 文件名 %q", ErrBadPath, name)
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("%w: 文件名不能含路径分隔符 %q", ErrBadPath, name)
	}
	if name != strings.TrimSpace(name) && strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: 文件名全是空白 %q", ErrBadPath, name)
	}
	if len(name) > 255 {
		return "", fmt.Errorf("%w: 文件名超过 255 字节", ErrBadPath)
	}
	return name, nil
}

// targetExists 看目标是否已被占用。
//
// 用 Lstat 而不是 Stat：指向不存在目标（或目录）的符号链接也算"已
// 占用"，因为写它会改到链接本身或跑到目录里去。
func targetExists(dir, name string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, name))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("检查目标 %s: %w", filepath.Join(dir, name), err)
}

// sameIntent 比较两次 Begin 的参数是否描述同一个上传。
func sameIntent(m sessionMeta, dir, name string, size, chunk int64) error {
	if m.Dir != dir {
		return fmt.Errorf("%w: upload_id 已用于目录 %s，不能改成 %s", ErrBadPath, m.Dir, dir)
	}
	if m.Name != name {
		return fmt.Errorf("%w: upload_id 已用于文件 %s，不能改成 %s", ErrBadPath, m.Name, name)
	}
	if m.Size != size {
		return fmt.Errorf("%w: upload_id 已声明大小 %d，不能改成 %d", ErrBadPath, m.Size, size)
	}
	if m.Chunk != chunk {
		// 块大小变了意味着所有已收块的边界作废（第 3 块在旧大小下是
		// 15MB 起，在新大小下是 6MB 起）。报错比悄悄重排诚实。
		return fmt.Errorf("%w: upload_id 已用块大小 %d 上传，不能改成 %d", ErrBadPath, m.Chunk, chunk)
	}
	return nil
}

// statTargetDir 确认目标目录可当目录用。三种错各有独立语义：
// 不存在 → fs.ErrNotExist(404)、是文件 → ErrNotDirectory(400)、
// 路径本身写错 → ErrBadPath(400)。
func (s *Service) statTargetDir(ctx context.Context, dir string) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrNotDirectory, dir)
	}
	return fi, nil
}

// copyCapped 拷贝但不超过 cap 字节；超出立刻 ErrChunkTooLarge。
//
// 上限必须是服务端定的：请求体多大由客户端说了算，没有这一条的话
// 一个请求就能把盘写穿，而未鉴权的攻击者也能靠它做拒绝服务。
func copyCapped(ctx context.Context, dst io.Writer, src io.Reader, max int64) (int64, error) {
	// 多读 1 字节：只有"确实超过了"才报错，正好等于上限的块要能过。
	n, err := io.Copy(dst, io.LimitReader(src, max+1))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	if n > max {
		return n, fmt.Errorf("%w: %d 字节 > 上限 %d 字节", ErrChunkTooLarge, n, max)
	}
	if err != nil {
		return n, err
	}
	return n, nil
}

// readCapped 读满一个请求体，超过 cap 立刻报错。
//
// 上限先于内容检查：这里读进来的东西直接进内存，而未鉴权的一端可以
// 声明任何长度。
func readCapped(ctx context.Context, src io.Reader, max int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(src, max+1))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return buf, ctxErr
	}
	if int64(len(buf)) > max {
		return buf, fmt.Errorf("%w: %d 字节 > 上限 %d 字节", ErrChunkTooLarge, len(buf), max)
	}
	return buf, err
}

func humanBytes(n int64) string {
	const m = 1 << 20
	if n >= 1<<30 && n%(1<<30) == 0 {
		return fmt.Sprintf("%dGB", n/(1<<30))
	}
	if n >= m {
		return fmt.Sprintf("%dMB", n/m)
	}
	return fmt.Sprintf("%dB", n)
}

var _ = sort.Ints
