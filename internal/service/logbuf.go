package service

import (
	"strings"
	"sync"
)

// maxLogLineBytes 是单行上限。二进制垃圾输出不该把预算吃光。
const maxLogLineBytes = 4096

// DefaultLogLines 是 D19 规定的默认保留行数。
const DefaultLogLines = 500

// LogBuf 是单个服务的内存日志环形缓冲。
//
// D19：服务日志只存在于内存 —— 不落盘、不进数据库，面板重启即空。
// 写入方是拷贝子进程输出的 goroutine，读取方是 API 与 WS，必须加锁。
type LogBuf struct {
	mu      sync.Mutex
	lines   []string
	limit   int
	pending string // 还没等到换行符的尾巴
	over    bool   // 当前行已超长，本行余下字节直接丢弃
	hook    func([]string)
}

// NewLogBuf 建一个保留最近 limit 行的缓冲。
func NewLogBuf(limit int) *LogBuf {
	if limit <= 0 {
		limit = DefaultLogLines
	}
	return &LogBuf{limit: limit}
}

// Write 实现 io.Writer，可直接吃子进程 stdout/stderr 的合并流。
func (b *LogBuf) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var done []string

	b.mu.Lock()
	for _, seg := range strings.SplitAfter(string(p), "\n") {
		if seg == "" {
			continue
		}
		if !strings.HasSuffix(seg, "\n") { // 尾巴，等下一块
			b.addPending(seg)
			continue
		}
		line := strings.TrimSuffix(seg, "\n")
		line = strings.TrimSuffix(line, "\r")
		if b.pending != "" {
			line = b.pending + line
			b.pending = ""
		}
		b.over = false
		line = clampLine(line)
		b.lines = append(b.lines, line)
		done = append(done, line)
	}
	b.trim()
	hook := b.hook
	b.mu.Unlock()

	// 回调在解锁后调：hook 里可能反过来读缓冲（WS 转发），持锁调用会自锁。
	if hook != nil && len(done) > 0 {
		hook(done)
	}
	return len(p), nil
}

// addPending 累积未换行的尾巴；本行一旦超长就停止接收余下字节。
func (b *LogBuf) addPending(seg string) {
	if b.over {
		return
	}
	b.pending += seg
	if len(b.pending) > maxLogLineBytes {
		b.pending = b.pending[:maxLogLineBytes]
		b.over = true
	}
}

func clampLine(s string) string {
	if len(s) > maxLogLineBytes {
		return s[:maxLogLineBytes]
	}
	return s
}

func (b *LogBuf) trim() {
	if n := len(b.lines) - b.limit; n > 0 {
		b.lines = append(b.lines[:0:0], b.lines[n:]...)
	}
}

// Tail 返回最近 n 行（含未换行的尾巴），按时间顺序。
// 尾巴不消费：下次读还能看到它，直到它等到换行。
func (b *LogBuf) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	src := b.lines
	if b.pending != "" {
		src = append(append([]string{}, b.lines...), b.pending)
	}
	if n <= 0 || n >= len(src) {
		return append([]string{}, src...)
	}
	return append([]string{}, src[len(src)-n:]...)
}

// Len 返回已结束行数（不含尾巴）。
func (b *LogBuf) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.lines)
}

// Limit 返回当前行数上限。
func (b *LogBuf) Limit() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

// Resize 改行数上限（设置页可调）。缩小会立刻丢掉最老的行。
func (b *LogBuf) Resize(limit int) {
	if limit <= 0 {
		limit = DefaultLogLines
	}
	b.mu.Lock()
	b.limit = limit
	b.trim()
	b.mu.Unlock()
}

// Clear 清空（对应 DELETE /api/services/{id}/log）。
func (b *LogBuf) Clear() {
	b.mu.Lock()
	b.lines = nil
	b.pending = ""
	b.over = false
	b.mu.Unlock()
}

// OnAppend 注册新行回调，供 WS 的 svclog:{id} 频道实时转发。
// 只保留最后一个回调；传 nil 取消。
func (b *LogBuf) OnAppend(fn func([]string)) {
	b.mu.Lock()
	b.hook = fn
	b.mu.Unlock()
}
