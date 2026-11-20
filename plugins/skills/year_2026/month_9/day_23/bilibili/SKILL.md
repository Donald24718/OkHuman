# B站操作经验（bilibili）— 母文档只做路由

本条目 = 这台机器 + 这个号上已验证可用的做法沉淀，不是泛泛教程。按任务读子模块，别整篇读；新经验归入对应子模块，不新开平行 skill。
现成脚本索引：`scripts/bili_comment.py`（读/回/删评论）、`scripts/bili_upload.py`（投稿）。

## 要做什么 → 读哪份

| 要做什么 | 文件 |
|---|---|
| 取登录态 / cookie、验证是否已登录（评论与上传都要） | `env-login.md` |
| 读评论、找新评论、回评论、删评论、判断「我回过没有」、回复口径、评论区安全边界 | `reply.md` |
| 投稿视频（上传、封面、标签、审核） | `upload.md` |
| 读一个视频讲了什么、取字幕/服务端转写 | `read_video.md`（画面抽帧见条目 `video-frames`） |
| 桌面浏览器兜底（API 走不通时） | `references/gui-route.md`；要点在 `reply.md` 末节 |
| 另一台机器上的差异 | `peer.md` |

## 遇到什么现象 → 读哪份

| 现象 | 文件 |
|---|---|
| 接口说未登录、cookie 失效 | `env-login.md` |
| 回复发不出去、被吞、提示要签名 | `reply.md`、`dead-ends.md` |
| 列视频那个接口报错（只有这一类需要签名） | `dead-ends.md`、`references/wbi.md` |
| 上传卡住、审核没过、封面不对 | `upload.md` |
| 某条 API 路线根本走不通（别再试） | `dead-ends.md` |
