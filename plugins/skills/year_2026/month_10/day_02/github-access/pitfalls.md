## 四、实测踩过的坑（别再踩）

0. **`~/.ssh/config` 里硬写 `ProxyCommand nc -X 5 -x 127.0.0.1:1080` 是个陷阱**（2026-10-06 实锤）：
   隧道没起时**所有** git 操作报 `Connection closed by UNKNOWN port 65535` +
   `fatal: 无法读取远程仓库`，极易误判成"GitHub 不通 / 密钥失效"。实际直连一直通。
   → 已改成 `github.com` 直连 + `github-via-proxy` 兜底两个别名。
0b. **SSH-over-443 必须把 `HostName` 写成 `ssh.github.com`**；`git@github.com` 配 `-p 443` 不通。
0c. **裸 TCP 读跳板机 SSH banner 会返回空**，但 sshd 正常（完整握手与认证都走通）→ 别据此判定服务器死了。
0d. **要密码时别写进命令行**（进进程列表与历史）：`sshpass -f <临时600文件>`，用完 `shred -u`。
   探测认证方式用 `ssh -o BatchMode=yes`（不会挂在密码提示上，直接给 `Permission denied (publickey,password)`）。

1. **GitHub 不支持密码 push**：`Password authentication is not supported for Git operations`。必须 SSH key 或 token。
2. **chromium 不吃 `all_proxy`**：起 browserd 时只设 `all_proxy=socks5://…` → `ERR_EMPTY_RESPONSE`。要浏览器走代理，得给一个**本地 HTTP 代理**并设 `http_proxy=http://127.0.0.1:8118`。
3. **自写 HTTP→SOCKS5 代理时，SOCKS5 握手后必须把 BND.ADDR + BND.PORT 全吞掉**（ATYP=1 → 6 字节；ATYP=3 → `len+2`；ATYP=4 → 18 字节）。少吞 2 字节 → 残留字节污染 TLS 流 → 连接永久超时（症状：代理进程活着、SOCKS 直连正常，但经代理 000）。
4. **GitHub 新版设置页大量是 React**：`/settings/tokens/new`（classic PAT）curl 抓不到 scope 字段，浏览器 eval 填受控组件也不可靠 → **别走 PAT 路线**。
5. **但 `/settings/ssh/new` 的表单是传统 form**：`form action=/settings/ssh`，字段 `ssh_key[title]` / `ssh_key[key]`（注意不是 `/settings/keys`，正则别写错）→ 浏览器 `eval` 填值 + `form.submit()` 一次成功，返回 `You have successfully added the key ...`。
6. **curl 登录 GitHub 可行**（本次成功，未触发设备验证码）：hidden fields 要全带（含 `timestamp` / `timestamp_secret`）；`authenticity_token` 提取时**别把结尾引号带进去**（否则 422 "Oh no · GitHub"）；成功标志 = cookie 出现 `logged_in`，随后 `/settings/tokens` 返回 200。
6b. **登录页字段名已变（2026-10-14 实测）**：用户名是 **`#login_field`**（`name=login`），
   不是 `input[name=login]`；密码 `#password`；提交按钮是 **`input[name=commit]`**（value=`Sign in`）。
   页面上还有 `required_field_xxxx`（honeypot，真人/真浏览器不用手填）和一堆 `webauthn-*` hidden。
   用旧选择器会报 `selector 未匹配到元素: #login`，而 `#password` 却能匹配——极易误判成"页面没加载全"。
7. **`browserctl` 有 `file_upload`**：上传类网页表单能用 browser 插件做（release asset 仍优先走 API+token）。
   动作集随版本漂移，别照抄任何文档里的列表，现取：
   `grep -o 'case "[a-z_]*"' "plugins/browser/cmd/browserctl/main.go"`（2026-10-28 取到 33 个，含 `click_at/cookies/info/count/errors/meta`）。
8. **`GIT_SSH_COMMAND` 里嵌套引号会吞掉 `-o` 参数**（曾导致 `Host key verification failed`）→ 写进 `~/.ssh/config` 更可靠。
9. **`browserctl` 必须串行调用，不能并发**（2026-10-14 实锤）：多个 `browserctl`
   同时连同一个 browserd 会互抢 CDP 会话，**全部**卡到 30s 超时，报
   `CDP Page.enable 响应超时（30s）——实例可能已退出`。**症状骗人**：实例其实活着
   （`pgrep -a browserd` 在、chromium 主进程也在）。判据：先 `pgrep -a browserctl` 看有没有
   别的客户端在飞，有就等它结束再发下一条。一次 `open` 一条命令、拿到回显再发下一条。
10. **长期不用的无头 chromium 会 CDP hang**（2026-10-14 实测：某 `--keep` 实例跑了 1 天 15 小时后
    `Page.enable` 永久超时），而 browserd 进程仍活着 → 同样报"实例可能已退出"。
    处置：`kill <browserd_pid> <chromium_pid>`（两者都在 `/tmp/okhuman-browser-<port>.state` 的
    JSON 里）后重起，或换个新端口自己起一个（别动别人起的实例）。
11. **机器 load 高时 GitHub 这类 React 重页面更容易超时**（`open` 可能要 20-50s）。别据此判定"GitHub 不通"——
    同时刻 `curl -o /dev/null -w '%{http_code} %{time_total}' https://github.com` 才是判据。
