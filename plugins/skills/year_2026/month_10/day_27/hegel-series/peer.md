## 另一台机器（另一台，192.168.1.3）差异（2026-11-04 实测）

本文其余部分写的是**生产机（192.168.1.2）**。在 另一台 上对应值：

两机的仓库根/模型根/实例目录/单元名对照见 `host-crash-autostart/services.md`「另一台机器」一节。
- 实例：小D 8451 运行目录 `/home/ey/桌面/OkHuman`（`okhuman-local`）、小F 8452 `/home/ey/桌面/OkHuman-8452`
- 数据/会话目录：`$OKHUMAN_DATA_DIR/okhuman-xiaod-8451`、`$OKHUMAN_DATA_DIR/okhuman-xiaof-8452`（生产是 `$OKHUMAN_DATA_DIR/okhuman-84xx`，**名字里带实例名**）
- 服务单元名：`okhuman-8451` `okhuman-8452` `lan-bridge-8451/8452` `strata-8080`(小D后端) `ninfer-8081`(小F后端) `okhuman-scout` `okmon` `okhuman-cron`
- ComfyUI：`/home/ey/桌面/ComfyUI-0.37.0`，端口 **8189**（细节见 `comfyui` 技能的 `port_to_peer.md`）

在 另一台 上解释器是 `/home/ey/桌面/ComfyUI-0.37.0/venv/bin/python`（site-packages 与生产同版本，可直接用）。
