#!/usr/bin/env bash
# 幂等起 SOCKS5 隧道：已在 1080 监听就什么都不做。
set -u
if ss -ltn 2>/dev/null | grep -q '127.0.0.1:1080 '; then
  echo "tunnel already up on 1080"; exit 0
fi
(setsid ssh -N -D 1080 -o BatchMode=yes cloud-proxy < /dev/null > /tmp/cloud-proxy.log 2>&1 &)
sleep 4
if ss -ltn 2>/dev/null | grep -q '127.0.0.1:1080 '; then
  echo "tunnel up"; curl -s -m 15 --socks5-hostname 127.0.0.1:1080 -o /dev/null -w "via proxy: %{http_code} in %{time_total}s\n" https://api.github.com
else
  echo "FAILED to start; log:"; tail -5 /tmp/cloud-proxy.log; exit 1
fi
