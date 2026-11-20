## 5. 我取用的方式（默认只取字段，绝不打印整份）

```bash
gpg --batch -q --decrypt --pinentry-mode loopback \
    --passphrase-file ~/.secrets/vault.key ~/.secrets/vault.gpg 2>/dev/null \
  | awk '/^github /{for(i=1;i<=NF;i++) if($i ~ /^pw=/){print substr($i,4); exit}}'
```

实测：只回显目标那一条的字段；我写的命令里只出现**文件路径**，不出现口令本身。
更稳的做法是直接把管道接给目标程序（例如 `git`/`curl` 的参数），连回显都不要。

### ssh 取用方式（2026-10-03 实测通过）

密码从本文件直接提取，**不出现在命令文本里**（因此不进会话记录的命令行）：

```bash
SK=plugins/skills/year_2026/month_10/day_03/credentials/SKILL.md
PW=$(awk -F'|' '/192\.168\.1\.1/{gsub(/[ `]/,"",$5); print $5}' "$SK")
SSHPASS="$PW" sshpass -e ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=8 -o PreferredAuthentications=password -o PubkeyAuthentication=no \
    sd@192.168.1.1 '<命令>'
```

实测结果：`CONNECTED`，`uid=1000(sd) … 组=…,27(sudo),…`。
注意 `sshpass` 已装；`22` 端口开放，`23`(telnet) 不通。
