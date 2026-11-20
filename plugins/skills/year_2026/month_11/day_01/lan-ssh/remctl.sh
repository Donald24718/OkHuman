#!/bin/bash
# remctl.sh — 跨机状态的唯一入口（lan-ssh §10）。只依赖 ssh/curl/ss/systemctl/nvidia-smi/awk/base64。
# 用法:
#   remctl.sh matrix [host...]              主机×单元×端口×GPU归属×脱管进程 的矩阵（默认 local ey）
#   remctl.sh owns <端口或名字> [host...]   谁提供这个端口/进程、归哪个单元管、正确的停法
#   remctl.sh kill <host> <模式>            pgrep → kill -9 → 再 pgrep 复核；没杀干净就非零退出
#   remctl.sh pre <host> [远端路径...]      跑远程命令前的预检：免密/sudo/文件在不在/非 ASCII 路径
# host 只用别名：local | ey(192.168.1.3) | sd-box(192.168.1.1)。写 IP 会绕开别名里的密钥 → 认证失败会被误当网络故障。
set -uo pipefail
PAT='okhuman|strata|ninfer|scout|okmon|lan-bridge|qq-bridge|asr|comfy'
PL_local="8451 8452 8080 8081 8480 8496 8601 8602"
PL_ey="8451 8452 8080 8081 8090 8480 8496 8601 8602 8603"

# rsh <host> <脚本>：脚本 base64 后远端解码执行 —— 免掉引号嵌套与非 ASCII 被远端转义
rsh(){ local h="$1" code="$2"
  if [ "$h" = local ]; then bash -c "$code"; else
    printf '%s' "$code" | base64 -w0 | { read -r b; ssh -o BatchMode=yes -o ConnectTimeout=8 "$h" "echo $b | base64 -d | bash -s"; }
  fi; }

CRED='plugins/skills/year_2026/month_10/day_03/credentials/SKILL.md'
sudo_rsh(){ local h="$1" code="$2" pw=""
  [ -f "$CRED" ] && pw=$(awk -F'|' '/192\.168\.1\.1/{gsub(/[ `]/,"",$5); print $5}' "$CRED" | head -1)
  if [ -z "$pw" ]; then echo "   （拿不到口令：用 credentials §10 现场取）"; return; fi
  printf '%s\n' "$pw" | ssh -o BatchMode=yes -o ConnectTimeout=8 "$h" 'sudo -S -k bash -c "$(base64 -d <<< $(cat))" ' 2>/dev/null || \
  { b=$(printf '%s' "$code" | base64 -w0); printf '%s\n' "$pw" | ssh -o BatchMode=yes "$h" "sudo -S -k bash -c 'echo $b | base64 -d | bash'"; }
}

units(){ rsh "$1" 'if [ "'"${IS_LOCAL:-0}"'" = 1 ]; then systemctl --user list-units --type=service --state=running --no-pager 2>/dev/null | grep -iE "okhuman|strata|ninfer|scout|okmon|lan-bridge|qq-bridge"; else systemctl list-units --type=service --state=running --no-pager 2>/dev/null | grep -iE "okhuman|strata|ninfer|scout|okmon|lan-bridge|asr"; fi' \
  | awk -v m="$MGR" 'NF>1{print "   ["m"] "$1"  "$4}'; }

