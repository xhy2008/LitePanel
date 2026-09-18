//go:build debug

package logx

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Enabled 为 true：调用真实输出。debug 构建本来就只用于排障，
// 多花的那点 CPU/内存正是它的存在意义。
const Enabled = true

var (
	mu    sync.Mutex
	out   io.Writer = os.Stderr
	level           = LevelDebug
)

// Init 指定输出去向（测试注入 bytes.Buffer；main 用 stderr）。
func Init(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	if w != nil {
		out = w
	}
}

// SetLevel 调整最低输出级别。
func SetLevel(l Level) {
	mu.Lock()
	defer mu.Unlock()
	level = l
}

func emit(lvl string, l Level, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if l < level {
		return
	}
	// 不用 log 包：它自带 goroutine 前缀格式化开销，这里一行 "时间 级别 正文" 足够。
	fmt.Fprintf(out, "%s %-5s %s\n",
		time.Now().Format("15:04:05.000"), lvl, fmt.Sprintf(format, args...))
}

func Debug(format string, args ...any) { emit("DEBUG", LevelDebug, format, args...) }
func Info(format string, args ...any)  { emit("INFO", LevelInfo, format, args...) }
func Warn(format string, args ...any)  { emit("WARN", LevelWarn, format, args...) }
func Error(format string, args ...any) { emit("ERROR", LevelError, format, args...) }
