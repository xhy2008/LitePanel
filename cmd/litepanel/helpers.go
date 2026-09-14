package main

import (
	"os/exec"
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
