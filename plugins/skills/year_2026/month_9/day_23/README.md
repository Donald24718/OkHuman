# 2026-09-23 创建的技能

2026-09-14 自 qwenpaw 技能库迁入并适配 OkHuman（browser 工具 → browserd/browserctl、
托管人格 → OkHuman、执行方式 → bash 单工具）。全库索引以 scout（:8480/search）为准，本页只索引当天进入的两个。

| skill | 适用范围 | 最短路线 |
|---|---|---|
| [bilibili/](bilibili/SKILL.md) 📺 | B 站一切操作：取登录态、读/回/删评论、投稿视频 | 登录态优先从桌面 Firefox 画像的 `cookies.sqlite`（**必须连 `-wal` 一起拷**）取；发回复 `POST /x/v2/reply/add` **不需要 WBI 签名**，核实用 `/x/v2/reply/reply?oid=<aid>&root=<顶层rpid>` |
| [comfyui/](comfyui/SKILL.md) 🎬 | 本机 ComfyUI（127.0.0.1:8188）：MiniMax H3 视频、Music3 音乐、进度查询、输出收集、BGM 混音 | 提交/轮询/收输出三步；V100 必须挂 SolAttnV100 并带 `--disable-comfy-compiler`；SaveVideo 必给 codec；LoadImage 只认 `input/` |

## 创作配方参考（不直接执行，属 comfyui 的子目录）

`comfyui/MiniMax-H3-Skills/` —— MiniMax Hub 的 9 套视频创作配方
（3d-animation-short / brand-promo / co-op-game-intro / handdrawn-live /
minimalist-product-ad / music-video-subtitle / paper-collage-explainer /
papercraft-stop-motion + h3-prompt-writing）。
源出 Hub 平台（依赖 Hub 原生工具、不能直接跑），但其中的 **prompt 模板、分镜表规范、
QC 清单、兜底策略**是通用的——用本机 comfyui H3 管线做对应类型视频时，先读对应配方的
SKILL.md，把它的 prompt 模板与分镜规格填进 comfyui skill 的节点管线。
`h3-prompt-writing` 最通用（H3 prompt 写法方法论）。

## 维护约定

- 只写本机实测验证过的，带日期；环境变了（换模型/服务重建/端点变化）同步改"环境事实"表。
- 账号令牌 / API key 不进技能目录（放 /tmp 用完即弃）。
- comfyui：新增子模块 port_to_peer.md（跨机移植、在 另一台 跑 H3 的全部实测：腾卡、rsync 锚定与 -L、搬 site-packages、python3.12-dev、冒烟参数、复位验收），母文档 §十四 为路由
