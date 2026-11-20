## 另一台机器（另一台，192.168.1.3）差异（2026-11-04 实测）

本文其余部分写的是**生产机（192.168.1.2）**。在 另一台 上对应值：

两机的仓库根/模型根/实例目录/单元名对照见 `host-crash-autostart/services.md`「另一台机器」一节。
- 实例：小D 8451 运行目录 `/home/ey/桌面/OkHuman`（`okhuman-local`）、小F 8452 `/home/ey/桌面/OkHuman-8452`
- 数据/会话目录：`$OKHUMAN_DATA_DIR/okhuman-xiaod-8451`、`$OKHUMAN_DATA_DIR/okhuman-xiaof-8452`（生产是 `$OKHUMAN_DATA_DIR/okhuman-84xx`，**名字里带实例名**）
- 服务单元名：`okhuman-8451` `okhuman-8452` `lan-bridge-8451/8452` `strata-8080`(小D后端) `ninfer-8081`(小F后端) `okhuman-scout` `okmon` `okhuman-cron`
- ComfyUI：`/home/ey/桌面/ComfyUI-0.37.0`，端口 **8189**（细节见 `comfyui` 技能的 `port_to_peer.md`）

**另一台 不同**：只有 2×V100-**16G**，且平时被 `strata-8080`(≈15.4G) 与 `ninfer-8081`(≈15.9G) 占满 → 选卡前必须先 `nvidia-smi` 看空闲，跑 H3 要先 `systemctl stop` 掉一个后端（见 `comfyui/port_to_peer.md`）。本机的 `CUDA_VISIBLE_DEVICES=2` 在那台不存在。
