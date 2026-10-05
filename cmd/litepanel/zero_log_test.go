package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// M7-T7 零日志发布构建验证（D9 + R4）。
//
// 为什么必须编译真二进制、起真进程：logx 的"发布构建零输出"在单元层
// （internal/logx 的 tag 测试）与装配层（TestMainWiringSilentInRelease）都
// 测过，但它们测的都是**这个包自己认为的**零输出。真正要钉住的用户承诺是
// "发布构建跑起来之后，机器上不会多出任何面板的日志文件，stderr 除了
// R4 的两类例外完全安静"。这句话只有在真产物上才能验证 —— 它能抓到单元
// 测试结构上看不见的一切：有人新加了一个 file logger、有人把 log 指到了
// 文件、vendor 依赖偷偷写盘、pprof 打开时往 cwd 落 profile。
//
// 四个断言面（对应实施计划 M7-T7 的四个用例）：
//  1. TestReleaseBuildProducesNoLogFiles —— 跑完扫运行目录：除数据库（含
//     WAL 伴随文件）与配置文件外不许有任何文件；uploads/ 目录不许被创建
//     （它只该在真上传时按需建）。第二遍启动（密码已设置）stderr 必须
//     一个字节都没有。
//  2. TestReleaseLogCallsAreNoOps —— 上面第二遍启动的 stderr 零字节断言
//     就是它的进程级版本；单元级（logx.Init 后调用全部函数、buf 为空）
//     由 internal/logx/logx_test.go 钉。两层都留着：一层测实现，一层测
//     承诺。
//  3. TestDebugBuildDoesLog —— debug-tagged 二进制真跑，stderr 必须出现
//     INFO 行。没有这条，"release 静默"可能只是"日志全坏了"的别名。
//  4. TestFatalErrorStillReachesStderr —— 坏配置启动：退出码 1、stderr
//     一行 "litepanel: " 前缀摘要（R4：systemd 下启动失败不能一片空白）。
//
// 成本说明：每个 tag 各编译一次二进制（sync.Map 缓存共享），构建一次约
// 1–2 分钟。这是本文件唯一的重资源，断言本身都是秒级。

// ---- 二进制缓存 ----

type binEntry struct {
	once sync.Once
	path string
	err  error
}

var bins sync.Map // tag -> *binEntry

// binRoot 是缓存二进制的家；由 socket_test.go 的 TestMain 统一清理
// （那已经是本包的全局环境装配点，不另起第二个 TestMain）。
var binRoot string

func panelBinary(t *testing.T, tag string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境里没有 go 命令，无法编译被测二进制")
	}
	v, _ := bins.LoadOrStore(tag, new(binEntry))
	e := v.(*binEntry)
	e.once.Do(func() {
		root, err := os.MkdirTemp("", "litepanel-releasetest")
		if err != nil {
			e.err = err
			return
		}
		if binRoot == "" {
			binRoot = root
		}
		out := filepath.Join(root, "litepanel-"+tag)
		// 工作目录就是 cmd/litepanel（go test 的规则），"." 即本包。
		// -tags release 与 Makefile 的 build 目标逐字对齐；debug 同理。
		cmd := exec.Command("go", "build", "-tags", tag, "-o", out, ".")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			e.err = fmt.Errorf("%v: %s", err, stderr.String())
			return
		}
		e.path = out
	})
	if e.err != nil {
		t.Fatalf("编译 %s 构建失败: %v", tag, e.err)
	}
	return e.path
}

// ---- 起停面板 ----

type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type panelProc struct {
	cmd    *exec.Cmd
	stderr *safeBuf
	exited chan error
}

