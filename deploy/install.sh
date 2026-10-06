#!/usr/bin/env bash
# litepanel 安装/升级脚本（root 执行）。
#
# 幂等是硬要求：`git pull && make web && make build && sudo ./deploy/install.sh`
# 就是升级路径本身，任何"第二次跑会毁坏现有部署"的行为（覆盖 config、
# 重新生成 secret）都让升级等于重装。
#
# 用法：
#   ./deploy/install.sh              安装或升级
#   ./deploy/install.sh --tls        同上，首次部署时自签 TLS 证书（README
#                                    承诺过的可选项；对已有部署不生效）
#   ./deploy/install.sh --uninstall  停服务、删二进制与单元；数据默认保留
#   ./deploy/install.sh --uninstall --purge-data   连配置与数据一起删
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO/bin/litepanel"
ETC=/etc/litepanel
STATE=/var/lib/litepanel

WITH_TLS=0
UNINSTALL=0
PURGE_DATA=0
for a in "$@"; do
    case "$a" in
        --tls) WITH_TLS=1 ;;
        --uninstall) UNINSTALL=1 ;;
        --purge-data) PURGE_DATA=1 ;;
        # 从"用法："标到下一个非注释行自动停：行号硬编码会在脚本改动后
        # 漂移（把 set -euo 或半行代码打进 --help 是迟早的事）。
        -h|--help) awk '/^# 用法：/{f=1} f&&/^#/{sub(/^# ?/,"");print;next} f{exit}' "$0"; exit 0 ;;
        *) echo "未知参数：$a（--help 看用法）" >&2; exit 1 ;;
    esac
done

[ "$(id -u)" = 0 ] || { echo "必须 root 运行（面板以 root 服务，装系统单元也需要）" >&2; exit 1; }
command -v systemctl >/dev/null || { echo "环境里没有 systemd（Termux 上请用 dev/ 那套方式跑）" >&2; exit 1; }

# ---- 卸载分支：与安装对称，但数据默认保留 ----
# 删配置/数据必须显式 --purge-data：装机时手滑同时敲 --uninstall --purge-data
# 就把数据库（密码、设置、任务历史）全清了，而"重装一下试试"是常见动作。
if [ "$UNINSTALL" = 1 ]; then
    systemctl disable --now litepanel.service 2>/dev/null || true
    systemctl disable --now aria2.service 2>/dev/null || true
    rm -f /etc/systemd/system/litepanel.service /etc/systemd/system/aria2.service
    systemctl daemon-reload
    rm -f /usr/local/bin/litepanel
    echo "已卸载（服务已停止并取消自启，二进制与单元文件已删）。"
    if [ "$PURGE_DATA" = 1 ]; then
        rm -rf "$ETC" "$STATE"
        echo "已删除 $ETC 与 $STATE（--purge-data）。"
        echo "注意：各盘根的下载目录与 .trash 不在删除范围（那是用户文件）。"
    else
        echo "保留：$ETC（配置/密钥）、$STATE（数据库与任务记录）。重装会自动继承。"
    fi
    exit 0
fi

# ---- 安装/升级 ----
[ -x "$BIN" ] || { echo "找不到 $BIN —— 先 make build" >&2; exit 1; }
# 只校验"存在且非空"不校验新鲜度：拿旧产物装完、回头被问"我明明改了配置
# 为什么没变化"，是本脚本唯一会浪费用户半小时的失误形态。
if [ -f "$BIN" ] && [ "$REPO/cmd/litepanel/main.go" -nt "$BIN" ]; then
    echo "bin/litepanel 比源码旧 —— 先 make build 再装（装旧产物 = 白装）" >&2
    exit 1
fi
command -v aria2c >/dev/null || { echo "缺 aria2c：apt install aria2" >&2; exit 1; }

install -Dm755 "$BIN" /usr/local/bin/litepanel

