# litepanel

单二进制服务器管理面板：终端 / 文件 / 快捷命令 / 下载 / 设置 五个标签页 +
右栏资源仪表。前端产物 `go:embed` 进二进制，目标机上没有 Node。

- 目标环境：Ubuntu 24.04+（systemd、root）、任意架构
- 开发环境：本仓库日常在 Termux/Android 上开发，那条路径见文末

## 部署（Ubuntu，root）

```bash
git clone https://github.com/xhy2008/LitePanel && cd LitePanel
sudo apt install -y build-essential golang-go tmux aria2 nodejs npm
make web && make build          # 前端产物 → 嵌入 → bin/litepanel
sudo ./deploy/install.sh        # 也可 make install
```

`install.sh` 是幂等的，安装与升级共用：

- 二进制 → `/usr/local/bin/litepanel`；单元 → systemd；数据 → `/var/lib/litepanel/`
- **已有配置绝不被覆盖**：`/etc/litepanel/config.toml` 只在缺失时从模板生成
- **aria2 secret 只生成一次**（`/etc/litepanel/aria2.env`，0600）：换 secret
  会让面板里的值与 aria2 实际值分家，之后每个下载调用都回"未授权"
- 下载目录自动选**可用容量最大的挂载点**下的 `downloads/`，写进 env 文件；
  面板（设置页预填）与 aria2（`--dir`）读同一份，两边不会漂
- `systemctl enable` 两个服务：面板与 aria2 都开机自启（aria2 由 systemd
  托管，面板只连它的 RPC，永远不负责拉起它）

### 首次登录

一次性初始密码**不落任何文件**，只打一次到 journald：

```bash
journalctl -u litepanel -n 5      # 找到"一次性初始密码"那行
```

登录后立即改密（面板会强制引导）。密码之后以数据库为准。

### HTTPS

监听 `0.0.0.0` 必须已设密码且启用 TLS，否则面板拒绝启动（原因看
journalctl）。默认 `listen = "tailscale"`：自动绑 tailscale IPv4，探测不到
回落 `127.0.0.1`——tailnet 内部访问可以不折腾证书。

要证书的话两条路任选，面板只从 `cert_file`/`key_file` 读，不会自动签发：

```bash
# 自签（局域网 IP 直连够用；浏览器会警告一次）
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout /etc/litepanel/key.pem -out /etc/litepanel/cert.pem \
  -days 3650 -nodes -subj "/CN=lit" \
  -addext "subjectAltName=IP:<服务器IP>"

# 或 tailscale cert 签好后把路径填进 config.toml 的 [tls]
```

### 升级

```bash
git pull && make web && make build && sudo ./deploy/install.sh
```

既知行为（设计 R1，不是 bug）：面板重启时**自己托管的 command 型服务会
全部停止**（子进程随面板进程组退出）；systemd 型服务不受影响。重启后在
服务页手动拉起前者。终端会话走 tmux，**不会**被重启打断（D5）。

### 验证

```bash
systemctl is-enabled litepanel aria2     # 两个 enabled
journalctl -u litepanel -n 5             # 首启密码 / 错误摘要都在这
ps -o user= -p "$(pgrep -x litepanel)"   # root
curl -sk https://<地址>:9530/ -o /dev/null -w '%{http_code}\n'
```

冒烟脚本（对着装好的实例跑）：

```bash
make smoke PANEL_PASS=<改后的密码>          # 启动路径端到端
make smoke-restart                          # kill -9 后 tmux 会话存活（D5）
```

## 开发

```bash
make deps          # 装工具链（Termux 走 pkg，Linux 走 apt）
make test          # Go 双构建标签 + 前端 vitest
make build-debug   # 带日志的调试构建 → bin/litepanel-debug
make web && make build
```

Termux 上的开发实例（dev/ 下自成一体，与仓库源码隔离）：

```bash
./bin/litepanel-debug -config dev/acc.toml &   # 见 dev/acc.pid
python3 dev/settings_e2e.py                    # 设置模块 23 项真机回归
```

## 仓库结构

```
cmd/litepanel/        入口与装配层（tag 分文件：debug/release 行为差异）
internal/<domain>/    api auth config download filemgr metrics quickcmd
                      service settings store terminal termws upload ws logx webdist
web/                  Vue3 + Pinia + vitest；产物嵌进 internal/webdist/dist
deploy/               systemd 单元、配置模板、install.sh
tools/                make smoke / smoke-restart / 证伪脚本
```

发布构建（`-tags release`，默认）：零日志落盘、stderr 仅 R4 两类例外
（启动失败摘要 + 一次性初始密码）；`-tags debug` 才往 stderr 打运行日志。
两套标签的测试都在 `make test` 里。
