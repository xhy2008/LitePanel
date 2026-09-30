package filemgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 量一批删除到底要多久。同步端点能不能撑住，取决于这个数字而不是我的推理。
func BenchmarkDeleteMany(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(files(n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				e := newTrashEnv(b)
				dirs := make([]string, 0, 20)
				for d := 0; d < 20; d++ {
					dirs = append(dirs, filepath.Join(e.disk, "目录"+string(rune('a'+d))))
				}
				var paths []string
				for i := 0; i < n; i++ {
					p := filepath.Join(dirs[i%len(dirs)], "文件"+itoa(i)+".txt")
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						b.Fatal(err)
					}
					if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
						b.Fatal(err)
					}
					paths = append(paths, p)
				}
				b.StartTimer()
				if _, err := e.svc.DeleteMany(context.Background(), paths, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// 同样的批量，永久删除（不建条目、不写 meta）作对照。
func BenchmarkDeleteManyPermanent(b *testing.B) {
	for _, n := range []int{1000, 5000} {
		b.Run(files(n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				e := newTrashEnv(b)
				var paths []string
				for i := 0; i < n; i++ {
					p := filepath.Join(e.disk, "d", "f"+itoa(i)+".txt")
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						b.Fatal(err)
					}
					if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
						b.Fatal(err)
					}
					paths = append(paths, p)
				}
				b.StartTimer()
				if _, err := e.svc.DeleteMany(context.Background(), paths, true); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func files(n int) string { return itoa(n) + "个文件" }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
