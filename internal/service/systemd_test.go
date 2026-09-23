package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"litepanel/internal/store"
	"strings"
	"testing"
	"time"
)

// systemd 类型服务的测试全靠一个假的 systemctl 可执行文件：真 systemd 在
// Termux 上不存在，在 CI 上也不该真去动宿主的服务。假脚本把收到的参数
// 逐行写进 $FAKE_LOG，于是"参数传对了没有"变成可断言的事实 —— 这一层
// 走的是真实的 PATH 查找与 exec，所以引号/切分错误也会被抓到。

// fakeSystemctl 在临时目录造一个假 systemctl，并把 PATH 指向它。
// 返回日志文件路径（读取即可得知被调用过哪些命令）。
func fakeSystemctl(t *testing.T, stdout string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		// 先回显参数，再按需输出/退出码。环境变量控制"服务当前状态"。
		"echo \"$@\" >> \"$FAKE_LOG\"\n" +
		"case \"$1\" in\n" +
		"  is-active) echo \"${FAKE_ACTIVE:-inactive}\"; [ \"${FAKE_ACTIVE:-inactive}\" = active ] || exit 3;;\n" +
		"  show) echo \"${FAKE_PID:-0}\";;\n" +
		"  start|stop|restart) printf '%s' \"${FAKE_OUT:-}\"; [ -n \"${FAKE_FAIL:-}\" ] && { echo \"${FAKE_FAIL}\" >&2; exit 5; };;\n" +
		"esac\n" +
		"exit 0\n"
	bin := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写假 systemctl: %v", err)
	}
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", logPath)
	return logPath
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		// 一次都没被调用过 —— 空字符串比报错更好断言。
		return ""
	}
	return string(b)
}

