package main

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// execCommand 抽成变量便于测试注入。
var execCommand = func(name string, args ...string) (string, error) {
	b, err := exec.Command(name, args...).Output()
	return string(b), err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// uploadRootFor 从 db 路径推出分块暂存目录（<db 同级>/uploads）。
//
// 独立成函数是为了让"接的是哪一个目录"能直接被测试断言：内联在
// buildDeps 的字面量里也行，但那样只能靠"上传成功了"间接推出来，
// 而"成功"在一个恰好 cwd 可写的开发机上永远成立。
func uploadRootFor(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "uploads")
}
