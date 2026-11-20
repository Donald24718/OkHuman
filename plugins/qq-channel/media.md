# qq-channel — 媒体进出（outbox 2026-09-24 立 / attach 2026-11-11 / 入站按实例分 2026-11-12）

## 出站：发文件一律显式投递，用子命令、不自己拼路径

```bash
./okhuman-qq-bridge attach <文件>   # 按 OKHUMAN_SELF_URL 认出我 = 哪个实例 → 取 config 里属于我的那个 bot
# 可选 --bot <app_id> / --to <实例id>；--dry-run 只打印将投给谁，不落文件
```

- 映射写在 config（`instances[].url` + `bots[].instance`），不用手抄 app_id；认不出自己 / 该实例配多个 bot / `--bot` 与实例不符 → **报错不猜**（猜错就发进别人的会话，收不回）。
- 手写法 `cp … "outbox/<botAppID>/"` 仍可用，但可能被 2s 轮询拾到半截文件（attach 先写 `.staging` 再 rename）。
- **发出去就删掉原文件**（2026-11-12 改：原先归档 `sent/`，到那天已积 475 个 / 730MB 且无 retention；要留档在 attach 前自己存一份）。失败留原位重试；图片扩展名按图片、其余按文件。

## 入站

主人发来的附件落 `/tmp/qq-inbox/<实例 id>/<ts36>-<安全名>`（`/inject` 通知带绝对路径）；按实例一口是 2026-11-12 起，此前两实例共用一口；目录动态建，默认留 1 天、在下次落盘时清（tmpfs 占内存）。**入站在常驻进程里跑，改这条要重启桥。**

## 失败与限制（历史坑，已修）

- QQ `/v2/users/{openid}/files` 返回的 `file_info` 是 **base64 字符串**不是对象，按 map 解析恒 nil → 报"缺 file_info"（09-24 修）。
- 文件消息 `msg_seq` 原用 WS 会话 seq（`c.seq`），与文本消息的 `c2cMsgSeq` 不同源，回退被 QQ 拒 → 统一 `nextC2CSeq()`。
- 正文里 `[File: <路径>]` 自动发文件曾存在，因纯副作用（思考段引用路径就误发）整块删除（2026-09-24）。别再指望"提到路径就自动发出去"。
