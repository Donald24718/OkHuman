## 另一台机器（另一台，192.168.1.3）差异（2026-11-04 实测）

本文其余部分写的是**生产机（192.168.1.2）**。在 另一台 上对应值：

两机的仓库根/模型根/实例目录/单元名对照见 `host-crash-autostart/services.md`「另一台机器」一节。
- 实例：小E 8451 运行目录 `~/hdd/OkHuman`、小C 8452 `~/hdd/OkHuman-8452`（**2026-11-12 实测**；旧文档写的 `/home/ey/桌面/…` 与小D/小F两个名字已废，别照着找）
- 数据/会话目录：`$OKHUMAN_DATA_DIR`、`$OKHUMAN_DATA_DIR`——**和本机同名**（2026-11-12 实测；`okhuman-xiaod-*`/`okhuman-xiaof-*` 是旧实例遗留，还在但不在用）
- 服务单元名：`okhuman-8451` `okhuman-8452` `lan-bridge-8451`(8602，唯一入口；`lan-bridge-8452`/8603 已于 2026-11-12 删) `strata-8080`(小E后端) `ninfer-8081`(小C后端) `okhuman-scout` `okmon` `okhuman-cron`
- ComfyUI：`/home/ey/桌面/ComfyUI-0.37.0`，端口 **8189**（细节见 `comfyui` 技能的 `port_to_peer.md`）

扫 另一台 的会话泄露时按 `$OKHUMAN_DATA_DIR`、`$OKHUMAN_DATA_DIR` 扫（2026-11-12 实测，与本机同名）；动手前先 `ss -ltnp` 看端口在谁手里——那天我就见过两个 11-08 手起的老映像进程占着 8451/8452，unit 管的实例退到 8453/8454。


实例运行目录、数据/会话目录、服务单元名的两机对照**不在这里**，唯一真源见条目 `host-crash-autostart` 的 `services.md`「另一台机器」一节。
