#!/bin/bash
# nxguard.sh — 跑命令期间临时断开所有已连接的 NoMachine 会话。
# 背景更新（2026-09-13 晚，二次排查后）：原以为的根因"NoMachine 光标同步"已被实测洗清
#   （strace nxnode：空闲只发 30s Ping，零 X 写入零指针同步；指针全程零漂移）。
#   真正根因 = 别的最大化窗口（Thunar）盖住点击点 + xfwm4 windowactivate 置顶延迟，
#   详见 LESSONS.md。本脚本降级为备而不用（用户客户端真在动鼠标时可临时用）。
# 用法: nxguard.sh <命令...>
# 例:   nxguard.sh plugins/computer/xusectl click 48 29
set -u
PW=${NX_SUDO_PASSWORD:-}
NX=/usr/NX/bin/nxserver
run() { echo "$PW" | sudo -S bash -c "$NX $*" 2>/dev/null; }

mapfile -t SIDS < <(run --list | awk 'NF>=5 && $1!="Display" && $1!~/^-+/ && $3!="-" {print $4}')
if [ ${#SIDS[@]} -eq 0 ]; then
  echo "(无已连接的 NoMachine 会话，直接执行)"
  exec "$@"
fi
for s in "${SIDS[@]}"; do run --disconnect --session "$s"; done
echo "已断开 NoMachine 会话: ${SIDS[*]}"
"$@"
rc=$?
for s in "${SIDS[@]}"; do run --resume --session "$s"; done
echo "已恢复 NoMachine 会话"
exit $rc
