# OkHuman — 项目总览

> 核心思想 / 架构亮点 / 插件机制 / 部署拓扑。
> 部署细节与二进制交换协议见 `README.md`；插件与技能仓管理规范见 `OkHuman_p/RULE.md`。

## 一、核心思想

- **一个进程 = 一个 agent**：Go 单仓（main.go + internal/ + webui/ + prompts/），一个进程就是一个完整 agent——系统提示词、消息队列、上下文管理、工具循环、WebUI 全在一个进程里。多 agent = 多个独立端口的进程，没有框架、没有共享状态。
- **一个元工具（bash）**：agent 只有一个工具 bash，一切外部操作（执行命令、文件读写改、搜索、抓被截断的 log 全文）都靠它。能力来自插件、经验来自技能库，全部经 bash + HTTP 调用，不搞多工具路由。
- **生产 = 权威**：插件、配置、提示词以生产盘（小K）为唯一权威，副本（小黑）单向 rsync 瘦镜像跟随；救生艇（小欧）冻结旧树、永不更新，只用于双死救场。
- **生命自感知**：系统提示词首 prepend 一块"[你的生命]"——本实例端口/PID/源码位置 + 所接入 LLM 服务的端口/PID，每次 LLM 调用现算。明确告诉模型：这些 PID 和端口就是你和你思考的载体，kill/重启/占用它们 = 伤自己的命。
- **署名先于记忆**：agent 的提交必带 `Provenance: agent(小K @8451)` trailer——账本要先承载"谁写的"，才谈得上承载过去。

## 二、架构亮点

- **上下文滚动压缩**：总 token 超预算时，把旧消息批次交给 LLM 压成总结滚动进 summary；迭代压缩最多 3 轮（单轮 LLM 倾向写长），3 轮后取最短一轮保底；压缩流式可见（WebUI 实时看思考块）。
- **前台超时自动转后台**：工具调用与 30s 前台超时赛跑，超时不取消、转后台继续执行，立即回填"已转后台"通知；结果出来自动注入消息队列（与用户追加消息同一队列同一逻辑）——模型不必傻等，结果也不丢。
- **死循环检测（doom）**：连续 3 次完全相同（工具名+参数逐字段比较）的调用 → 注入警告；警告后仍重复 → 强停本轮。
- **提示词分层 + 热加载**：通用层（prompts/*.md）拼接为系统提示词，POST /prompts/reload 热加载；专用层（部署拓扑 / 插件规范 / 各插件 README）不进系统提示词，agent 按需 cat。
- **彻底解耦契约**：主程序对插件无感知——不加载、不扫描、不启动；插件不 import 主程序任何模块、不共享进程/内存，只走公开 HTTP 接口（/chat、/enqueue、/events SSE、/history……）；配置自管（插件目录内 config.json）、数据自管（插件/data/）、独立可跑（独立 Go 模块 + 独立 git 仓）。插件挂了不影响 OkHuman，OkHuman 重启不影响插件。
- **scout 语义索引**：Go 常驻服务（:8480）递归扫描插件/技能仓，条目摘要读 agent 自写的 meta.json，打分 = 关键词 + Qwen3-Embedding-0.6B 语义向量。硬规则：需要主程序之外的能力，先 curl 搜索引，凭记忆跳过 = 违规（缘由：读图需求没搜索引走了 tesseract OCR 弯路）。
- **二进制交换协议 + 保护期**：主程序二进制只换对方实例的运行树；换前先用一次性实例端到端验证；新二进制严禁同时落两棵树——旧二进制实例就是救生艇。换前问一句："此刻如果新二进制有 bug，谁还活着能救？"答不上就不换。

## 三、插件是怎么做的

三个文件夹、三种布局（`OkHuman_p/RULE.md` 为唯一权威）：

1. **Frequently Used Plugins/** —— 扁平排列：attach / browser / computer / cron / okmon / qq-channel / voice-chat
2. **Rarely Used Plugins/** —— 日期层级 year_/month_/day_：新插件（含降级移入）按当天日期进目录链，同一天共用同一 day 目录
3. **OkHuman_skill/** —— 技能库，同样日期层级：skill = SKILL.md（本机**实测验证过**的操作经验，照它执行）+ 可选 scripts/references；账号令牌不进技能目录

每个插件是一个**自包含小系统**：

- 独立进程 + 自带 config.json + 自带 data/ + 独立 git 仓
- 目录必带 `README.md`（启动 + 配置 + 排障）和 `meta.json`（agent 自写一句话摘要，scout 索引信号，summary 必填）
- 多实例适配：配置里 `instances[]` 列出每个 OkHuman 实例的 {id, url}，消息按配置路由到对应实例
- 改完源码 = 没干活，**重编 + 真实启动验证 = 才干完**（忘 go build 曾是一起老 bug 复发的根因）
- 改完 README/meta.json → POST /reindex 刷 scout 索引

现有插件一览（各自一句话能力）：

| 插件 | 干什么 |
|---|---|
| attach | 素材注入上下文：图片原图/压缩注入，视频按策略直通 input_video（自动压 360p / 分段） |
| browser | browserctl 轻 CLI 驱动本地 Firefox：导航、定位、填表、点击、取文本，主程序零改动 |
| computer | 给 agent 装"手"：整屏截图注入 + X11 鼠标键盘事件，可看屏、点鼠标、敲键盘 |
| cron | 定时任务（:8601），到点把 prompt 作为 user 消息 POST 到对应实例 /chat，每实例独立落盘 |
| okmon | 进程监控 + 启停管理（显卡使用 / Go 进程状态，webui :8496） |
| qq-channel | QQ 官方渠道桥：独立常驻进程，QQ 消息 ↔ agent 双向转发（无监听端口，只有出站） |
| voice-chat | 语音全闭环：麦克风 → VAD → 声纹门控（只认你的声音）→ ASR → 唤醒词路由（小K/小黑）→ Kokoro 本地 TTS 朗读；回复按条排队 + 消息内句子级流式播报；全自包含（二进制/模型/Python 环境都在目录里） |

技能库（OkHuman_skill）：comfyui（MiniMax H3 视频/音乐提交、进度、输出收集）、bilibili（回/扫/删评论、投稿、WBI 签名）。

## 四、部署拓扑

- **生产 小K**：本机 512nv 固态盘，服务 :8451，LLM :8080；插件/技能仓在 `/media/xhy/512nv固态/OkHuman_p/`
- **副本 小黑**：同机 lil_app 盘，服务 :8452，LLM :8081；插件仓跟随生产瘦镜像（okp-sync 单向 rsync --delete，重资产不镜像，~150M）
- **救生艇 小欧**：同机 docker 盘，服务 :8453；冻结旧树（永不更新），平时不起——唯一用途是生产与副本都死时救场
