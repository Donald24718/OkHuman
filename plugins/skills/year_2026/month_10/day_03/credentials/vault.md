## 3. vault 布局

```
~/.secrets/                 chmod 700
  vault.gpg                 唯一密码载体（gpg AES256 对称加密）
  vault.key                 解锁口令文件，chmod 600，主人自己写；我不读其内容
  accounts.md               不含密码的清单（可选）
```

条目格式（一行一条，便于机器精确取字段）：

```
github    user=Donald24718  pw=<密码>  scope=fine-grained  note=Strata fork 推送
bilibili  user=<用户名>      pw=<密码>  note=扫码登录态优先，见 bilibili skill
```

## 4. 建立 vault（主人在自己终端做，口令不经过我）

以下命令形态我已用测试数据实测过：

```bash
mkdir -m 700 -p ~/.secrets
nano ~/.secrets/accounts.txt                       # 按上面格式写条目
umask 077; printf '%s' '你的口令' > ~/.secrets/vault.key
gpg --batch --yes --symmetric --cipher-algo AES256 \
    --pinentry-mode loopback --passphrase-file ~/.secrets/vault.key \
    -o ~/.secrets/vault.gpg ~/.secrets/accounts.txt
shred -u ~/.secrets/accounts.txt                   # 明文销毁
```

## 7. 加 / 改 / 删条目

```bash
gpg --batch -q --decrypt --pinentry-mode loopback --passphrase-file ~/.secrets/vault.key \
    ~/.secrets/vault.gpg > /tmp/vault.edit && chmod 600 /tmp/vault.edit
nano /tmp/vault.edit
gpg --batch --yes --symmetric --cipher-algo AES256 --pinentry-mode loopback \
    --passphrase-file ~/.secrets/vault.key -o ~/.secrets/vault.gpg /tmp/vault.edit
shred -u /tmp/vault.edit
```