// startPanel 在 dir 里以 config.toml 启动面板。stderr 归集到 safeBuf ——
// 这是被测行为的观测点本身，不是杂音收集器。
func startPanel(t *testing.T, bin, dir string) *panelProc {
	t.Helper()
	cmd := exec.Command(bin, "-config", "config.toml")
	cmd.Dir = dir
	// 与 TestMain 的 tmux 隔离约定一致：子进程继承测试进程的 TMUX_TMPDIR，
	// 面板的终端对账只打在私有 socket 上，不碰开发者正开着的会话。
	p := &panelProc{cmd: cmd, stderr: new(safeBuf), exited: make(chan error, 1)}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动面板失败: %v", err)
	}
	go func() { p.exited <- cmd.Wait() }()
	return p
}

// waitReady 轮询到面板能答 HTTP 为止。进程提前退出立刻失败并把 stderr 原样
// 贴出来 —— 测试失败信息里最没用的就是"没起来"三个字。
func (p *panelProc) waitReady(t *testing.T, port int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.exited:
			t.Fatalf("面板过早退出: %v\nstderr:\n%s", err, p.stderr.String())
		default:
		}
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("静态首页应 200, got %d", resp.StatusCode)
			}
			return
		}
		time.Sleep(120 * time.Millisecond)
	}
	t.Fatalf("面板 30s 内未就绪\nstderr:\n%s", p.stderr.String())
}

// stopAndRead 发 SIGTERM（main 的优雅关停路径），等真退出，返回 stderr。
// Wait 返回后 exec 保证 stderr 拷贝已完成，读 safeBuf 无竞争。
func (p *panelProc) stopAndRead(t *testing.T) string {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("发送 SIGTERM 失败: %v", err)
	}
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
		p.cmd.Process.Kill()
		t.Fatalf("面板 30s 内没有退出（优雅关停路径坏了）")
	}
	return p.stderr.String()
}

// ---- 夹具 ----

