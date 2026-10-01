// Package config 加载 /etc/litepanel/config.toml 并做启动前校验。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	Listen string `toml:"listen"`
	Port   int    `toml:"port"`
	DBPath string `toml:"db_path"`
	// TrashDirName 是**每个盘根目录下**回收站的目录名（不是绝对路径）。
	//
	// 回收站按盘分置（用户的裁定：删除永远是同盘 rename，绝不跨盘复制），
	// 所以这里只能是一段相对的名字：条目落在 <该文件所在盘根>/<此名>/。
	// 曾经叫 trash_path、写的是绝对路径，那个语义已经不存在（见 Load 里
	// 对旧键的显式拒绝）。
	TrashDirName string `toml:"trash_dir_name"`
	// TrashRetainDays 是回收站条目保留天数（设计 D12：默认 3，可设 1–90）。
	TrashRetainDays int `toml:"trash_retain_days"`
	// JobConcurrency 是后台复制/移动/删除的并发上限（设计 858：默认 2）。
	//
	// 它必须是可配的，而且 0 必须被拒：**0 个 worker 的队列是一个完全无
	// 症状的瘫痪队列** —— 提交照常返回 job_id、界面照常转"排队中"，而
	// 永远不会有任务开始跑。.1 项那样漏接数据库也是同一形态，但那是装配
	// bug（会被装配测试逮住），这个是用户手写出来的、没人会去怀疑一个
	// 数字没被用上。
	//
	// 为什么不是越大越好：这台机器上是 HDD + 12GB 内存，并发复制的收益在
	// 2–4 之后就是磁头互相打断；而每个 worker 各握着一块 1MB 缓冲和若干
	// 打开的文件描述符。上限 16 是"再高也不会更快，只会更挤"的分界。
	JobConcurrency int `toml:"job_concurrency"`
	TLS            TLSConfig

	// PasswordSet 由主程序在打开 DB 后回填：DB 中是否已存在密码。
	PasswordSet bool `toml:"-"`
}

// Defaults 返回设计 4.2 节约定的默认值。
func Defaults() Config {
	return Config{
		Listen:          ListenAuto,
		Port:            9530,
		DBPath:          "/var/lib/litepanel/litepanel.db",
		TrashDirName:    ".trash",
		TrashRetainDays: 3,
		JobConcurrency:  2,
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
	// 用 MetaData 拿"没被任何字段接住的键"。这里不是要拒绝一切未知键
	// （那会让"新版配置 + 旧版二进制"的降级直接起不来，而升级失败回滚
	// 是常规操作），只要抓住已废弃的那一个。
	meta, err := toml.Decode(string(b), &cfg)
	if err != nil {
		return cfg, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	for _, k := range meta.Undecoded() {
		if k.String() == "trash_path" {
			// 静默忽略老键的后果：配置里还写着 trash_path = "/DISK/.trash"，
			// 面板照常启动，而条目其实已经建到每个盘的 .trash 里 ——
			// 用户按他认得的回收站路径去找"删掉的文件"，找不到，那就是
			// 一次"面板把我的文件弄丢了"的误判。面板管着用户的文件，
			// 这种"配置还在、语义变了、一声不吭"必须拒绝到改配置为止。
			return cfg, fmt.Errorf("配置文件 %s：trash_path 已废弃（回收站现在建在每个盘根目录下的 %s 里），请改名为 trash_dir_name 并只填一个目录名（如 .trash）",
				path, cfg.TrashDirName)
		}
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
	if err := c.validateTrash(); err != nil {
		return err
	}
	// 与回收站天数的处理一致：配置文件是手写的，越界要**告知**而不是夹取。
	// （运行期从设置页写入的值另有装配层兜底，两处职责不同。）
	if c.JobConcurrency < 1 || c.JobConcurrency > 16 {
		return fmt.Errorf("job_concurrency（后台任务并发数）应在 1–16 之间，当前 %d；设为 0 会让所有复制/移动/删除永远停在排队中", c.JobConcurrency)
	}
	return nil
}

// 保留天数是否越界（含 0）。
func (c Config) trashRetentionInvalid() bool {
	return c.TrashRetainDays < 1 || c.TrashRetainDays > 90
}

// validateTrash 校验回收站的两项配置。
func (c Config) validateTrash() error {
	name := c.TrashDirName
	if name == "" {
		// 空值取默认而不是报错：文件里没写这一项（老配置改完删掉了、或者
		// 用了精简配置）是常态，为这个起不来面板不值。
		name = Defaults().TrashDirName
	}
	// 必须是**单个路径段**。回收站建在每个盘根下，绝对路径或多段路径拼接
	// 之后会变成 <盘根>/DISK/.trash —— 在每个盘上悄悄建出目录，而界面
	// 显示的回收站跟磁盘上的对不上。这种错不是"报个错"能糊过去的。
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("trash_dir_name（回收站目录名）必须是单个目录名（如 .trash），不能是路径：%q", c.TrashDirName)
	}
	// 天数边界与 filemgr.NewService 的夹取一致，但这里**报错**而不是夹：
	// 配置文件是手写的，写错的人需要被告知；装配层的夹取是给运行期
	// （设置页写入的值）兜底的最后一道，两处职责不同。
	// 0 也拒绝：没写这一项的话 Defaults() 已经填了 3，能到这里的是用户
	// 显式写了 0 —— 那正好是"刚删就没"，必须挡。
	if c.trashRetentionInvalid() {
		return fmt.Errorf("trash_retain_days 应在 1–90 之间，当前 %d", c.TrashRetainDays)
	}
	return nil
}