// addSystemd 走真实 Create：生产中 Start/Stop 收到的 Service 永远来自
// 数据库那一行，凭空造的 Service 会在 SaveState 那里撞上外键。
func addSystemd(t *testing.T, db *store.DB, unit string) Service {
	t.Helper()
	svc, err := Create(db, ServiceInput{
		Name: "nginx", Kind: KindSystemd, Unit: unit,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return svc
}

func TestSystemdStartPassesExactArgs(t *testing.T) {
	logPath := fakeSystemctl(t, "")
	db := openDB(t)
	sup := NewSupervisor(db)
	svc := addSystemd(t, db, "nginx")

	if _, err := sup.Start(svc); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := readCalls(t, logPath)
	if !strings.Contains(calls, "start nginx\n") {
		t.Fatalf("应当调用 `systemctl start nginx`，实际调用:\n%s", calls)
	}
}

// 单元名来自用户输入。若哪天真用 shell 去拼这条命令，
// "nginx; rm -rf /" 就会被执行 —— 所以断言参数是一个整体。
func TestSystemdUnitNameIsNotShellParsed(t *testing.T) {
	logPath := fakeSystemctl(t, "")
	db := openDB(t)
	sup := NewSupervisor(db)

	nasty := "nginx; touch /tmp/should-not-exist"
	if _, err := sup.Start(addSystemd(t, db, nasty)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := readCalls(t, logPath)
	if !strings.Contains(calls, "start "+nasty+"\n") {
		t.Fatalf("单元名应当整串作为单个参数传入，实际:\n%s", calls)
	}
}

func TestSystemdStopPassesExactArgs(t *testing.T) {
	logPath := fakeSystemctl(t, "")
	db := openDB(t)
	sup := NewSupervisor(db)
	svc := addSystemd(t, db, "nginx")

	if _, err := sup.Start(svc); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := sup.Stop(context.Background(), svc, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	calls := readCalls(t, logPath)
	if !strings.Contains(calls, "stop nginx\n") {
		t.Fatalf("应当调用 `systemctl stop nginx`，实际调用:\n%s", calls)
	}
}

// systemd 单元不归面板托管，也就没有"面板重启把服务一起停掉"的副作用。
// 这条断言保护的是行为差异：用户之所以选 SYSV 类型，就是因为不想让
// 重启面板牵连它。
func TestSystemdUnitsSurvivePanelShutdown(t *testing.T) {
	logPath := fakeSystemctl(t, "")
	db := openDB(t)
	sup := NewSupervisor(db)

	if _, err := sup.Start(addSystemd(t, db, "nginx")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	calls := readCalls(t, logPath)
	if strings.Contains(calls, "stop ") {
		t.Fatalf("面板关停不得停 systemd 单元，实际调用:\n%s", calls)
	}
}

func TestSystemdStatusMapping(t *testing.T) {
	cases := []struct {
		active string
		want   string
	}{
		{"active", "running"},
		{"inactive", "stopped"},
		{"unknown", "stopped"},
		{"failed", "stopped"},
		{"activating", "running"},
		{"deactivating", "stopping"},
	}
	for _, c := range cases {
		t.Run(c.active, func(t *testing.T) {
			fakeSystemctl(t, "")
			t.Setenv("FAKE_ACTIVE", c.active)
			t.Setenv("FAKE_PID", "4242")
			db := openDB(t)
			sup := NewSupervisor(db)
			svc := addSystemd(t, db, "nginx")
			if _, err := sup.Start(svc); err != nil {
				t.Fatalf("Start: %v", err)
			}
			got, err := sup.ProbeUnit(svc)
			if err != nil {
				t.Fatalf("ProbeUnit(%s): %v", c.active, err)
			}
			if got.State != c.want {
				t.Fatalf("is-active=%s 应映射成 %s，得到 %s", c.active, c.want, got.State)
			}
		})
	}
}

// is-active 报 active 时 PID 得是真 PID：磁贴那行"PID xxxx"是用户判断
// "这到底是不是我以为的那个进程"的唯一依据，写 0 等于没写。
func TestSystemdStatusReadsMainPID(t *testing.T) {
	fakeSystemctl(t, "")
	t.Setenv("FAKE_ACTIVE", "active")
	t.Setenv("FAKE_PID", "4242")
	db := openDB(t)
	sup := NewSupervisor(db)

	got, err := sup.ProbeUnit(addSystemd(t, db, "nginx"))
	if err != nil {
		t.Fatalf("ProbeUnit: %v", err)
	}
	if got.PID != 4242 {
		t.Fatalf("PID 应为 MainPID 4242，得到 %d", got.PID)
	}
}

// 没有 systemd 的机器（容器、Termux）上必须给一句人话，而不是
// `exec: "systemctl": executable file not found in $PATH`。
func TestSystemdMissingBinaryGivesReadableError(t *testing.T) {
	dir := t.TempDir() // 空的：里面没有 systemctl
	t.Setenv("PATH", dir)
	db := openDB(t)
	sup := NewSupervisor(db)

	_, err := sup.Start(addSystemd(t, db, "nginx"))
	if err == nil {
		t.Fatal("systemctl 不存在时应当报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "systemd") && !strings.Contains(msg, "systemctl") {
		t.Fatalf("错误里应当指明 systemd/systemctl，得到: %s", msg)
	}
	if strings.Contains(msg, "executable file not found") {
		t.Fatalf("不该把 Go 的原始错误直接甩给用户: %s", msg)
	}
}

// systemctl 失败时它自己的 stderr（"Unit nginx.service not found."）
// 才是用户需要的信息，必须带出来。
func TestSystemdFailureIncludesSystemctlOutput(t *testing.T) {
	fakeSystemctl(t, "")
	t.Setenv("FAKE_FAIL", "Failed to start nginx.service: Unit not found.")
	db := openDB(t)
	sup := NewSupervisor(db)

	_, err := sup.Start(addSystemd(t, db, "nginx"))
	if err == nil {
		t.Fatal("systemctl 非零退出时应当报错")
	}
	if !strings.Contains(err.Error(), "Unit not found") {
		t.Fatalf("错误里应带 systemctl 自己的输出，得到: %s", err)
	}
}

// Start 之后状态要落库：面板刷新后磁贴才知道它是在跑的。
func TestSystemdStartPersistsState(t *testing.T) {
	fakeSystemctl(t, "")
	t.Setenv("FAKE_ACTIVE", "active")
	t.Setenv("FAKE_PID", "4242")
	db := openDB(t)
	sup := NewSupervisor(db)

	svc := addSystemd(t, db, "nginx")
	if _, err := sup.Start(svc); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err := GetState(db, svc.ID)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if st.State != StateRunning {
		t.Fatalf("落库状态应为 running，得到 %q", st.State)
	}
}

// systemd 类型没有子进程可托管：日志缓冲永远是空的，但绝不能因此报错，
// 否则前端点开"查看输出"就是一片红。
func TestSystemdLogIsEmptyNotNil(t *testing.T) {
	fakeSystemctl(t, "")
	db := openDB(t)
	sup := NewSupervisor(db)
	if buf := sup.Log(addSystemd(t, db, "nginx").ID); buf == nil {
		t.Fatal("Log 不应返回 nil")
	}
	if n := sup.Log(1).Len(); n != 0 {
		t.Fatalf("systemd 单元不该有面板日志，得到 %d 行", n)
	}
}

// 保证假脚本本身没写错：PATH 查找后跑起来的必须是我们造的那个。
func TestFakeSystemctlIsActuallyUsed(t *testing.T) {
	logPath := fakeSystemctl(t, "")
	// 用 start 而不是 is-active 做自检：假脚本的 is-active 会按 systemd
	// 的约定在非 active 时退出 3，拿它验"跑起来了"会把正确行为读成失败。
	out, err := exec.Command("systemctl", "start", "nginx").CombinedOutput()
	if err != nil {
		t.Fatalf("假 systemctl 没跑起来: %v (%s)", err, out)
	}
	if !strings.Contains(readCalls(t, logPath), "start nginx") {
		t.Fatal("假 systemctl 未记录调用")
	}
}
