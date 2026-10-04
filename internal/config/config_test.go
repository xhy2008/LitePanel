package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsWhenFileMissing(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("缺失配置文件应回落到默认值而不是报错: %v", err)
	}
	if cfg.Port != 9530 {
		t.Errorf("默认端口应为 9530, got %d", cfg.Port)
	}
	if cfg.Listen != "tailscale" {
		t.Errorf("默认应自动探测 tailscale 地址, got %q", cfg.Listen)
	}
	if !strings.HasPrefix(cfg.DBPath, "/var/lib/litepanel") {
		t.Errorf("默认 DB 路径错误: %q", cfg.DBPath)
	}
	// 回收站目录名是**相对的一段名字**（每个盘根下各建一个），默认值
	// 不能带斜杠 —— 带斜杠拼出来就是 <盘根>/DISK/.trash 那种东西。
	if cfg.TrashDirName != ".trash" {
		t.Errorf("默认回收站目录名错误: %q", cfg.TrashDirName)
	}
	if cfg.TrashRetainDays != 3 {
		t.Errorf("默认保留天数应为 3（设计 D12）, got %d", cfg.TrashRetainDays)
	}
}

func TestValidateTable(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // 空串表示应通过
	}{
		{"默认值合法", func(*Config) {}, ""},
		{"监听 0.0.0.0 且未设密码应拒绝", func(c *Config) {
			c.Listen = "0.0.0.0"
			c.PasswordSet = false
			c.TLS.Enabled = false
		}, "0.0.0.0"},
		{"监听 0.0.0.0 已设密码但未启用 TLS 应拒绝", func(c *Config) {
			c.Listen = "0.0.0.0"
			c.PasswordSet = true
			c.TLS.Enabled = false
		}, "TLS"},
		{"监听 0.0.0.0 已设密码且启用 TLS 合法", func(c *Config) {
			c.Listen = "0.0.0.0"
			c.PasswordSet = true
			c.TLS.Enabled = true
			c.TLS.TailscaleCert = true
		}, ""},
		{"端口 0 非法", func(c *Config) { c.Port = 0 }, "端口"},
		{"端口越界非法", func(c *Config) { c.Port = 70000 }, "端口"},
		{"TLS 启用但证书路径为空应拒绝", func(c *Config) {
			c.TLS.Enabled = true
			c.TLS.CertFile = ""
			c.PasswordSet = true
			c.Listen = "100.64.0.1"
		}, "证书"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过校验, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报含 %q 的错误, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息应含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadParsesTOML(t *testing.T) {
	p := write(t, `
listen = "100.64.0.1"
port = 9999
db_path = "/tmp/x.db"
trash_dir_name = ".回收站"
trash_retain_days = 7

[tls]
enabled = true
cert_file = "/etc/certs/a.crt"
key_file = "/etc/certs/a.key"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PasswordSet = true // 模拟 DB 中已存在密码
	if err := cfg.Validate(); err != nil {
		t.Fatalf("显式配置应合法: %v", err)
	}
	if cfg.Listen != "100.64.0.1" || cfg.Port != 9999 || cfg.DBPath != "/tmp/x.db" || cfg.TrashDirName != ".回收站" || cfg.TrashRetainDays != 7 {
		t.Fatalf("解析结果不符: %+v", cfg)
	}
	if !cfg.TLS.Enabled || cfg.TLS.CertFile != "/etc/certs/a.crt" {
		t.Fatalf("TLS 段解析不符: %+v", cfg.TLS)
	}
}

func TestLoadRejectsBrokenTOML(t *testing.T) {
	p := write(t, `listen = `)
	if _, err := Load(p); err == nil {
		t.Fatal("损坏的 TOML 应报错")
	}
}

// ---------- 回收站配置：从"一个绝对路径"变成"每个盘一个目录名" ----------

// 回收站目录名必须是**单个路径段**。
//
// 回收站现在建在每个盘根下（<盘根>/<目录名>），"每盘一个"这件事完全依赖
// 这个值是相对的一段名字。配置里写 "/DISK/.trash" 这样的绝对路径，拼接
// 之后会变成 <盘根>/DISK/.trash —— 悄悄在**每个盘**上都建出一个 DISK
// 子目录，而用户在界面上看到的回收站内容跟磁盘上的对不上。这种错不是
// 报错能糊过去的：它会把垃圾写到每个盘上。所以必须在启动时就拒绝。
func TestValidateTrashDirName(t *testing.T) {
	cases := []struct {
		name string
		set  string
		bad  bool
	}{
		{"默认 .trash 合法", ".trash", false},
		{"自定义相对名合法", ".回收站", false},
		{"带斜杠的绝对路径拒绝", "/DISK/.trash", true},
		{"带斜杠的相对路径拒绝", "a/.trash", true},
		{".. 拒绝", "..", true},
		{"点拒绝", ".", true},
		{"空值取默认（合法）", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.TrashDirName = tc.set
			err := cfg.Validate()
			if tc.bad && err == nil {
				t.Errorf("%q 应被拒绝（回收站目录名必须是单个路径段）", tc.set)
			}
			if !tc.bad && err != nil {
				t.Errorf("%q 应合法, got %v", tc.set, err)
			}
		})
	}
}

// 保留天数的边界：设置页输入 0 或负数不能让"3 天后清理"变成"立刻清理"。
func TestValidateTrashRetain(t *testing.T) {
	cases := []struct {
		name string
		set  int
		bad  bool
	}{
		{"默认 3 天合法", 3, false},
		{"1 天合法", 1, false},
		{"90 天合法", 90, false},
		{"0 拒绝（等于刚删就没）", 0, true},
		{"负数拒绝", -1, true},
		{"91 拒绝（超出设计上限）", 91, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.TrashRetainDays = tc.set
			err := cfg.Validate()
			if tc.bad && err == nil {
				t.Errorf("%d 应被拒绝", tc.set)
			}
			if !tc.bad && err != nil {
				t.Errorf("%d 应合法, got %v", tc.set, err)
			}
		})
	}
}

// 老的 trash_path 配置项必须**明确报错**，不能被静默忽略。
//
// 上一版设计把回收站放在一个绝对路径里，配置文件里就写着 trash_path =
// "/DISK/.trash"。现在改成每个盘自己的回收站，那个键的意思没了。toml
// 解析默认忽略未知键 —— 什么都不做的话，老配置照常启动，而用户以为
// 回收站还在 /DISK，实际条目已经建到每个盘的 .trash 里去了。
// 面板正在管着用户的文件，这种"配置还在、语义变了、一声不吭"的组合
// 必须拒绝到用户改配置为止。
func TestLoadRejectsRetiredTrashPath(t *testing.T) {
	p := write(t, `
listen = "127.0.0.1"
db_path = "/tmp/x.db"
trash_path = "/DISK/.trash"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("已废弃的 trash_path 必须报错，不能静默忽略")
	}
	if !strings.Contains(err.Error(), "trash_dir_name") {
		t.Errorf("错误要告诉用户改成什么, got %q", err.Error())
	}
}

// 后台拷贝/移动/删除的并发上限必须是可配的（设计 858：默认 2）。
func TestJobConcurrencyDefaultAndLoad(t *testing.T) {
	if Defaults().JobConcurrency != 2 {
		t.Errorf("默认并发该是 2（设计 8.4），got %d", Defaults().JobConcurrency)
	}
	p := write(t, "job_concurrency = 5\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JobConcurrency != 5 {
		t.Errorf("没读进来, got %d", cfg.JobConcurrency)
	}
}

// 越界的并发数必须报错而不是悄悄夹取。
//
// 尤其 0：**0 个 worker 的队列是一个完全无症状的瘫痪队列** —— 端点照常
// 返回 job_id，界面照常显示"排队中"，而永远不会有东西开始跑。这跟漏接
// 数据库是同一个失效形态，而配置文件是手写的，写的人需要被告知。
// 上限 16：并发再高也只是让磁盘排队，而每个 worker 各占一个打开的文件
// 与一块 1MB 缓冲，在 12GB 内存的机器上没有换来任何东西。
func TestJobConcurrencyValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  int
		ok   bool
	}{
		{"下界 0 拒绝", 0, false},
		{"负数拒绝", -1, false},
		{"上界 16 可以", 16, true},
		{"超上界拒绝", 17, false},
		{"正常值可以", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.JobConcurrency = tc.val
			err := c.Validate()
			if tc.ok && err != nil {
				t.Errorf("JobConcurrency=%d 不该报错: %v", tc.val, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("JobConcurrency=%d 该报错却没有", tc.val)
			}
		})
	}
}

