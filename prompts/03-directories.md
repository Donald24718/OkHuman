# 目录指引（插件）

外部维护目录。**本文件只说位置和什么时候去查/去写，不列具体有什么**——有什么以 scout 索引为准。
需要插件能力（规则 8 的触发场景）先 scout 语义搜索：`curl "http://127.0.0.1:8480/search?q=能力关键词"`（插件目录建了索引，返回名称+相关度），命中后细读该目录的 README / 源码再动手；scout 挂了才退回 `ls` 看目录。

## 插件：`/OkHuman_p/`（Go 版，生产权威）

顶层分 `Frequently Used Plugins/` 与 `Rarely Used Plugins/` 两个分类目录（2026-09-21 重排）；每个插件是其中的一个独立子目录（独立进程或独立脚本，经 HTTP/文件与 OkHuman 主进程松耦合，各有自己的启停生命周期，挂掉不影响主进程），带 `meta.json`（agent 自写的一句话摘要，scout 索引时优先用它）。另有 `OkHuman_skill/`（技能库：本机实测操作经验沉淀，SKILL.md 格式，不是插件、不建索引，任务落在其适用范围内先读对应 SKILL.md 照做）与 `scout/`（本目录的搜索索引服务）。

- **什么时候去查**：任务涉及主进程之外的周边能力（对外渠道、浏览器、定时、注入素材等）——先 scout 搜索（规则 8），命中读它的 README 再启动/调用；别重复造轮子，更别拿通用工具顶替现成插件。
- **什么时候往里放**：新开发的独立能力（独立进程、独立 config、不 import OkHuman 模块）放进对应分类目录（Frequently Used / Rarely Used）建新子目录，附 README（启动方式 + 配置 + 排障）+ meta.json 一句话摘要。
- **维护**：改完插件功能 → 同步更新它的 README/meta.json，并 `curl -X POST http://127.0.0.1:8480/reindex` 刷索引；发现 README/meta 过时 → 就地修正。本盘是生产（小K），插件一律用这里的 Go 版；`OkHuman_skill/` 技能库 2026-09-21 自 qwenpaw 技能库迁入（用法见其 README：只收本机实测验证过的操作经验）。
