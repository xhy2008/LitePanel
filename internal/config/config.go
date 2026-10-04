// Package config 加载 /etc/litepanel/config.toml 并做启动前校验。
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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

	// Aria2RPCURL 是 aria2 的 JSON-RPC 端点（D16：面板不拉起 aria2，只连它
	// 的 RPC 与事件 WS；aria2 由 systemd 常驻托管）。
	//
	// 默认只绑 localhost 是安全决定而不是偷懒：aria2 的 addUri 能指定任意
	// dir —— 一个能达的 aria2 RPC 就是一个无需身份的文件写入口。面板与
	// aria2 同机，改成远程地址只能是显式配置。
	Aria2RPCURL string `toml:"aria2_rpc_url"`
	// Aria2RPCSecret 是 aria2 的 rpc-secret（部署时由 install.sh 生成）。
	// 空串 = 不带 token：aria2 那边若配了 secret，所有调用会被回 -32000，
	// 健康检查会把它当“未授权”报给用户，而不是静默空列表。
	Aria2RPCSecret string `toml:"aria2_rpc_secret"`
	// Aria2DownloadDir 是新建下载的默认保存目录（供前端表单预填）。
	// 空串 = 不预填，aria2 用它自己的 --dir。
	Aria2DownloadDir string `toml:"aria2_download_dir"`

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
		Aria2RPCURL:     "http://127.0.0.1:6800/jsonrpc",
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
	if err := c.validateAria2(); err != nil {
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

// validateAria2 校验 aria2 RPC 地址（D16）。
//
// 为什么这属于"启动时拒绝"而不是"运行时报错"：这个配置的后果不是"下载失败"
// 而是"在服务器上开了一个文件写入口"。aria2 的 addUri 能指定任意 dir，拿到
// rpc-secret 就等于能在面板主机上落任意文件 —— 与监听 0.0.0.0 那几条门禁是
// 同一类：扩大暴露面必须同时有对应的防护，而且必须在启动时说清楚，而不是在
// 某次下载失败时让用户猜。
//
// 明文 http 只放行回环：面板与 aria2 同机是常态，回环上的流量不出网卡，
// 密钥不裸奔；跨机就必须上 https（或自己拉隧道），这是显式可以做出的选择。
func (c Config) validateAria2() error {
	u := strings.TrimSpace(c.Aria2RPCURL)
	if u == "" {
		return errors.New("aria2_rpc_url 不能为空（默认 http://127.0.0.1:6800/jsonrpc；不想用下载功能也不能留空）")
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("aria2_rpc_url %q 不是合法的 RPC 地址，应形如 http://127.0.0.1:6800/jsonrpc", u)
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("aria2_rpc_url %q 用明文 http 连非回环地址：rpc-secret 会在网络上裸奔，"+
			"拿到它的人能在本机的任意目录写任意文件；跨机请改用 https，或在中间拉隧道后仍连 127.0.0.1", u)
	}
	return nil
}

// isLoopbackHost 判断主机名是否是回环地址。
//
// 用 net 解析而不是字符串比 "127.0.0.1"：IPv6 的 ::1、以及 "localhost"
// 这种写法都是真回环；而把 "127.0.0.1.evil.com" 当成回环（前缀匹配的经典
// 漏法）会把这条门禁整个绕过。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