ports(){ local h="$1"; shift
  for p in "$@"; do
    rsh "$h" "b=000
for pa in health v1/models api/state '' x; do
  c=\$(curl -s -o /dev/null -m3 -w '%{http_code}' \"127.0.0.1:$p/\$pa\" 2>/dev/null)
  case \"\$c\" in 2*|3*) b=\"\$c\"; break;; 4*) [ \"\$b\" = 000 ] && b=\"\$c\";; esac
done
l=\$(ss -tlnp 2>/dev/null | grep -c \":$p \")
case \"\$b\" in 000) if [ \"\$l\" -gt 0 ]; then t=\"有监听但无HTTP应答\"; else t=\"无监听\"; fi;;
  4*) t=\"活（探测端点都没有，4xx）\";; *) t=\"应答 ok\";; esac
printf '   :%s  %-24s 码=%s 监听=%s\n' \"$p\" \"\$t\" \"\$b\" \"\$l\""
  done; }

gpus(){ rsh "$1" 'nvidia-smi --query-gpu=index,name,memory.used,memory.total --format=csv,noheader 2>/dev/null | while IFS=, read -r i n u t; do printf "   GPU%s %-22s 已用 %s /%s\n" "$i" "$n" "$u" "$t"; done
nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader 2>/dev/null | while IFS=, read -r pid mem; do
  u=$(sed -n "s#.*slice/\(.*\)\.service.*#\1#p" /proc/"$pid"/cgroup 2>/dev/null | head -1)
  [ -z "$u" ] && u="非单元进程("$(ps -o comm= -p "$pid" 2>/dev/null)") —— 谁起的要看它，别停错"
  printf "   显存 %s pid=%s 归属=%s\n" "$mem" "$pid" "$u"
done'; }

strays(){ rsh "$1" 'ps -eo pid,ppid,etime,cmd --no-headers 2>/dev/null | awk "\$2==1"' \
  | grep -vE '/usr/bin/(blueman|pulse|nm-|gjs|gnome|xfdesktop)|xfce4|xdg-desktop|/usr/lib/' \
  | grep -E 'OkHuman|nohup|while |sleep [0-9]|tail -f|ffmpeg|python|node' \
  | awk '{ if ($3 ~ /^([0-9]+-|[0-9]+:[0-9][0-9]:)/) print "   长期脱管(≥1h) 先确认用途/属主: " $0; else print "   脱管中: " $0 }' | head -8; }

matrix(){ local hs=("$@"); [ ${#hs[@]} -eq 0 ] && hs=(local ey)
  for h in "${hs[@]}"; do
    echo "== $h =="
    echo " -- 运行中的服务单元（[user]/[system] 决定用哪种 systemctl）"
    if [ "$h" = local ]; then IS_LOCAL=1 MGR=user units "$h"; else IS_LOCAL=0 MGR=system units "$h"; fi
    echo " -- 端口"
    case "$h" in local) ports "$h" $PL_local;; ey) ports "$h" $PL_ey;; *) rsh "$h" 'ss -tlnp 2>/dev/null | awk "{print \"   \"\$4}" | sort -u | head -12';; esac
    [ "$h" = sd-box ] || { echo " -- GPU 与显存归属"; gpus "$h"; }
    echo " -- 我起的脱管进程"; strays "$h"
  done; }

owns(){ local t="$1"; shift; local hs=("$@"); [ ${#hs[@]} -eq 0 ] && hs=(local ey)
  local pat; if [[ "$t" =~ ^[0-9]+$ ]]; then pat="(--port[= ]$t|:$t|port $t)"; else pat="($t)"; fi
  for h in "${hs[@]}"; do
    echo "== $h：谁提供 $t / 谁管它 =="
    local raw pids=""
    if [[ "$t" =~ ^[0-9]+$ ]]; then
      raw=$(rsh "$h" "ss -tlnp 2>/dev/null | grep \":$t \"")
      case "$raw" in *"users:("*|"") ;; *) echo "   非 root 的 ss 看不到进程属主 → 走 sudo（口令取自 credentials §10，不回显）"; raw=$(sudo_rsh "$h" "ss -tlnp | grep \":$t \"");; esac
      [ -n "$raw" ] && printf '%s\n' "$raw" | sed 's/^/   监听: /'
      pids=$(printf '%s' "$raw" | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u | tr '\n' ' ')
    fi
    local pg; pg=$(rsh "$h" "pgrep -f '$pat' 2>/dev/null | while read -r q; do c=\$(tr '\\0' ' ' < /proc/\$q/cmdline 2>/dev/null); case \"\$c\" in *remctl*) ;; *) echo \"\$q\";; esac; done")
    pids="$pids $(printf '%s' "$pg" | tr '\n' ' ')"
    pids=$(printf '%s' "$pids" | tr ' ' '\n' | grep -E '^[0-9]+$' | sort -u | tr '\n' ' ')
    if [ -n "${pids// /}" ]; then
      rsh "$h" "for pid in $pids; do
        printf '   pid=%s 属主=%s\n' \"\$pid\" \"\$(ps -o user= -p \$pid 2>/dev/null)\"
        printf '     命令: %s\n' \"\$(ps -o args= -p \$pid 2>/dev/null | cut -c1-160)\"
        u=\$(sed -n 's#.*[/.]\(.*\)\.service.*#\1#p' /proc/\$pid/cgroup 2>/dev/null | head -1)
        [ -z \"\$u\" ] && u='非服务单元（桌面会话进程，或 agent 用 setsid 起的——重启主程序会连带带走它）'
        printf '     单元归属: %s\n' \"\$u\"
      done"
    else echo "   没有匹配到进程：端口没监听、模式也不匹配 —— 别据此断定「它不在」，换 owns 的名字再试一次"
    fi
    rsh "$h" "for m in system user; do f=''; [ \"\$m\" = user ] && f='--user'; systemctl \$f list-units --all --no-pager 2>/dev/null | grep -iE '$pat' | awk -v m=\"\$m\" 'NF>2{print \"   单元[\"m\"]:\", \$1, \$3, \$4}'; done"
    echo "   正确停法: user 单元 = systemctl --user stop|restart；system 单元（root 跑）= sudo systemctl stop|restart；非单元进程只能 kill 并复核"
  done; }

killjob(){ local h="$1" pat="$2"
  echo "== 在 $h 上处理 /$pat/ =="
  rsh "$h" "targets=\$(pgrep -f '$pat' 2>/dev/null | while read -r q; do c=\$(tr '\\0' ' ' < /proc/\$q/cmdline 2>/dev/null); case \"\$c\" in *remctl*|*base64*) ;; *) echo \$q;; esac; done)
if [ -z \"\$targets\" ]; then echo '   没有匹配 —— 先确认模式名与属主。「我打不到」不等于「它不在」：root 的单元进程你 kill 不动，得走 systemctl'; exit 0; fi
for pid in \$targets; do printf '   匹配: %s %s\n' \"\$pid\" \"\$(ps -o args= -p \$pid 2>/dev/null | cut -c1-140)\"; done
for pid in \$targets; do kill -9 \$pid 2>/dev/null || echo \"   kill 失败 pid=\$pid → 属主不是你，走 systemctl\"; done
sleep 2
left=\$(pgrep -f '$pat' 2>/dev/null | while read -r q; do c=\$(tr '\\0' ' ' < /proc/\$q/cmdline 2>/dev/null); case \"\$c\" in *remctl*|*base64*) ;; *) echo \$q;; esac; done)
if [ -n \"\$left\" ]; then for pid in \$left; do printf '   仍有存活: %s %s\n' \"\$pid\" \"\$(ps -o args= -p \$pid 2>/dev/null | cut -c1-140)\"; done; exit 3; fi
echo '   已复核：无存活'"
  [ $? -eq 3 ] && echo "   ⚠ 没杀干净：先 owns 看归属 → systemctl stop；不要继续起下一批（上次两条线多跑了 40 分钟）"
  return 0; }

pre(){ local h="$1"; shift
  echo "== 预检 $h =="
  [ "$h" = local ] && { echo "   本机：跳过网络预检"; return; }
  if ssh -o BatchMode=yes -o ConnectTimeout=6 "$h" 'echo "   免密 ok: $(hostname)"'; then :; else echo "   ⚠ 免密失败：用 credentials §10 的 sshpass 写法；这不是网络故障，别接着往网络上找原因"; fi
  rsh "$h" 'sudo -n true 2>/dev/null && echo "   sudo 免密可用" || echo "   sudo 需口令：system 单元操作用 credentials §10 口令 + sudo -S -k"'
  for p in "$@"; do
    rsh "$h" "test -e '$p' && echo '   远端存在: $p' || echo '   ⚠ 远端不存在: $p —— 先传再跑（传前先测目标目录可写）'"
    printf '%s' "$p" | LC_ALL=C grep -q '[^ -~]' && echo "   路径含非 ASCII：rsync 的远端路径会被转义而失败 → 用 tar-over-ssh（tar cf - -C src 相对路径 | ssh $h 'tar xf - -C 目标'）"
  done; }

case "${1:-}" in
  matrix) shift; matrix "$@";;
  owns) shift; owns "$@";;
  kill) shift; killjob "$@";;
  pre) shift; pre "$@";;
  *) sed -n '2,10p' "$0"; exit 2;;
esac
