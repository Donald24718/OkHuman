# lan-bridge — 排障

| 现象 | 原因 / 办法 |
|---|---|
| 手机连不上（超时） | ① PC 防火墙：`ufw` 若启用需 `sudo ufw allow 8602/tcp`；② **随身wifi/AP 开了"客户端隔离"**——很多随身wifi 会把 WiFi 客户端与 USB 主机互相隔离，需在 AP 设置里关掉；③ 手机不在同一网段 |
| 502 上游不可达 | 实例没起 / `instances[].url` 写错；`curl http://127.0.0.1:8451/health` 先验 |
| 403 | 端点不在白名单，或方法不符（如 `GET /strip-injects`） |
| 反复回到登录页 | cookie 被清 / token 改了 / 浏览器禁第三方 cookie（桥与页面同源，正常不该发生） |
| 流式不出现 | 中间加了会缓冲的反代；桥本身已逐块 flush，别再套一层 |

## 探活怎么测才算数

- 按 `config.json` 的 `listen.host` 决定打哪个地址。**2026-11-08 起本机 = `0.0.0.0:8602`**（`127.0.0.1` 和两个 LAN 地址都听）。之前绑死单个内网地址时打 `127.0.0.1:8602` 得 000，会误判成"没起"（2026-11-07 犯过）；绑单地址 = 只有一条路进得来。**判"能不能用"要从访问方那侧 curl，不要只在本机测。**
- 跨机的入口枢纽在 **`.1`（sd-box，用户 `sd`）的 root 单元 `okhuman-entries.service`** = 本桥的 TCP 转发枢纽（`/opt/okhuman-entries/entries.py`，root 所有 644 → 改它**连备份都要 sudo**）：`UP` 表每行 =（tag, 本机端口, 上游 host:port, 显示名, 对端 token, 强制实例），靠 `force_inst` 改写 `Cookie` 里的 `okh_inst`。当前四行：18451→8451、18452→8452、18501→`.3:8451`、18502→`.3:8452`（18454/小G已于 2026-11-08 删，备份 `entries.py.bak-20261108-drop-8454`）。加/删实例只改 `UP` 一行 + `python3 -m py_compile` 过语法 + `sudo systemctl restart okhuman-entries`。入口页在 `:8500`（**只有 `.1` 本机取得到，跨机被它的防火墙挡**）→ 判"卡还在不在"要 `ssh sd-box curl 127.0.0.1:8500`，从这边测不到不等于没改。
