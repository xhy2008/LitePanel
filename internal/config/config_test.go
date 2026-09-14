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
	if !strings.HasSuffix(cfg.TrashPath, "/.trash") {
		t.Errorf("默认回收站路径错误: %q", cfg.TrashPath)
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
trash_path = "/DISK/.trash"

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
	if cfg.Listen != "100.64.0.1" || cfg.Port != 9999 || cfg.DBPath != "/tmp/x.db" || cfg.TrashPath != "/DISK/.trash" {
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
