# 已禁用的 browser 插件发笔记路线（2026-10-04 曾跑通，现不得再用）

留档原因：这套技术细节当时是真跑通的（14 张图文 09:49 发布成功），将来若主人明确解除
"小红书只走 computer use"的规矩，可以少踩一遍坑。**在规矩解除前不得执行。**

- 起 browserd 用**持久画像**（`<本地盘>/browser-profiles/xhs`，`chmod 700`，
  放 plugins 之外，否则被 okp-sync 镜像带走）+ 扫码登录。
- 上传页 `creator.xiaohongshu.com/publish/publish?source=official`；切"上传图文"tab 用
  `document.querySelectorAll('.creator-tab')[2].click()`（label 有重复：0/1 都是"上传视频"）。
- 传图 `file_upload "input[type=file]" <单张>` **逐张循环**（input 虽 multiple，browserctl
  一次只吃一个路径）；发前核对页面 `14/18` 计数，别只数 img 标签。
- 标题 `input[placeholder="填写标题会有更多赞哦"]`（20 字）；正文 `.tiptap.ProseMirror`（1000 字）。
- 内容类型声明：`.d-select` → **笔记含AI合成内容**。
- **发布按钮在 shadow root 里，DOM 查不到** → 用视口坐标 `click_at x y --shot <视口截图.png>`
  （不给 `--shot` 直接报错；`--full` 整页图不行）。
- 成功判定：URL 出现 `&published=true` + 表单清空；权威核对去 `/new/note-manager`。
- 通用坑：`eval` 输出必须 `.slice()` 截断（曾一次灌进 360 万字符 CSS）；
  浏览器级 `Network.getAllCookies` 返回 0 ≠ 注入失败，用 `browserctl cookies <域名>` 验。

## 另一台机器（另一台，192.168.1.3）差异（2026-11-04 实测）

本文其余部分写的是**生产机（192.168.1.2）**。在 另一台 上对应值：

- 仓库根等路径对照见 `host-crash-autostart/services.md`「另一台机器」一节
- 实例：小D 8451 运行目录 `/home/ey/桌面/OkHuman`（`okhuman-local`）、小F 8452 `/home/ey/桌面/OkHuman-8452`
- 数据/会话目录：`$OKHUMAN_DATA_DIR/okhuman-xiaod-8451`、`$OKHUMAN_DATA_DIR/okhuman-xiaof-8452`（生产是 `$OKHUMAN_DATA_DIR/okhuman-84xx`，**名字里带实例名**）
- 服务单元名：`okhuman-8451` `okhuman-8452` `lan-bridge-8451/8452` `strata-8080`(小D后端) `ninfer-8081`(小F后端) `okhuman-scout` `okmon` `okhuman-cron`
- ComfyUI：`/home/ey/桌面/ComfyUI-0.37.0`，端口 **8189**（细节见 `comfyui` 技能的 `port_to_peer.md`）

（路径以生产机为准）另一台：持久画像在插件根下，浏览器名 `chromium`。
