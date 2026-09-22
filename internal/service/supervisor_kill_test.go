//go:build !release

// D11 的验收核心：面板被 kill -9（不是优雅退出，没有任何清理机会）时，
// 托管的服务必须被内核的 PDEATHSIG 收走。这条只能起真进程测。
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSIGKILLPanelKillsChildren(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "svchost")
	// go test 的工作目录就是本包目录，testdata/svchost 就在脚下。
	build := exec.Command("go", "build", "-o", bin, "./testdata/svchost")
	if out, err := build.CombinedOutput(); err != nil {
		// 不 skip：skip 会让 D11 在最需要它的时候悄悄失守。
		t.Fatalf("构建测试主机失败: %v\n%s", err, out)
	}

	dir := t.TempDir()
	host := exec.Command(bin, dir)
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = host.Process.Kill()
		_, _ = host.Process.Wait()
	})

	// 主机把面板自己的 pid 与它 fork 出来的子进程 pid 分别写盘。
	self, child := waitForTwoPids(t, filepath.Join(dir, "self"), filepath.Join(dir, "child"), 15*time.Second)
	t.Logf("面板=%d 子进程=%d", self, child)

	if err := host.Process.Kill(); err != nil { // kill -9，面板来不及做任何事
		t.Fatal(err)
	}
	_, _ = host.Process.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for alive(child) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(child) {
		t.Fatalf("面板被 kill -9 后子进程仍在: %d", child)
	}
}

func waitForTwoPids(t *testing.T, a, b string, d time.Duration) (int, int) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		p1, e1 := readInt(a)
		p2, e2 := readInt(b)
		if e1 == nil && e2 == nil && p1 > 0 && p2 > 0 {
			return p1, p2
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等测试主机写 pid 超时")
	return 0, 0
}

func readInt(p string) (int, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	var n int
	_, err = fmt.Sscanf(string(b), "%d", &n)
	return n, err
}
