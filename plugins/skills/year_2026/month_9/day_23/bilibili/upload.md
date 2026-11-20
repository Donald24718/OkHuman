# B站 · 投稿视频（bilibili skill 的子模块）
> 编号沿用母文档的体系：本文件含 §2/§2.5/§2.6/§3/§6/§7/§9/§10/§11/§12；
> **§1 登录态、§4 已验证死路、§5 WBI 在 `../SKILL.md`**，别在子模块里重抄一遍。


登录态与通用 API 事实在 `../SKILL.md`。现成脚本 `scripts/bili_upload.py`。


## 8. 视频上传（bilibili-api-python，2026-08-21 实测成功 BV18D8T6jE5r）

浏览器上传页是重型 SPA，headless 渲染进程会被回收（page context closed），**别走浏览器**，直接服务器端：

```bash
# 库装在独立 venv（清华镜像快）
uv venv /tmp/biliup && uv pip install --python /tmp/biliup/bin/python \
  --index-url https://pypi.tuna.tsinghua.edu.cn/simple bilibili-api-python aiohttp
```

新版 API 注意（和网上旧教程不同）：
- 从 `bilibili_api.video_uploader` 导入，**不是**顶层 `bilibili_api`：
  `from bilibili_api.video_uploader import VideoUploader, VideoMeta, VideoUploaderPage`
- `Credential(sessdata, bili_jct, buvid3, buvid4, dedeuserid)` —— 从 cookie 取
- 一个 P 用 `VideoUploaderPage(视频路径, "P1")`
- `VideoMeta(tid=65, title, desc, cover=封面jpg路径, tags="英文逗号分隔", original=True)`
  - **标签上限 12 个、单个 ≤20 字**（2026-10-05 实测：给 20 个直接报 `21005 Tag不能为空，总数量不能超过12个`，整个上传回滚，改完重传）
  - cover 传本地 jpg 路径（可从视频抽帧：`ffmpeg -ss 1.5 -i in.mp4 -frames:v 1 -update 1 -q:v 2 cover.jpg`）
  - 分区沿用科技 `tid=65`（本账号视频都是这个分区）
- `VideoUploader(pages=[...], meta=..., credential=cred, cover=COVER)` → `asyncio.run(up.start())` 返回 `{aid, bvid}`
- **慢上行必改超时**（2026-10-26 实测，BV1Qjnd6hEX4）：本机上行仅 ~0.6MB/s 时，默认 `request_settings` 超时 30s 会让每个分片超时→`chunks_pending` 无限重传，进程看着"卡住"（CPU 几乎不涨、账号里 0 条在审），9 分钟能重传出 4 倍文件量。开头加：
  ```python
  from bilibili_api import request_settings
  request_settings.set_timeout(900)
  ```
  改完 79MB 两分钟过。别先怀疑文件大小，先测 `cat /sys/class/net/<wan>/statistics/tx_bytes` 的真实上行。
- **别同时起两个上传进程**（同一脚本重复启动会各自传一份，账号侧只会有一条，但白烧带宽）；`pgrep -cf bili_up.py` 会把监视脚本自己算进去，用 `pgrep -cf "work/bili_up.py"`。
- **上传用码率**：B 站自己转码也就 3-6Mbps，上传件压到 ~3.5Mbps（`-crf 23 -maxrate 3.5M`）比 14Mbps 原件快 4 倍且看不出差别；慢网络下先压再传。

- **必须**额外 `pip install aiohttp`，否则报"尚未安装第三方请求库"
- 成功判定：`start()` 返回带 `bvid` 的 dict 即成功；再用 `GET /x/web-interface/view?bvid=...` 查 `state`（0=正常/过审，1=待审）
- **审核中 `get_info()` 的 `tags` 返回空 ≠ 没打上标签** → 用 `video.Video(aid=...).get_tags()` 复查；封面回查 `get_info()["pic"]` 下载核尺寸（2026-10-28）