mkdir -p "$STATE" "$STATE/aria2"
chmod 700 "$STATE"
# 分块上传的暂存根（从 db_path 同级 uploads/ 推导，见 uploadRootFor）。
mkdir -p "$STATE/uploads"
chmod 700 "$STATE/uploads"

# 下载目录：首次部署时问用户（设计 4.2 修订：从"探测最大挂载点"改为
# 显式询问——自动探测再聪明也是在猜用户意图，而回车一下就是零歧义）。
# 三条硬规矩：已有 env（=不是首装）绝不追问，升级脚本必须无话可说；
# stdin 不是终端（管道执行/CI）绝不 read，否则脚本挂在那里等一个不存在
# 的键盘；给出的路径必须 mkdir+touch 实测可写才收——"配置里写着一个
# 写不进的目录"要等到第一个下载任务才暴露，代价完全不成比例。
ask_download_dir() {
    local def="$STATE/downloads" dir tries=0
    while :; do
        if [ -t 0 ]; then
            read -r -p "默认下载目录 [$def]（回车用默认）: " dir
            dir="${dir:-$def}"
        else
            dir="$def"
        fi
        case "$dir" in
            /*) ;;
            *)  echo "必须是绝对路径（以 / 开头）" >&2
                if [ -t 0 ] && [ "$tries" -lt 4 ]; then tries=$((tries+1)); continue; fi
                return 1 ;;
        esac
        # touch 而不是只看 mkdir：mkdir 成功但写入 EPERM 的挂载点真实存在。
        if mkdir -p "$dir" 2>/dev/null && touch "$dir/.litepanel-write-test" 2>/dev/null; then
            rm -f "$dir/.litepanel-write-test"
            A2_DIR="$dir"
            chmod 700 "$A2_DIR"
            return 0
        fi
        echo "建不出或写不进 $dir" >&2
        if [ -t 0 ] && [ "$tries" -lt 4 ]; then tries=$((tries+1)); continue; fi
        return 1
    done
}

if [ ! -f "$ETC/aria2.env" ]; then
    ask_download_dir || { echo "下载目录不可用，安装中止（未改动系统任何文件）" >&2; exit 1; }
fi

# aria2 secret 与目录：env 文件是唯一来源（secret 只生成一次 —— 每次装都
# 换会让 config.toml 里的值与 aria2 实际值悄悄分家，之后所有下载调用都
# 回"未授权"；目录同理，aria2 与面板预填必须读同一份）。
# A2_DIR 此时三种来路：首装 = ask_download_dir 刚问过；env 已存在 = 下方
# else 分支从 env 读回；都不在 = 不可能（首装必问）。
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
    TLS_ENABLED=false; TLS_CERT=''; TLS_KEY=''
    if [ "$WITH_TLS" = 1 ]; then
        command -v openssl >/dev/null || { echo "--tls 需要 openssl：apt install openssl" >&2; exit 1; }
        mkdir -p "$ETC/tls"
        # SAN 覆盖面：回环 + 本机 tailscale IPv4。监听地址换了（比如手改
        # 0.0.0.0 走局域网 IP）证书就对不上，那是 --tls 的一次性便利边界，
        # README 让人换正式证书，脚本不假装能追平所有变化。
        # SAN 必须含局域网 IP：只签 loopback+tailscale 的话，LAN 地址访问
        # 证书名对不上又被浏览器拦（真机坑）。hostname -I 列出全部本机 IPv4。
        SANS="IP:127.0.0.1,IP:::1,DNS:localhost"
        for ip in $(hostname -I 2>/dev/null); do SANS="$SANS,IP:$ip"; done
        if command -v tailscale >/dev/null; then
            TSIP="$(tailscale ip -4 2>/dev/null | head -1)"
            [ -n "$TSIP" ] && SANS="$SANS,IP:$TSIP"
        fi
        openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
            -keyout "$ETC/tls/key.pem" -out "$ETC/tls/cert.pem" \
            -days 3650 -nodes -subj "/CN=litepanel" \
            -addext "subjectAltName=$SANS" 2>/dev/null
        chmod 600 "$ETC/tls/key.pem"; chmod 644 "$ETC/tls/cert.pem"
        TLS_ENABLED=true; TLS_CERT="$ETC/tls/cert.pem"; TLS_KEY="$ETC/tls/key.pem"
        echo "已自签 TLS 证书（SAN：$SANS）。浏览器会警告一次，属自签预期。"
    fi
    # 顺序是实质性的：REPLACED_BY_INSTALL 是 ..._DIR 的前缀，先换短的会
    # 把目录占位符啃成 "密钥+_DIR"。长模式必须先走。
    sed -e "s|REPLACED_BY_INSTALL_DIR|$A2_DIR|" -e "s|REPLACED_BY_INSTALL|$SECRET|" \
        -e "s|^enabled = false|enabled = $TLS_ENABLED|" \
        -e "s|^cert_file = \"\"|cert_file = \"$TLS_CERT\"|" \
        -e "s|^key_file = \"\"|key_file = \"$TLS_KEY\"|" \
        "$REPO/deploy/config.toml.template" > "$ETC/config.toml"
    chmod 600 "$ETC/config.toml"
    echo "已生成 $ETC/config.toml（RPC secret 与下载目录与 aria2 同源）"
elif [ "$WITH_TLS" = 1 ]; then
    echo "注意：$ETC/config.toml 已存在，--tls 不改动已有部署（证书请手工配置，见 README）" >&2
else
    # secret 一致性只检不修：config 与 env 分家时下载功能整个坏且症状
    # 迷惑（"未授权"），而擅自改 config 会盖掉用户可能的有意改动。报出来
    # 让用户自己定夺。
    # 注释行也要排除：有人会把旧 secret 注释掉留在文件里，sed 若把
    # `#aria2_rpc_secret = "…"` 读进来会误报"不一致"。
    CFG_SECRET="$(sed -n 's/^[[:space:]]*aria2_rpc_secret = "\(.*\)"/\1/p' "$ETC/config.toml" | head -1)"
    if [ -z "$CFG_SECRET" ]; then
        # 缺键与值分家是同一场事故（缺=按无 token 发，全部被 aria2 拒），
        # 只在"值不一致"时报警会把这种更常见的形态漏掉——真机栽过一次。
        echo "警告：config.toml 里没有 aria2_rpc_secret（或值为空）—— 下载会报未授权。补上：" >&2
        echo "  printf '\\naria2_rpc_secret = \\"%s\\"\\n' \"\$(sed -n 's/^A2_RPC_SECRET=//p' $ETC/aria2.env)\" >> $ETC/config.toml && systemctl restart litepanel" >&2
    elif [ "$CFG_SECRET" != "$SECRET" ]; then
        echo "警告：config.toml 里的 aria2_rpc_secret 与 $ETC/aria2.env 不一致 —— 下载会报未授权。二选一改成一致后 systemctl restart litepanel aria2" >&2
    fi
fi

# ---- 启动门禁预检（真机三连坑：拒启被误读成防火墙）----
# 浏览器"拒绝连接"十有八九不是网络问题，是面板被自己的安全门禁拒之门外、
# 循环重启中（真机原话："监听地址设置成0.0.0.0会拒绝局域网访问"——实际是
# 面板压根没活着）。与其让 systemd 每 2 秒撞一次墙再翻 journal，不如装前
# 就把这三条拦下并给修复命令。门禁本身在 config.Validate，这里只做脚本
# 能看懂的部分（listen/TLS/证书文件/密码是否已设），不是完整解析器。
preflight_config() {
    local f="$ETC/config.toml" listen enabled cert key db bad=0
    listen="$(sed -n 's/^listen[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' "$f" | head -1)"
    enabled="$(sed -n 's/^enabled[[:space:]]*=[[:space:]]*\(.*\)/\1/p' "$f" | head -1 | tr -d '[:space:]')"
    cert="$(sed -n 's/^cert_file[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' "$f" | head -1)"
    key="$(sed -n 's/^key_file[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' "$f" | head -1)"
    if { [ "$listen" = "0.0.0.0" ] || [ "$listen" = "::" ]; } && [ "$enabled" != "true" ]; then
        echo "门禁：listen=$listen 但未启用 TLS，面板会拒绝启动（设计 4.1）。修复：给 [tls] 配证书（README 的 openssl 命令），或改回 listen = \"tailscale\"。" >&2
        bad=1
    fi
    if [ "$enabled" = "true" ]; then
        # -s 而非 -f：cert_file="" 或空文件与不存在同罪，ListenAndServeTLS 全拒。
        if [ ! -s "$cert" ] || [ ! -s "$key" ]; then
            echo "门禁：tls enabled=true 但证书/私钥缺失或为空（cert=$cert key=$key）。一条生成含本机所有 IP 的自签证书：" >&2
            echo "  go run $REPO/deploy/mkcert.go /etc/litepanel/tls  # 或 README『HTTPS』里的 openssl 命令" >&2
            bad=1
        fi
    fi
    # 0.0.0.0 还要求已设密码。密码在 bcrypt 库里读不出明文，但"设过没有"
    # 查得到；没有 sqlite3 时放手交给面板自己的报错，不强求预检全覆盖。
    db="$(sed -n 's/^db_path[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' "$f" | head -1)"
    if { [ "$listen" = "0.0.0.0" ] || [ "$listen" = "::" ]; } && command -v sqlite3 >/dev/null && [ -f "$db" ]; then
        if ! sqlite3 "$db" "select 1 from settings where key='password_hash' limit 1" 2>/dev/null | grep -q 1; then
            echo "门禁：listen=$listen 但数据库里还没设过密码——首次部署必须先走 tailscale/127.0.0.1 登录改密，之后再开 0.0.0.0（这是门禁的本意：防裸奔，不是拦初见）。" >&2
            bad=1
        fi
    fi
    return "$bad"
}
preflight_config || { echo "预检未通过，未启动任何服务（其余安装动作已完成，可修复配置后重跑）。" >&2; exit 1; }

install -Dm644 "$REPO/deploy/litepanel.service" /etc/systemd/system/litepanel.service
install -Dm644 "$REPO/deploy/aria2.service" /etc/systemd/system/aria2.service
systemctl daemon-reload
# 单元文件与 secret 都可能在升级里变过：enable 幂等、restart 保证 aria2
# 跑的是刚装的单元。aria2 失败只警告不退出：面板对 aria2 掉线有专门的
# 健康提示（下载页横幅），不该让"下载暂时不可用"把整个面板安装判死。
systemctl enable aria2.service litepanel.service
if ! systemctl restart aria2.service; then
    echo "警告：aria2 启动失败，最近日志：" >&2
    journalctl -u aria2 -n 5 --no-pager >&2 || true
fi
systemctl restart litepanel.service   # 首次=start，升级=restart

# restart 返回 0 不等于活着：Type=simple 下进程可能在 exec 之后立刻退出。
# 等两秒实际检查，失败直接甩日志 —— 让用户装完就看见问题，而不是打开
# 浏览器连不上再回来猜。
sleep 2
FAILED=0
for svc in litepanel aria2; do
    if ! systemctl is-active --quiet "$svc.service"; then
        echo "错误：$svc.service 未处于 active 状态，最近日志：" >&2
        journalctl -u "$svc" -n 8 --no-pager >&2 || true
        FAILED=1
    fi
done
[ "$FAILED" = 0 ] || exit 1

echo
echo "完成，两个服务都 active 且已开机自启。"
echo "首次部署读一次性初始密码："
echo "  journalctl -u litepanel -n 5"
echo "监听地址：config.toml 里 listen（默认 tailscale 自动探测，回落 127.0.0.1）"
