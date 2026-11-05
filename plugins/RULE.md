# plugins/ — 插件与技能仓管理规范（RULE）

本目录 = OkHuman 的插件与技能容器，与主程序完全解耦。插件**扁平**平铺在本目录；技能库在 `skills/`，**日期层级**。

## 布局与必备文件

| 位置 | 布局 |
|---|---|
| `plugins/<插件>/` | 扁平，不分层 |
| `plugins/skills/year_<年>/month_<月>/day_<日>/<skill>/` | 日期层级，只放 skill |

日期层级：按**当天**日期进入——day 目录已存在就直接放，不存在才逐级新建；同一天创建的多个条目共用同一 day 目录。day 目录内的 `README.md` = 当天进什么写一行的人工索引。

- **插件**：`README.md`（启动 + 配置 + 排障）+ `meta.json`
- **skill**：`SKILL.md`（**本机实测验证过**的经验，带日期，照它执行）+ 可选 `scripts/`、`references/` + `meta.json`
- **`meta.json` = scout 的索引信号**：`summary` 必填；`keywords` 必填（触发词、别名、用户的口语说法；字符串或字符串数组都可）；`name` 可选（缺省用目录名）。**没写 keywords ≈ 这个条目在口语式查询下不存在。**
- 账号令牌 / API key 不进技能目录（放 /tmp 用完即弃）。

## skill 怎么写

- **建前先查重**（先搜索引 + `find skills -name SKILL.md`）：命中同域（同一网站/工具/能力面）就**并入既有 skill 加小节，不新建平行 skill**。
- 一个 skill 覆盖多件事：`SKILL.md` 只写**路由**（任务 → 同目录子模块文件名）+ 共用前置；细节写进子模块文件。划分判据：同一能力面的不同任务 → 子模块；不同能力面 → 另建 skill（仍先查重）。共用内容只留一处，引用写「§N 在 `../SKILL.md`」。
- **子模块 / 子技能不独立建索引**：scout 认定一个目录为文档后**不再向下走**。母文档必须点名每个子模块/子技能——路由表写清文件名与适用任务，`meta.json` 的 `summary` + `keywords` 带上触发词，否则细节永远搜不到。
- 环境变了（换模型、服务重建、端点变化）→ 同步改 SKILL.md 里的「环境事实」。

## scout 索引判据（插件与技能同一套）

- 递归扫 `plugin_root`：任意深度含非空 `summary` 的 `meta.json` 的目录 → 入索引。分类容器/日期目录不入库；运行时资产目录（`venv`、`data` 等）剪枝，**但该目录自己带 `meta.json` 就不剪**。「搜不到」的第一原因是没有 `meta.json`。
- **纯关键词检索，没有语义向量；索引只在内存、不落盘**（真源就是 `meta.json`，启动即扫毫秒级）。实测数据与规模阈值只写在 `scout/README.md`。
- **动手前先搜**：任务落在插件/skill 能力面内就先搜一遍，命中就读 README/SKILL.md（常有已验证的更短路线和现成脚本）。
- 改完 README / meta.json → `curl -X POST http://127.0.0.1:8480/reindex`。
- **搜不到先 `GET /doctor` 再下结论**：它报 `no_keywords / stale_in_index / missing_from_index / same_name / meta_problems`；`/health` 的 `docs=0` = 服务没起或数据盘没挂载。
- **同名不自动取舍**：都返回 + `same_name` 计数，由调用方决定删哪份。发现 README / meta 过时 → 就地修正。

## 写作纪律：任何文档都要精简，只记录可复用的东西

适用于一切文档（SKILL.md 与子模块、README、`meta.json`、LESSONS、注释）。唯一判据是**可复用**：能被下次直接照做的才写——结论、参数与上限、入口与端点、判据、失败的真因与绕法、能当阈值的实测数（标日期）。出现即删：叙事与过程、决策心路与感想、只对本次成立的细节、别处已写过的话（重复只留一处，其余写「见 X」）。新增优先改写或替换旧条目，一条一行；写完自查：**这段删掉，下次会不会走弯路？不会 → 删。**

## 解耦契约（所有新插件必须遵守）

1. **不 import OkHuman 任何模块**，不依赖源码树任何文件。
2. **不共享进程/内存**：插件是独立进程，只走公开 HTTP —— `POST /chat`（同步整轮，返回 `{reply, events}`）、`POST /enqueue`（异步）+ `GET /events`（SSE）或 `GET /events/poll`（短轮询）。旧 `POST /chat/stream` 已删除（慢客户端会把实例卡死），要流式就走 `/enqueue` + `/events`。
3. **配置自管**：`config.json` 放插件目录内，不进 OkHuman 的 `config/`；敏感值只落插件自己的文件。
4. **数据自管**：运行时数据放 `<插件>/data/`。
5. **独立可跑**：每个插件 = 独立 Go 模块（含 `go.mod`）+ 独立 git 仓，`cd <插件目录> && go build ./...` 可单独编译；无外网用 `GOPROXY=off GOSUMDB=off`。二进制、`data/`、含密钥的 `config.json` 不入 git（配置样例写 `config.json.example`）。

## 多实例与构建

- 一个进程 = 一个 agent：插件配置用 `instances[]` 列 `{id, url}` 并按 url 路由（`qq-channel` 用 `bots[].instance`；`cron` 每实例独立存储）。
- 各插件单独编译：`cd <插件目录> && go build -o <二进制名> .`。**改完源码 = 没干活；重编 + 真实启动验证 = 才干完。**
