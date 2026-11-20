## 三、每次操作的步骤（**先直连，不通再隧道**）

```bash
# 1) 先测直连——通则到此为止，不要碰代理
curl -s -m 8 -o /dev/null -w "%{http_code}\n" https://api.github.com   # 200 = 直连可用
timeout 20 ssh -T git@github.com                                       # Hi <用户名>! = SSH 直连可用

# 2) 直连不通 → 起隧道（免密，幂等；已在听就不重复起）
bash scripts/start-tunnel.sh
#   等价手工写法：
#   (setsid ssh -N -D 1080 -o BatchMode=yes cloud-proxy < /dev/null > /tmp/cloud-proxy.log 2>&1 &)

# 3) 走隧道干活
git clone ssh://git@github-via-proxy/<owner>/<repo>.git
git -C <仓> push ssh://git@github-via-proxy/<owner>/<repo>.git main
curl -s -m 15 --socks5-hostname 127.0.0.1:1080 -o /dev/null -w "%{http_code}\n" https://github.com
export ALL_PROXY=socks5h://127.0.0.1:1080        # 给非 git 的 HTTP 客户端
```

隧道起来后实测：SOCKS5 出网 `api.github.com` **200 / 1.1s**；
`git ls-remote ssh://git@github-via-proxy/Donald24718/strata-v100.git` 正常返回 SHA。

**判跳板机死活用三连**（别只凭一次失败下结论）：
`ping -c3 <跳板机 IP>` → `timeout 10 bash -c '</dev/tcp/<跳板机 IP>/22'` →
`ssh -o BatchMode=yes cloud-proxy 'echo OK'`。三连都断才怀疑欠费/关机，**报主人**，
不要自己拿密码重试。
