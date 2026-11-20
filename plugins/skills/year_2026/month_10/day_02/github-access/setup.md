## 二、一次性配置（2026-10-02 建，2026-10-06 重写 config + 装跳板机密钥；正常情况不用重做）

- 本机密钥：`~/.ssh/okhuman_gh`（ed25519，comment `okhuman-8451-agent`）
- 该公钥已加进 GitHub 账号：**Settings → SSH and GPG keys，标题 `okhuman-8451-agent`，Read/write**
- `~/.ssh/config`（**2026-10-06 重写，改前备份 `~/.ssh/config.bak-20261006`**）三个别名：

  ```
  Host github.com          # 默认直连，不依赖隧道
    HostName github.com
    User git
    IdentityFile ~/.ssh/okhuman_gh
    IdentitiesOnly yes
    ProxyCommand none
    StrictHostKeyChecking accept-new

  Host github-via-proxy    # 兜底：隧道起来后用这个别名
    HostName github.com
    User git
    IdentityFile ~/.ssh/okhuman_gh
    IdentitiesOnly yes
    ProxyCommand nc -X 5 -x 127.0.0.1:1080 %h %p
    StrictHostKeyChecking accept-new

  Host cloud-proxy         # 跳板机 / 隧道本体
    HostName <跳板机 IP>
    User ubuntu
    IdentityFile ~/.ssh/cloud_proxy
    IdentitiesOnly yes
    ServerAliveInterval 30
    ServerAliveCountMax 3
    ExitOnForwardFailure yes
    StrictHostKeyChecking accept-new
  ```

- 开源仓 remote 已从 HTTPS 改为 SSH：`git@github.com:Donald24718/OkHuman.git`
  （HTTPS + 密码 push 早已被 GitHub 拒绝，别再改回去）
