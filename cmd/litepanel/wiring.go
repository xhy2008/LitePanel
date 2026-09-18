package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"litepanel/internal/logx"
)

// wireLogging 是 main 的日志接线，两种构建共用同一份代码：
//   - 发布构建：logx.Enabled=false 是构建期常量，debug 分支被编译器
//     整段消除，log.SetOutput(io.Discard) 无条件执行（D9：发布构建
//     零运行日志；R4 例外见 stderrNote）；
//   - 调试构建：标准 log 与 logx 都走 stderr，-debug 决定详细程度。
//
// 单独成函数是为了可测（wiring_release_test.go 直接断言 log.Writer()）。
func wireLogging(debug bool) {
	if logx.Enabled {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags | log.Lmsgprefix)
		log.SetPrefix("litepanel: ")
		logx.Init(os.Stderr)
		if !debug {
			// 调试构建没带 -debug：只留 WARN 以上，面板保持安静。
			logx.SetLevel(logx.LevelWarn)
		}
		return
	}
	log.SetOutput(io.Discard)
}

// stderrNote 打印 R4 例外里"非致命"的那类：目前只有初始密码。
// 不走 logx（发布构建会被编译掉），也不复用 fatal（那个要退出进程）。
func stderrNote(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "litepanel: "+format+"\n", args...)
}
