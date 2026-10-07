# plugins/ — 插件与技能仓管理规范（RULE）

本目录 = OkHuman 的插件与技能容器（与主程序完全解耦）。布局：
插件目录**扁平**平铺在本目录下；技能库在 `skills/` 子目录，**日期层级**。
通用约定（解耦契约 / 索引 / 构建 / 多实例）见文末附录。

> 模块总览（定位 / 架构 / 插件清单 / 运行接线）见 [`README.md`](README.md)；
> **本文件是唯一权威规范**，与其冲突时以本文件为准。

---

## 规范一：插件目录 —— 扁平排列

- 插件目录直接平铺在本目录下，不分层（现状：attach / browser / computer / cron / okmon / qq-channel / scout / voice-chat）
- 新插件加入：直接在本目录建子目录，附：
  - `README.md`（启动方式 + 配置 + 排障）
  - `meta.json`（agent 自写的一句话摘要，scout 索引信号，summary 必填）
- 插件目录内部要求见附录「解耦契约」（独立进程、自带 config.json、data/ 自管）

## 规范二：skills/ —— 技能库：日期层级（只放 skill，不放插件）

- 布局：`skills/year_<年>/month_<月>/day_<day>/<skill目录>/`
- 新建 skill：按**创建当天**的日期进入对应 day 目录——
  - day 目录已存在 → 直接放进去，不新建
  - 不存在 → 逐级新建 `year_年/` → `month_月/` → `day_日/`，再放进去
  - 同一天创建的多个 skill 共用同一个 day 目录
- skill 目录 = `SKILL.md`（本机**实测验证过**的操作经验，照它执行）+ 可选 `scripts/`、`references/`
- day 目录内的 `README.md` = 当天技能的人工索引（进什么写一行什么）
- 只写本机实测验证过的，带日期；环境变了（换模型/服务重建/端点变化）同步改 SKILL.md 里的「环境事实」
- 账号令牌 / API key 不进技能目录（放 /tmp 用完即弃）
- skill 目录同样带 `meta.json`（agent 自写一句话摘要），**进 scout 索引**，与插件同规则；
  任务落在某 skill 适用范围时，先 `cat` 对应 SKILL.md 照做

---

## 附录：通用约定

### scout 索引判据（对插件与技能均生效）

- scout（:8480）递归扫描本目录（`plugin_root`），**任意深度**含非空 `summary` 的
  `meta.json` 的目录 → 自动建索引（插件与技能同规则）；分类容器目录、日期目录无 meta.json → 不被索引
- 改完插件功能 → 同步更新其 README / meta.json →
  `curl -X POST http://127.0.0.1:8480/reindex` 刷索引
  （规则 2「先搜索引」的缘由：实测中读图需求没搜索引走了通用 OCR 弯路，
  而素材注入插件两次都是搜索第一命中——跳过这一步 = 违规）
- 发现 README / meta 过时 → 就地修正

### 解耦契约（所有新插件必须遵守）

1. **不 import OkHuman 任何模块**。
2. **不共享进程/内存**。插件是独立进程，只通过 OkHuman 的公开 HTTP 接口交互：
   - `POST /chat` `{ message }` → 同步跑完整轮，返回 `{reply, events}`
     （多实例插件经 `instances[]` 的 url 路由到不同实例）
   - `POST /chat/stream`（SSE）、`POST /enqueue`（异步）按需选用
3. **配置自管**：插件自己的 `config.json` 放插件目录内，不进 OkHuman 的 `config/`；
   敏感值只落插件自己的文件，不入 OkHuman 日志。
4. **数据自管**：运行时数据放 `<插件>/data/`，不写 OkHuman 的 data 目录。
5. **独立可跑**：每个插件是独立 Go 模块 + 独立 git 仓（插件目录内含 go.mod），
   `cd <插件目录> && go build ./...` 即可单独编译；不依赖 OkHuman 源码树任何文件。
   无外网环境用 `GOPROXY=off GOSUMDB=off go build ./...`。
6. **仓库布局**：OkHuman 本体一个仓；本目录每个插件各一个仓
   （本目录本身与 skills/ 都不是仓，只是容器）。
   插件二进制、data/、含部署值/密钥的 config.json 均不入 git（配置样例见 config.json.example）。

### OkHuman 主程序对插件无感知

- 插件物理上不在主程序构建路径内；主程序构建、go vet、git 都不碰插件。
- OkHuman 不加载、不扫描、不启动插件。插件生命周期（启动/停止/重启）
  由外部管理，和 OkHuman 进程无关。
- 插件挂了不影响 OkHuman；OkHuman 重启不影响插件。

### 多实例适配

OkHuman 的"多 agent"= 多个独立端口的进程（一个进程 = 一个 agent）。
插件要支持这种部署：

- **`instances[]`**：插件配置里列出每个 OkHuman 实例的 `{ id, url }`
- **qq-channel**：每个 bot 用 `bots[].instance` 路由到某实例；消息只进该实例
- **cron**：每个实例独立维护自己的 `cron.json`（落 `<插件>/data/<inst-id>/`）
- **browser**：不适用 instances[]——agent 为自己开浏览器实例（约定 87xx 端口段）

### Go 构建

- 单文件插件（attach/computer/cron/qq-channel）源码即 `<插件>/<同名>.go`；
  browser 有两个可执行文件：`browser/cmd/{browserd,browserctl}/main.go`
- 各插件单独编译：`cd <插件目录> && go build -o <二进制名> .`（见各插件 README）
- **改完源码 = 没干活，重编 + 真实启动验证 = 才干完**
