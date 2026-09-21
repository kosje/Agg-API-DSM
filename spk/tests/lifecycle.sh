#!/bin/sh
# SPK 生命周期自测：不需要真的装到 DSM 上，用目录模拟安装布局即可。
#
#   ./spk/tests/lifecycle.sh <spk 文件路径>
#
# 覆盖上游踩过的坑：
#   - 生命周期脚本往 stderr 写东西 -> DSM 会弹成错误对话框
#     （一次正常的启动却弹报错，很误导）
#   - 端口被占用时启动失败，但脚本仍报成功
#   - 向导口令文件读完不删 -> 每次重启都重置，冲掉用户在控制台改的口令
set -eu

SPK="${1:?用法: lifecycle.sh <spk 文件路径>}"
PKG=agg-api-dsm
PORT=4444
DEST="/var/packages/$PKG/target"
VAR="/var/packages/$PKG/var"
SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
FAIL=0

ok()   { printf "  [OK] %s\n" "$1"; }
bad()  { printf "  [FAIL] %s\n" "$1"; FAIL=$((FAIL + 1)); }

echo "==> 准备安装布局"
# 先清掉上一轮可能残留的进程：它会占着端口，让这次 start 静默失败，
# 表现为「日志正常但 status 报未运行」这种很难查的现象。
pkill -f "$DEST/bin/agg-api" 2>/dev/null || true
pkill -f "agg-api -host" 2>/dev/null || true
sleep 1

rm -rf "/var/packages/$PKG"
mkdir -p "$DEST" "$VAR"
tar xzf "$SPK" -C /tmp --wildcards 'package.tgz' 2>/dev/null || \
    tar xOf "$SPK" package.tgz | tar xzf - -C "$DEST"
chmod 755 "$DEST/bin/agg-api" 2>/dev/null || true

# 模拟 postinst 落下向导口令
printf 'Wizard-Password-2026\n' > "$VAR/admin-password-reset"

export SYNOPKG_PKGDEST="$DEST"
export SYNOPKG_PKGVAR="$VAR"
export SYNOPKG_TEMP_LOGFILE="/tmp/$PKG.err"

echo "==> start"
ERR="$(sh "$SCRIPT_DIR/scripts/start-stop-status" start 2>&1 >/dev/null || true)"
if [ -n "$ERR" ]; then
    bad "start 往 stderr 写了内容（DSM 会弹错误对话框）：$ERR"
else
    ok "start 无 stderr 输出"
fi

echo "==> status"
sh "$SCRIPT_DIR/scripts/start-stop-status" status && ok "status 报告运行中" || bad "status 未报告运行中"

sleep 1
echo "==> 校验副作用"
[ -s "$VAR/data/app.log" ] && ok "日志已生成" || bad "日志未生成"
grep -q "listening on" "$VAR/data/app.log" && ok "日志含 listening on" || bad "日志缺 listening on"
grep -q '"hash"' "$VAR/data/config.json" 2>/dev/null && ok "向导口令已写入配置" || bad "向导口令未生效"
[ ! -f "$VAR/admin-password-reset" ] && ok "向导口令文件已消费删除" || bad "向导口令文件残留（会导致每次重启重置口令）"

echo "==> 重复 start 应当幂等"
sh "$SCRIPT_DIR/scripts/start-stop-status" start >/dev/null 2>&1 && ok "重复 start 返回 0" || bad "重复 start 失败"
N="$(pgrep -f "$DEST/bin/agg-api" | wc -l)"
[ "$N" = "1" ] && ok "只有一个进程实例" || bad "进程数异常：$N"

echo "==> stop"
sh "$SCRIPT_DIR/scripts/start-stop-status" stop >/dev/null 2>&1 && ok "stop 返回 0" || bad "stop 失败"
sh "$SCRIPT_DIR/scripts/start-stop-status" status >/dev/null 2>&1 && bad "stop 后 status 仍报运行中" || ok "stop 后 status 报告已停止"

echo "==> 端口被占用时应报失败"
# 占住端口，再启动一次
python3 -c "
import socket, time
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('0.0.0.0', $PORT)); s.listen(1); time.sleep(20)
" &
HOLDER=$!
sleep 1
if sh "$SCRIPT_DIR/scripts/start-stop-status" start >/dev/null 2>&1; then
    bad "端口被占用时仍报启动成功"
    sh "$SCRIPT_DIR/scripts/start-stop-status" stop >/dev/null 2>&1 || true
else
    ok "端口被占用时正确报失败"
fi
kill "$HOLDER" 2>/dev/null || true
wait "$HOLDER" 2>/dev/null || true

rm -rf "/var/packages/$PKG" /tmp/package.tgz "/tmp/$PKG.err"
echo
if [ "$FAIL" -eq 0 ]; then
    echo "==> 全部通过"
else
    echo "==> $FAIL 项失败"
    exit 1
fi
