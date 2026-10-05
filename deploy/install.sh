#!/usr/bin/env bash
# litepanel 安装/升级脚本（root 执行）。
#
# 幂等是硬要求：`git pull && make web && make build && sudo ./deploy/install.sh`
# 就是升级路径本身，任何"第二次跑会毁坏现有部署"的行为（覆盖 config、
# 重新生成 secret）都让升级等于重装。
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO/bin/litepanel"
ETC=/etc/litepanel
STATE=/var/lib/litepanel

[ "$(id -u)" = 0 ] || { echo "必须 root 运行（面板以 root 服务，装系统单元也需要）" >&2; exit 1; }
[ -x "$BIN" ] || { echo "找不到 $BIN —— 先 make build" >&2; exit 1; }
# 只校验"存在且非空"不校验新鲜度：拿旧产物装完、回头被问"我明明改了配置
# 为什么没变化"，是本脚本唯一会浪费用户半小时的失误形态。
if [ -f "$BIN" ] && [ "$REPO/cmd/litepanel/main.go" -nt "$BIN" ]; then
    echo "bin/litepanel 比源码旧 —— 先 make build 再装（装旧产物 = 白装）" >&2
    exit 1
fi
command -v aria2c >/dev/null || { echo "缺 aria2c：apt install aria2（或 pkg install aria2）" >&2; exit 1; }
command -v systemctl >/dev/null || { echo "环境里没有 systemd（Termux 上请用 dev/run.sh 方式跑）" >&2; exit 1; }

install -Dm755 "$BIN" /usr/local/bin/litepanel

mkdir -p "$STATE" "$STATE/aria2"
chmod 700 "$STATE"
# 分块上传的暂存根（从 db_path 同级 uploads/ 推导，见 uploadRootFor）。
mkdir -p "$STATE/uploads"
chmod 700 "$STATE/uploads"

# 下载目录 = 可用容量最大的挂载点下建 downloads（设计 4.2）。awk 端剥离
# 挂载点八进制转义（\040=空格等）后再按空格切，含空格的路径不会从中间断。
A2_DIR="$(df -Pk 2>/dev/null | awk 'NR>1 {gsub(/\\0[0-7][0-7]/,"",$6); print $4, $6}' | sort -rn | head -1 | cut -d" " -f2-)/downloads"
# 根盘只读等病态环境下探测会给出不可建目录；回落到 $STATE 而不是带病开工。
mkdir -p "$A2_DIR" 2>/dev/null || { A2_DIR="$STATE/downloads"; mkdir -p "$A2_DIR"; }
chmod 700 "$A2_DIR"

# aria2 secret 与目录：env 文件是唯一来源（secret 只生成一次 —— 每次装都
# 换会让 config.toml 里的值与 aria2 实际值悄悄分家，之后所有下载调用都
# 回"未授权"；目录同理，aria2 与面板预填必须读同一份）。
if [ ! -f "$ETC/aria2.env" ]; then
    SECRET="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    mkdir -p "$ETC"
    # 值加双引号：systemd 的 EnvironmentFile 对含空格的值要求引号
    # （挂载点可能真含空格，awk 已把 df 的 \040 转义还原成空格）。
    printf 'A2_RPC_SECRET=%s\nA2_DIR="%s"\n' "$SECRET" "$A2_DIR" > "$ETC/aria2.env"
    chmod 600 "$ETC/aria2.env"
else
    SECRET="$(sed -n 's/^A2_RPC_SECRET=//p' "$ETC/aria2.env")"
    # env 已存在时目录以 env 为准，不采用本次新探测值：aria2 跑的就是
    # env 里那份，config 若填新探测值，设置页预填与实际落盘就分家了。
    ENV_DIR="$(sed -n 's/^A2_DIR=//p' "$ETC/aria2.env" | tr -d '"')"
    [ -n "$ENV_DIR" ] && A2_DIR="$ENV_DIR"
fi

# 配置：没有就从模板生成，有了绝不动（用户手改的东西 + 已生效的密码策略）。
if [ ! -f "$ETC/config.toml" ]; then
    # 顺序是实质性的：REPLACED_BY_INSTALL 是 ..._DIR 的前缀，先换短的会
    # 把目录占位符啃成 "密钥+_DIR"。长模式必须先走。
    sed -e "s|REPLACED_BY_INSTALL_DIR|$A2_DIR|" -e "s|REPLACED_BY_INSTALL|$SECRET|" \
        "$REPO/deploy/config.toml.template" > "$ETC/config.toml"
    chmod 600 "$ETC/config.toml"
    echo "已生成 $ETC/config.toml（RPC secret 与下载目录与 aria2 同源）"
fi

install -Dm644 "$REPO/deploy/litepanel.service" /etc/systemd/system/litepanel.service
install -Dm644 "$REPO/deploy/aria2.service" /etc/systemd/system/aria2.service
systemctl daemon-reload
# 单元文件与 secret 都可能在升级里变过：enable 幂等、restart 保证 aria2
# 跑的是刚装的单元（内含 new secret）。放 start 的话升级改了单元不会生效。
systemctl enable --now aria2.service
systemctl restart aria2.service
systemctl restart litepanel.service   # 首次=start，升级=restart

echo
echo "完成。首次部署读一次性初始密码："
echo "  journalctl -u litepanel -n 5"
echo "监听地址：config.toml 里 listen（默认 tailscale 自动探测，回落 127.0.0.1）"
