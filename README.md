# OkHuman — 主程序（一个进程 = 一个 agent）

Go 单仓：`main.go` + `internal/`（config / agent / server / context / prompt…）+ `webui/` + `prompts/`。
本树 = 生产实例（小K）运行树。

## 部署拓扑

- **生产**：小K，本机（512nv），服务 :8451，LLM :8080；插件/技能在
  `/media/xhy/512nv固态/OkHuman_p/`（管理规范见其 `RULE.md`）
- **副本**：小黑，lil 机器，服务 :8452，LLM :8081；插件/技能跟随生产
- **生产 = 权威**：插件、配置、提示词以生产盘为准，副本跟随

## 提示词分层（2026-09-23）

- **通用层**：`prompts/*.md`（按文件名排序拼接为系统提示词，
  `POST /prompts/reload` 热加载）——身份、工具行为规则、目录触发器
- **专用层**（不进系统提示词）：部署拓扑与二进制交换协议 → 本 README；
  插件/技能布局与管理规范 → `OkHuman_p/RULE.md`；
  各插件/技能自身细节 → 各自目录的 README / SKILL.md

## 二进制交换协议（主程序二进制）

- 主程序二进制**只换对方实例的运行树**。对方实例 = 同机另一个 okhuman 进程
  （排除自己；`pgrep -x okhuman` + `readlink /proc/<pid>/cwd` 定位各自运行树，
  `ss -ltnp` 看端口——本实例端口即本目录 config.json 的 server.port，对方的端口 = 另一个 LISTEN）。
- 换前先用**一次性实例**验证新二进制（临时 data dir + 临时端口 + 真实 LLM，走一遍端到端），
  通过后才换对方树的文件（unlink + 重建替换运行中的可执行文件，不影响其运行进程）。
- **自己的运行树不直接换**，留给对方实例——两边同时换、都换坏就没人能救。

## 提交规范

agent 身份的提交必须带 provenance trailer：`Provenance: agent(小K @8451)`——
仓库 git 身份是机主（xhy）的，不带 trailer 则记录无法区分人/agent 手。
（署名先于记忆：账本要先承载"谁写的"，才谈得上承载过去。）
