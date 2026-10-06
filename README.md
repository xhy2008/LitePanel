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
- 下载目录：**首次部署时询问**（回车用 `/var/lib/litepanel/downloads`；
  非交互安装自动用默认值），写入 env 文件；面板（设置页预填）与 aria2
  （`--dir`）读同一份，两边不会漂；之后改它在设置页热改，或由你同时改
  env + 重启两个服务
- `--tls`：首次部署时顺带自签一张 TLS 证书（SAN 含 127.0.0.1 +
  本机 tailscale IP）；对已有部署不生效
- `--uninstall`：停服务、删单元与二进制，**配置与数据默认保留**；加
  `--purge-data` 才连 `/etc/litepanel` 与 `/var/lib/litepanel` 一起删
- `systemctl enable` 两个服务：面板与 aria2 都开机自启（aria2 由 systemd
  托管，面板只连它的 RPC，永远不负责拉起它）

### 首次登录

一次性初始密码**不落任何文件**，只打一次到 journald：

```bash
journalctl -u litepanel -n 5      # 找到"一次性初始密码"那行
```

登录后立即改密（面板会强制引导）。密码之后以数据库为准。

### 局域网 / 全接口访问

想从局域网（或任意网卡）访问，把 `listen` 改成 `0.0.0.0` —— 它包含
tailscale0，所以局域网 IP 与 tailscale IP 同时可达，不必二选一：

```toml
listen = "0.0.0.0"
port = 9530
```

**但 `0.0.0.0` 会触发安全门禁**（设计 4.1），面板要求同时满足，缺一拒绝
启动：① 已设过密码；② 启用 TLS 且证书/私钥文件真实存在。`install.sh`
会在启动服务前预检这三条并把修复命令打出来——**别忽略它**，直接跑
下去只会让 systemd 反复重启、浏览器报"拒绝连接"。

首台部署的正确顺序：先用默认 `listen = "tailscale"` 起面板、登录改密码，
再回来配 TLS、把 listen 改 `0.0.0.0`。门禁的本意是防"运行中的面板
不小心裸奔"，从没打算拦第一次开机。

### HTTPS

默认 `listen = "tailscale"` 时可以不折腾证书（tailnet 内部访问走明文即可）。
要证书的话面板只从 `cert_file`/`key_file` 读，不会自动签发，两条路任选：

```bash
# A. 项目自带生成器：把本机所有 IP（含局域网 IP）自动签进 SAN，
#    免手填 IP、不怕漏。输出到 /etc/litepanel/tls/{cert,key}.pem
sudo go run deploy/mkcert.go /etc/litepanel/tls

# B. openssl 手签（SAN 必须含要访问的每一个 IP，逗号分隔；
#    占位符要换成真地址，写了假的会 "bad ip address" 报错）
sudo mkdir -p /etc/litepanel/tls
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout /etc/litepanel/tls/key.pem -out /etc/litepanel/tls/cert.pem \
  -days 3650 -nodes -subj "/CN=litepanel" \
  -addext "subjectAltName=IP:127.0.0.1,IP:::1,DNS:localhost,IP:<服务器局域网IP>"
sudo chmod 600 /etc/litepanel/tls/key.pem
```

签完把 `[tls]` 的 `enabled=true` + 两个路径填进 config，`systemctl restart
litepanel`。自签证书浏览器会警告一次（点"继续访问"）；换 IP 段要重签，
建议路由器给服务器做 DHCP 静态绑定。

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

### 排障速查（真机踩过的坑）

先看状态再看日志，`journalctl -u litepanel -n 10` 里一行 `litepanel: `
开头的摘要就是启动失败的全部原因（R4：启动失败一定有一句人话）。

| 症状 | 真实原因 | 修法 |
|---|---|---|
| 浏览器"拒绝连接"，且只在 `listen=0.0.0.0` 时 | 不是防火墙！是面板被安全门禁拒之门外、根本没启动（缺密码或 TLS）。绑具体 IP 能通就是因为没触发门禁 | `journalctl -u litepanel`，多半是 TLS；按上面 HTTPS 节配证书 |
| aria2 反复重启，日志 `Failed to open … session.txt` | 单元已自备该文件；若仍报错是旧单元未更新 | `git pull && sudo ./deploy/install.sh` |
| 下载页报"失败/未授权"，但 aria2 的 RPC 手动 curl 正常 | 面板 config 里没 `aria2_rpc_secret`（缺=按无 token 发，被 aria2 拒） | `printf '\naria2_rpc_secret = "%s"\n' "$(sed -n 's/^A2_RPC_SECRET=//p' /etc/litepanel/aria2.env)" >> /etc/litepanel/config.toml && systemctl restart litepanel` |
| 首启不知道密码 | 一次性密码不落文件，只打一次 journald | `journalctl -u litepanel -n 5` |
| 面板重启后自己托管的服务全停了 | 设计 R1 既知行为（command 型服务随面板进程组退出）；systemd 型与 tmux 终端不受影响 | 服务页手动拉起 |

`install.sh` 已能自动拦前两条（启动门禁预检）与第三条（secret 缺失/分家
告警）；报错里的修复命令可直接复制。

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
