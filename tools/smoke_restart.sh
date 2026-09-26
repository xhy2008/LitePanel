#!/bin/bash
# D5 的端到端验收：面板被 kill -9 之后，终端会话必须还活着、任务继续跑；
# 面板重启后必须把它认回来，并且 WS 链路一切正常。
#
# 全程 curl + 真 tmux + 编译出来的二进制，不经任何 Go 测试替身：
# Go 测试能证明模块逻辑，证不了"配置加载 → 路由装配 → 静态资源 →
# tmux 桥接"这条真实启动路径。
set -u
cd "$(dirname "$0")/.."
# 配置与端口都可覆盖：脚本里的 API 地址要和配置里的端口一致，
# 各写一处就会在改端口后静默打到别的服务上。
CFG="${CFG:-dev/t6.toml}"
API="${API:-http://127.0.0.1:18080}"
H1='Content-Type: application/json'
H2='X-Requested-With: litepanel'
PASS='Smoke-Test-2026!'
LOG="${LOG:-dev/t7.log}"

up() { curl -s -o /dev/null "$1"; }

# 自己拉起面板：否则脚本会死在"登录失败"上，而真正的原因是面板没起。
if ! up "$API/"; then
  echo "- 面板未运行，拉起"
  (setsid ./litepanel --config "$CFG" >> "$LOG" 2>&1 &)
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    up "$API/" && break
    sleep 1
  done
  up "$API/" || { echo "FAIL 面板起不来，见 $LOG"; exit 1; }
fi

# token 从 Set-Cookie 响应头取。不用 -w '%{cookie}'：那要 curl 8.x，
# 旧版会报 unknown --write-out variable，于是这里静默拿到空 token，
# 后面每个断言都因为"未登录"而红，看不出真正原因。
tok() {
  local p="${1:-$PASS}"
  curl -s -D - -o /dev/null -X POST "$API/api/login" -H "$H1" -H "$H2" \
    -d "{\"password\":\"$p\"}" \
    | tr -d '\r' \
    | sed -n 's/^[Ss]et-[Cc]ookie:[[:space:]]*lp_session=\([^;]*\).*/\1/p' \
    | head -1
}

login() {
  TOKEN=$(tok)
  [ -n "$TOKEN" ] && return 0
  # 登录不上有两种原因，排查方向完全不同：还挂着一次性初始密码，
  # 或者改过密但 PASS 不对。/api/me 不要求认证就报 must_change_password。
  if curl -s -H "$H2" "$API/api/me" | grep -q '"must_change_password":true'; then
    echo "- 还挂着初始密码，用启动日志里的一次性密码改掉"
    OTP=$(grep -oE '一次性初始密码[^：]*：.*' "$LOG" | tail -1 | sed 's/.*：//')
    [ -n "$OTP" ] || { echo "FAIL 日志里找不到一次性密码"; exit 1; }
    TOKEN=$(tok "$OTP")
    [ -n "$TOKEN" ] || { echo "FAIL 用一次性密码也登不上"; exit 1; }
    # 改密会吊销全部会话（M7-T5），所以改完要重新登录
    curl -s -o /dev/null -b "lp_session=$TOKEN" -X POST "$API/api/password" \
      -H "$H1" -H "$H2" -d "{\"old\":\"$OTP\",\"new\":\"$PASS\"}"
    TOKEN=$(tok)
  fi
  [ -n "$TOKEN" ] || { echo "FAIL 登录失败（密码不对？面板没起？）"; exit 1; }
}

api() { curl -s -b "lp_session=$TOKEN" -H "$H2" "$@"; }

login
echo "OK 登录"

SID=$(api -X POST "$API/api/term/sessions" -H "$H1" -d '{"title":"重启验证"}' \
  | tr ',' '\n' | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)
[ -n "$SID" ] || { echo "FAIL 建会话失败"; exit 1; }
NAME="lp-$SID"
echo "OK 会话 id=$SID tmux=$NAME"

# 用 tmux 直接起一个每秒都在动的长任务：要验的正是"面板不在了它也照跑"，
# 所以故意不经 WS/桥接注入。
#
# 目标是 pane（name:0.0），不是会话：send-keys / capture-pane 只认 pane 目标，
# 而且**不接受 = 前缀**（会报 can't find pane）。会话级命令才用 = 精确匹配：
# has-session / list-clients / kill-session。混用的话 = 写法在这里直接失败。
PANE="$NAME:0.0"
tmux send-keys -t "$PANE" 'for i in $(seq 100000); do echo "tick-$i"; sleep 1; done' Enter
sleep 3
BEFORE=$(tmux capture-pane -p -t "$PANE" | grep -c 'tick-')
[ "$BEFORE" -gt 0 ] || { echo "FAIL 任务没跑起来"; exit 1; }
echo "OK 面板重启前已输出 $BEFORE 行"

pkill -9 -f "litepanel --config $CFG"
sleep 1
tmux has-session -t "=$NAME" 2>/dev/null \
  || { echo "FAIL 面板被杀时把 tmux 会话一起带走了"; exit 1; }
echo "OK 面板被 kill -9 后 tmux 会话仍在"

sleep 3
AFTER=$(tmux capture-pane -p -t "$PANE" | grep -c 'tick-')
[ "$AFTER" -gt "$BEFORE" ] \
  || { echo "FAIL 面板不在时任务停了：$BEFORE → $AFTER"; exit 1; }
echo "OK 面板不在时任务继续跑（$BEFORE → $AFTER 行）"

(setsid ./litepanel --config "$CFG" >> "$LOG" 2>&1 &)
for _ in 1 2 3 4 5 6 7 8 9 10; do up "$API/" && break; sleep 1; done
up "$API/" || { echo "FAIL 重启后面板没起来，见 $LOG"; exit 1; }
login

LIST=$(api "$API/api/term/sessions")
# 只锺定 "id":<SID> 这一个字段（带上逗号排除 12 命中 123）。
# 不写 *"[{\"id\":$SID,"* 这种形式：Go 的字段输出顺序改了就会误报失败，
# 而面板根不依赖那个顺序。
case "$LIST" in
  *"\"id\":$SID,"*) ;;
  *) echo "FAIL 重启后没认回会话: $LIST"; exit 1 ;;
esac
echo "OK 重启后列表认回了该会话"
case "$LIST" in
  *'"alive":true'*) echo "OK alive=true（对账按 tmux 现状刷新）" ;;
  *) echo "FAIL 重启后 alive 没刷成 true: $LIST"; exit 1 ;;
esac

# 新进程里 WS 必须还能收发：Go 冒烟脚本对刚重启的进程重走一遍
# 登录 → 建会话 → WS 收发 → 双设备同步。
if go run tools/smoke/main.go "$PASS"; then
  echo "OK 重启后 WS 链路正常"
else
  echo "FAIL 重启后 WS 链路不通"; exit 1
fi

echo "全部通过"