func writePanelConfig(t *testing.T, dir string, port int) {
	t.Helper()
	// 127.0.0.1 + 无 TLS 是配置校验允许的组合（只有 0.0.0.0 强制 TLS）。
	cfg := fmt.Sprintf(`listen = "127.0.0.1"
port = %d
db_path = "./panel.db"
trash_dir_name = ".trash"
trash_retain_days = 3
`, port)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runPanelOnce 完整生命周期：启动 → 等就绪 → 停 → 返回 stderr。
func runPanelOnce(t *testing.T, bin, dir string, port int, hold time.Duration) string {
	t.Helper()
	p := startPanel(t, bin, dir)
	p.waitReady(t, port)
	time.Sleep(hold)
	return p.stopAndRead(t)
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---- 断言面 1 + 2：release 真产物零落盘、运行期 stderr 零字节 ----

func TestReleaseBuildProducesNoLogFiles(t *testing.T) {
	bin := panelBinary(t, "release")
	dir := t.TempDir()
	port := freePort(t)
	writePanelConfig(t, dir, port)

	// 第一遍：全新数据库。stderr 的唯一合法输出是 R4 豁免的一次性初始密码
	// 行 —— "必须可见"与"只有这一行可见"是同一枚硬币的两面，这里把两面
	// 都钉住。
	out1 := runPanelOnce(t, bin, dir, port, 4*time.Second)
	lines := nonEmptyLines(out1)
	if len(lines) != 1 || !strings.Contains(lines[0], "一次性初始密码") {
		t.Fatalf("首启 stderr 应只有初始密码一行, got:\n%s", out1)
	}
	// release 走 CSPRNG 路径：密码不该恰好是 debug 的固定值（钉住构建标签
	// 真的分开了两条实现 —— 反过来 dev 值出现在 release 产物里就是灾难）。
	if strings.Contains(out1, "12345") {
		t.Fatalf("release 产物泄露了调试固定密码:\n%s", out1)
	}

	// 第二遍：密码已设置，没有任何 R4 豁免理由，stderr 必须零字节。
	// 这就是"发布构建的 logx 调用全是空操作"的进程级证明。
	out2 := runPanelOnce(t, bin, dir, port, 4*time.Second)
	if out2 != "" {
		t.Fatalf("release 第二遍运行 stderr 必须为空, got:\n%s", out2)
	}

	// 落盘面：除配置与数据库（含 WAL 伴随文件）外不许有任何文件。
	// uploads/ 显式要求不存在 —— 它只该在真上传时按需创建，启动期创建
	// 它意味着"面板一活就碰盘"，与零日志承诺同一性质。
	want := map[string]bool{
		"config.toml":  true,
		"panel.db":     true,
		"panel.db-wal": true,
		"panel.db-shm": true,
	}
	var extra []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if !want[rel] {
			extra = append(extra, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) > 0 {
		t.Errorf("发布构建在运行目录里创建了白名单之外的文件: %v", extra)
	}
}

// ---- 断言面 3：debug 产物必须真会说话 ----
//
// 没有这条测试，"release 静默"完全可能与"日志系统整体损坏"同时为真。
// 同夹具、同路径、只差一个构建标签 —— stderr 有 INFO 行，证明静默来自
// 构建标签，而不是来自某个把输出掐死的公共缺陷。
// 验证的是"日志管道是活的"，而不是"某一行必然出现"：WARN/ERROR 行一样
// 证明管道活。断言成 "INFO" 会把一个合法实现（比如启动行改词）杀成假红。
func TestDebugBuildDoesLog(t *testing.T) {
	bin := panelBinary(t, "debug")
	dir := t.TempDir()
	port := freePort(t)
	writePanelConfig(t, dir, port)

	out := runPanelOnce(t, bin, dir, port, 2*time.Second)
	// 日志行的时间戳格式（logx emit 的 "15:04:05.000 LEVEL "）是 debug 产物
	// 独有的形状：stderrNote 的行不带它。有带级别的日志行 = 管道活。
	if !regexp.MustCompile(`\d{2}:\d{2}:\d{2}\.\d{3}\s+(DEBUG|INFO|WARN|ERROR)`).MatchString(out) {
		t.Fatalf("debug 产物的 stderr 应含带时间戳与级别的日志行, got:\n%s", out)
	}
	// 调试固定初始密码（initpass_debug.go）：一并钉住"标签分开的不仅是
	// 日志，还有密码策略"。
	if !strings.Contains(out, "12345") {
		t.Fatalf("debug 产物应使用调试初始密码, got:\n%s", out)
	}
}

// ---- 断言面 4：启动失败必须吼出来（R4） ----

func TestFatalErrorStillReachesStderr(t *testing.T) {
	bin := panelBinary(t, "release")
	dir := t.TempDir()
	// 回收站目录名含路径分隔符 —— config.Validate 明确拒绝的输入，走的是
	// fatal()（直写 stderr），不经过 logx。db_path 指向可写目录：否则
	// 会先死在开库阶段，验不到配置校验那条路径。
	cfg := fmt.Sprintf(`listen = "127.0.0.1"
port = %d
db_path = "./panel.db"
trash_retain_days = 3
trash_dir_name = "bad/name"
`, freePort(t))
	if err := os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	p := startPanel(t, bin, dir)
	select {
	case err := <-p.exited:
		if err == nil {
			t.Fatalf("坏配置必须非零退出")
		}
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 1 {
			t.Fatalf("退出码应为 1, got %v", err)
		}
		out := p.stderr.String()
		if !strings.HasPrefix(out, "litepanel: ") {
			t.Fatalf("失败摘要应带 litepanel: 前缀, got:\n%s", out)
		}
		// 必须能定位到是哪个配置项坏了 —— 只有 "litepanel: boom" 的话，
		// journalctl 里等于没说。
		if !strings.Contains(out, "trash_dir_name") {
			t.Fatalf("失败摘要应指明坏掉的配置项, got:\n%s", out)
		}
	case <-time.After(20 * time.Second):
		p.cmd.Process.Kill()
		t.Fatal("坏配置下面板没有退出（fatal 路径没走到？）")
	}
}
