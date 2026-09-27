package filemgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 10 万目录的性能夹具（验收条款）。
//
// 默认跳过：本机构建 10 万个文件要 5~6 秒，而且会吃掉几百 MB 的目录项
// inode。要跑就显式打开：
//
//	LP_BENCH_DIR=/data/lpbench go test ./internal/filemgr -run '^$' -bench 100k -benchtime 1x
//
// 不指向临时目录是有意的：tmpfs 上的 readdir 测不出真实磁盘（HDD）的
// 元数据延迟，而这条指标存在的理由就是"机械盘上翻大目录会不会卡死"。
func benchDir100k(b *testing.B) string {
	b.Helper()
	dir := os.Getenv("LP_BENCH_DIR")
	if dir == "" {
		b.Skip("需要 LP_BENCH_DIR 指向一块真实磁盘上的可写目录")
	}
	root := filepath.Join(dir, "bench100k")
	if _, err := os.Stat(root); err == nil {
		return root // 复用上一次构建的夹具（10 万文件的创建成本比列举本身高）
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		b.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 100000; i++ {
		name := fmt.Sprintf("f%05d_%s.txt", i, []string{"go", "md", "log", "png", ""}[i%5])
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.Logf("夹具构建耗时 %v", time.Since(start))
	return root
}

func BenchmarkList100kName(b *testing.B) {
	dir := benchDir100k(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := List(ctx, dir, ListOptions{Sort: SortName})
		if err != nil {
			b.Fatal(err)
		}
		if p.Total != 100000 || len(p.Entries) != DefaultPageSize {
			b.Fatalf("total=%d n=%d", p.Total, len(p.Entries))
		}
	}
}

func BenchmarkList100kSize(b *testing.B) {
	dir := benchDir100k(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := List(ctx, dir, ListOptions{Sort: SortSize}); err != nil {
			b.Fatal(err)
		}
	}
}
