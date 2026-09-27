//go:build debug

package filemgr

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"litepanel/internal/logx"
)

// "读不了的目录被跳过"这件事的唯一记录就是那行日志（那条路径返回
// nil）。这里把 logx 接到 buffer 上，把它变成可断言的事实：
// 删掉 zip.go 里的 logx.Info 后，本文件必须变红。
//
// 只有 debug 构建能看到日志：release 里 logx 挂载点被编译期剥离（D9），
// 所以这条断言按构建标签分家，而不是写成"两种构建都断言日志"——
// 后者在 release 下永远只能断言"什么都没有"，那是恒真断言。
func TestZipLogsSkippedUnreadableDir(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "d", "root专用")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("以 root 跑测试，0o000 夹具无效")
	}

	var buf bytes.Buffer
	logx.Init(&buf)
	t.Cleanup(func() { logx.Init(os.Stderr) }) // 不还原会把后续测试的日志吞进这个 buffer

	var out strings.Builder
	if err := NewService(Options{}).Zip(context.Background(),
		[]string{filepath.Join(dir, "d")}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "root专用") {
		t.Errorf("跳过哪一层必须可查, 日志=%q", buf.String())
	}
}