// aria2 RPC 地址的安全门禁（D16 的落点）。
//
// 面板不拉起 aria2，只连它 —— 但**连到哪里**是面板配置里的一个决定，而这个
// 决定的后果不对称：
//
//   - 明文 http 连远程地址：rpc-secret 在网络上裸奔，拿到它的人就能用
//     addUri 指定任意 dir，在服务器上写任意文件。等于开一个无需身份的文件
//     写入入口，而且没人会怀疑是自己的一行配置开的。
//   - 空白名单、缺失字段：面板对着空 URL 发请求，用户看到的是"aria2 没起"，
//     方向完全错。
//
// 与 0.0.0.0 那几条门禁同源：扩大暴露面必须同时有对应的防护，且必须在**启动
// 时**说清楚，而不是在第一次下载失败时让用户猜。
func TestValidateAria2RPCURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"默认 localhost 放行", "http://127.0.0.1:6800/jsonrpc", true},
		{"localhost 主机名放行", "http://localhost:6800/jsonrpc", true},
		{"IPv6 回环放行", "http://[::1]:6800/jsonrpc", true},
		{"https 到远程放行（带密钥不算裸奔）", "https://aria2.internal:6800/jsonrpc", true},
		{"明文 http 到远程拒绝", "http://10.0.0.5:6800/jsonrpc", false},
		{"空 URL 拒绝", "", false},
		{"没有协议拒绝", "127.0.0.1:6800/jsonrpc", false},
		{"不是 URL 的字符串拒绝", "aria2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Aria2RPCURL = tc.url
			err := c.Validate()
			if tc.ok && err != nil {
				t.Errorf("%q 不该报错: %v", tc.url, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("%q 该报错却没有", tc.url)
			}
		})
	}
}

// https 之外的加密形态不算数：file://、ftp:// 这类地址会被 net/http 直接拒
// （面板侧报一句看不懂的错），或者更糟：悄悄连到别的东西上。
func TestValidateAria2RPCURLScheme(t *testing.T) {
	c := Defaults()
	c.Aria2RPCURL = "ftp://127.0.0.1:6800/jsonrpc"
	if err := c.Validate(); err == nil {
		t.Error("非 http/https 协议该被拒")
	}
}

// 默认值本身必须能通过校验。
//
// 否则"用户什么都没配"就等于"面板起不来"，而默认值恰恰是最常走的那条路径
// （首次安装）。这条看起来是废话的断言，实测挡过一次：默认值写成了不带协议
// 的 "127.0.0.1:6800/jsonrpc"。
func TestDefaultsPassValidation(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("默认配置应能通过校验: %v", err)
	}
}
