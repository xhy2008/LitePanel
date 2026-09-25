package terminal

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 探测的验收要点（设计 7.3）：tmux 不可用时终端页要给出**明确的引导**，
// 而不是静默降级到自研 pty。所以探测结果必须能区分出"为什么不行"——
// 前端要据此决定引导文案。

// fakeTmux 造一个假的 tmux 可执行文件，输出固定内容。
// 用真的可执行文件而不是注入 runner：这样 exec、stderr 合并、版本解析
// 这条真实路径全都被走到，换掉假文件就等于换了 tmux。
func fakeTmux(t *testing.T, output string, exitOK bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("需要 POSIX shell")
	}
	p := filepath.Join(t.TempDir(), "tmux")
	status := "0"
	if !exitOK {
		status = "1"
	}
	body := "#!/bin/sh\nprintf '%s' '" + output + "'\nexit " + status + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHealthReportsVersion(t *testing.T) {
	p := fakeTmux(t, "tmux 3.7c\n", true)
	h := Probe(p)
	if !h.Available {
		t.Fatalf("3.7c 应判定可用，实际 reason=%q", h.Reason)
	}
	if h.Version != "3.7c" {
		t.Fatalf("版本应为 3.7c，得到 %q", h.Version)
	}
	if h.MinVersion == "" {
		t.Error("必须告诉前端版本下限，否则引导文案没法写")
	}
}

func TestHealthFailsOnMissingTmux(t *testing.T) {
	h := Probe(filepath.Join(t.TempDir(), "definitely-not-tmux"))
	if h.Available {
		t.Fatal("不存在的 tmux 不能判定为可用")
	}
	if h.Reason == "" {
		t.Error("必须给出原因，前端要显示安装引导")
	}
}

// TestHealthFailsOnAncientVersion：低于下限必须判不可用。
// 下限取 3.2（不是设计 7.3 早期写的 2.1）：本实现依赖的能力里
// window-size 语义、%begin/%end 的命令编号关联都是 3.x 才有的，
// 而环境探测（R8）确认目标机是 tmux 3.6。
func TestHealthFailsOnAncientVersion(t *testing.T) {
	for _, v := range []string{"tmux 1.8", "tmux 2.1", "tmux 3.1a", "tmux 2.7"} {
		p := fakeTmux(t, v+"\n", true)
		h := Probe(p)
		if h.Available {
			t.Errorf("%s 应判不可用（下限 %s）", v, h.MinVersion)
		}
	}
}

func TestHealthAcceptsEverySupportedVersion(t *testing.T) {
	for _, v := range []string{"tmux 3.2", "tmux 3.2a", "tmux 3.3b", "tmux 3.6",
		"tmux 3.7c", "tmux 4.0", "tmux next-3.4"} {
		p := fakeTmux(t, v+"\n", true)
		h := Probe(p)
		if !h.Available {
			t.Errorf("%s 应判可用，reason=%q", v, h.Reason)
		}
	}
}

// TestHealthFailsOnUnparseableOutput：tmux 存在但输出不是预期格式（发行版
// 改了 -V 的措辞、或者那个路径根本不是 tmux）必须判不可用。
// 判定方向要保守：**认不出来就当不可用**，因为误判可用会让终端页打开后
// 一片空白，而误判不可用只是多显示一行引导。
func TestHealthFailsOnUnparseableOutput(t *testing.T) {
	for _, out := range []string{"", "tmux\n", "some other tool 9.9", "tmux abc"} {
		p := fakeTmux(t, out, true)
		h := Probe(p)
		if h.Available {
			t.Errorf("输出 %q 不该判为可用", out)
		}
	}
}

// TestHealthFailsOnNonzeroExit：有些发行版的 tmux 在没有 server/socket 权限
// 时 -V 也会失败。
//
// 输出刻意写成**完全正常**的版本号：如果这里用 "no server running" 之类的
// 文案，那条"输出里必须有 tmux 字样"的守卫会顺带把它拦下，于是本测试对
// 退出码检查毫无区分力（变异掉退出码检查后测试照样绿，实测踩过）。
// 让版本号正常，唯一能拦住它的就只剩退出码这一条守卫。
func TestHealthFailsOnNonzeroExit(t *testing.T) {
	p := fakeTmux(t, "tmux 3.7c\n", false)
	h := Probe(p)
	if h.Available {
		t.Fatal("退出码非 0 不能判为可用")
	}
	if h.Reason == "" {
		t.Error("必须带上失败原因")
	}
}

// TestProbeUsesDefaultBinWhenEmpty：生产调用不传路径时用 PATH 里的 tmux，
// 这样 API 层不需要知道这个细节。
func TestProbeUsesDefaultBinWhenEmpty(t *testing.T) {
	h := Probe("")
	if h.Bin != DefaultBin {
		t.Fatalf("空路径应回落到 %q，得到 %q", DefaultBin, h.Bin)
	}
	// 本机（Termux）确实装了 tmux，所以这里必须可用；目标服务器同理。
	// 不写成"可用或不可用都接受"那种恒真断言。
	if !h.Available {
		t.Fatalf("本机装了 tmux 却判不可用: %q", h.Reason)
	}
}
