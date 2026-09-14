// Package config 加载 /etc/litepanel/config.toml 并做启动前校验。
package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// ListenAuto 是 Listen 的哨兵值：启动时自动探测 tailscale 地址。
const ListenAuto = "tailscale"

type TLSConfig struct {
	Enabled       bool   `toml:"enabled"`
	CertFile      string `toml:"cert_file"`
	KeyFile       string `toml:"key_file"`
	TailscaleCert bool   `toml:"tailscale_cert"`
}

type Config struct {
	Listen    string `toml:"listen"`
	Port      int    `toml:"port"`
	DBPath    string `toml:"db_path"`
	TrashPath string `toml:"trash_path"`
	TLS       TLSConfig

	// PasswordSet 由主程序在打开 DB 后回填：DB 中是否已存在密码。
	PasswordSet bool `toml:"-"`
}

// Defaults 返回设计 4.2 节约定的默认值。
func Defaults() Config {
	return Config{
		Listen:    ListenAuto,
		Port:      9530,
		DBPath:    "/var/lib/litepanel/litepanel.db",
		TrashPath: "/DISK/.trash",
	}
}

// Load 读取配置文件；文件不存在时返回默认值（首次安装场景）。
func Load(path string) (Config, error) {
	cfg := Defaults()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	return cfg, nil
}

// Validate 实施设计 4.1 的安全门禁：扩大暴露面必须同时有密码与 TLS。
func (c Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("端口 %d 非法，应在 1–65535", c.Port)
	}
	if c.Listen == "0.0.0.0" || c.Listen == "::" || c.Listen == "*" {
		if !c.PasswordSet {
			return errors.New("监听 0.0.0.0 前必须先在设置中设置密码（或改回 tailscale/127.0.0.1）")
		}
		if !c.TLS.Enabled {
			return errors.New("监听 0.0.0.0 必须同时启用 TLS")
		}
	}
	if c.TLS.Enabled && !c.TLS.TailscaleCert {
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return errors.New("启用 TLS 需要提供证书文件 cert_file 与私钥文件 key_file，或改用 tailscale_cert")
		}
	}
	if c.DBPath == "" {
		return errors.New("db_path 不能为空")
	}
	if c.TrashPath == "" {
		return errors.New("trash_path（回收站路径）不能为空")
	}
	return nil
}
