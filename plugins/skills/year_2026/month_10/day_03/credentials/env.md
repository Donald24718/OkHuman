## 1. 环境事实（2026-10-03 实测）

- `gpg (GnuPG) 2.4.4` 可用；**`age` 未安装**；`shred` 可用（`/usr/bin/shred`）。
- gpg-agent socket：`/run/user/1000/gnupg/…/S.gpg-agent`（同一用户可连）。
- **我的 bash 工具没有 tty**：不带 `--pinentry-mode loopback` 的 gpg 会**永久挂住等口令**（实测 `timeout 15` 后 `signal Terminated`）。它会占住我的进程组，别这么调。
- **loopback 传进去的口令不会被 agent 缓存**（实测：随后不带口令解密会挂住）。所以"主人在自己终端先解锁、我随后免口令解密"这条路**不成立**。
- 可行且已实测的路线：`--pinentry-mode loopback --passphrase-file <600 权限文件>`。加密、解密、只取单个字段全部验证通过。
- vault 放 `~/.secrets/`（在 `plugins/` 之外，不被同步）。
- **2026-10-28 核对：`~/.secrets/` 是空目录，vault 从未建立**（§3–§5 是已验证的做法，不是现状）。
  当前密码只存在于 §10 的明文清单里。要启用 vault 照 §4 做，主人自己在终端写口令。

## 11. 副本位置（事实，不是警告）

密码明文现在存在于以下位置，2026-10-03 核实：

1. 本文件（`OkHuman_skill/year_2026/month_10/day_03/credentials/SKILL.md`）
2. 副本树：`okp-sync` 会把本文件 rsync 到 `<副本树>/plugins/`（EXCL 不含 `.md`）→ 小B :8452 那棵树里会有一份
3. scout 索引：`meta.json` 的 summary 不含密码（有意如此），但索引服务会读 SKILL.md 做语义嵌入
4. 会话存档：`$OKHUMAN_DATA_DIR/session-records/` 与 `session-summaries/` 里已有明文（密码经会话传入时落下，**我无法回收**）
