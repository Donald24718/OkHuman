# plugins/ — 插件模块自述

> 本文件是**模块总览**：定位、架构、插件清单、运行接线。
> **工程规范（目录约束 / 解耦契约 / 索引 / 构建 / 多实例）以 [`RULE.md`](RULE.md) 为唯一权威**；
> 本文件只做面向人的导航与实证描述，不重复其规则细节。冲突时以 `RULE.md` 为准。

---

## 一、定位：与主程序零耦合的外挂容器

OkHuman 的世界观是「**一个进程 = 一个完整 agent**」，多 agent = 多个独立端口的进程。
`plugins/` 是这个体系的**扩展层**——不是主程序的一部分，而是挂在它旁边的独立进程集合。

三条经代码验证的事实（`grep` 全目录 `okhuman/internal|okhuman/config` 结果为空）：

1. **主程序对插件完全无感知**：不 import、不扫描、不启动、不编译。
   主程序的 `go build` / `go vet` / git 都不碰本目录。
2. **插件之间互不依赖**：没有共享内存、没有互相 import；唯一横向协同者是索引服务 `scout`。
3. **每个插件自成一仓**：各自带 `go.mod`（如 `okhuman-p/attach`、`okmon`、`scout`）、
   各自 `config.json`、各自 `data/`、各自 `.gitignore`。
   `plugins/` 与 `skills/` 本身**不是 git 仓**，只是容器。

**失败隔离**：插件挂了不影响 OkHuman；OkHuman 重启也不影响插件。

---

## 二、运行时如何绑成一体：两条纽带

插件与主程序**只通过两条纽带连接**，没有任何进程内调用。

### 纽带 1：OkHuman 的公开 HTTP 接口

所有插件对主程序的操作都收敛到这一组端点（实证自各插件源码）：

| 端点 | 方法 | 使用者 | 语义 |
|---|---|---|---|
| `/chat` | POST | `cron`、`qq-channel`（兜底） | 同步跑完整一轮，返回 `{reply, events}` |
| `/chat/stream` | POST | 按需（SSE） | 流式跑一轮 |
| `/enqueue` | POST | `qq-channel` | 异步入队，入队即返，不等回合结束 |
| `/events` | GET | `qq-channel` | SSE 事件广播（按事件边界拆气泡） |
| `/history` | GET | `qq-channel` | 静默兜底，轮询补发回尾 |
| `/inject` | POST | `attach`、`qq-channel` | 把素材 part（图/视频/文本）注入当前上下文 |
| `/health` | GET | `voice-chat` 等 | 存活探测 |

多实例场景下，插件经配置的 `instances[]` 的 `url` 把请求路由到不同实例。

### 纽带 2：`scout` 索引服务（发现层）

解耦的代价是「**主程序不知道有哪些插件**」，所以发现必须靠索引。
`scout`（`:8480`）**递归扫描本目录**，凡是含非空 `summary` 的 `meta.json` 的目录
（任意深度）→ 自动建索引；分类容器目录、日期目录无 `meta.json` → 不被索引。

- 检索打分：**名称关键词 0.40 > 简介关键词 0.25 > 语义向量 0.20 > 全局关键词 0.15**
- 语义后端：常驻 `llama-server`（Qwen3-Embedding-0.6B，`:8591`）；
  **模型缺席/拉不起时优雅降级为纯关键词检索**，服务照常可用。
- 端点：`/search` / `/reindex` / `/doc` / `/health`

这解释了为何 Agent 必须「**用插件前先查 scout**」——跳过索引去凭记忆找插件 = 走弯路。
插件新增或改功能后，务必刷新索引：

```bash
curl -X POST http://127.0.0.1:8480/reindex
```

---

## 三、插件清单

| 插件 | 形态 | 端口 | 与 OkHuman 的接线 | 一句话职责 |
|---|---|---|---|---|
| [`attach`](attach/) | CLI（bash 内启动） | 无（**父链自识别**） | `POST /inject` | 图片/视频/单帧素材注入上下文 |
| [`browser`](browser/) | CLI + 守护进程 | 87xx 段（agent 自定） | 无（agent 经 bash 驱动 CDP） | 真实浏览器操作 |
| [`computer`](computer/) | CLI | 无 | 经 `okattach` → `/inject` | 整屏截图 + X11 键鼠 |
| [`cron`](cron/) | 常驻 | `:8601` | `POST /chat` | 多实例定时任务调度 |
| [`okmon`](okmon/) | 常驻 | `:8496` | 无（`ss`/`pgrep` 操作 OS 进程） | OkHuman 系进程 + 显卡监控/启停 |
| [`qq-channel`](qq-channel/) | 常驻（无监听端口） | 无 | `/enqueue` + `/events` + `/history` | QQ 频道双向桥 |
| [`scout`](scout/) | 常驻（索引服务） | `:8480`（embed `:8591`） | 无（读目录 `meta.json`） | 插件/技能语义检索与发现 |
| [`voice-chat`](voice-chat/) | 常驻（Python，自包含） | 见其 `config.json` | `/health` + 按唤醒词路由 | 语音对话闭环（VAD→ASR→TTS） |

> `skills/` 子目录与插件**共用同一套 `meta.json` 约定**，同样进 scout 索引；
> 区别仅在布局：插件目录扁平，技能库按 `skills/year_<年>/month_<月>/day_<日>/<skill>/` 日期分层。

---

## 四、四种形态（按「谁启动、怎么连核心」划分）

- **A. CLI 类** —— 在 agent 的 bash 会话内启动，用完即退。
  `attach` 精妙处：无需配置即可找到目标实例端口——沿 `/proc/<pid>/status` 的 `PPid`
  上溯进程父链，找到 `cmdline` 基名为 `okhuman` 的主进程，再读 `/proc/net/tcp` 的
  LISTEN socket 取其端口。父链是 `okattach←bash←okhuman`，**端口天然随实例隔离**
  （生产 8451 / 副本 8452）；脱离会话才回退 8451 并告警。
- **B. 常驻驱动类** —— 独立进程，到点主动调核心。
  `cron`：tick 轮询 → 到点把 prompt 作为 user 消息 `POST /chat`；
  **每实例独立 `CronStore`**（落 `data/<inst-id>/cron.json`，原子写 tmp+rename），
  同实例串行触发，失败也推进排程以免每 tick 重发。
- **C. 常驻桥接类** —— 出站连外部、入站连核心。
  `qq-channel`：QQ 消息 → `POST /enqueue`；常驻订阅 `GET /events` 按事件边界拆气泡；
  多 bot 经 `bots[].instance` 路由；带单实例锁与 `msg_seq` 持久化。
- **D. 能力 / 监控类** —— 给 agent 加「感官」或「控制面」。
  `browser`（`browserd` 守护 + `browserctl` 动作，空闲 600s 自关）、
  `computer`（截图 + 键鼠）、`okmon`（用 `ss -ltnp`/`pgrep`/`/proc` 发现并启停实例）、
  `voice-chat`（VAD→声纹→ASR→唤醒词路由→Kokoro TTS）。

`scout` 是横跨其上的**发现层**，不算业务形态。

---

## 五、多实例适配

OkHuman「多 agent」= 多独立端口进程，插件按形态各有适配策略：

- **CLI 类**：父链自识别端口，无需配置（`attach`）。
- **服务类**：配置里 `instances: [{ id, url }]` 显式列出；
  `cron` 每实例一份独立 `CronStore`；`qq-channel` 每 bot 绑一个 instance。
- **路由增强**：`voice-chat` 用 `instances[].wake_word` 把「小K/小B」分派到不同实例；
  `browser` 用 87xx 端口段约定，避开 OkHuman(845x) 与 cron(8601)。

---

## 六、目录与文件约定（速览）

每个插件目录自管以下内容（细则见 `RULE.md`）：

```
<插件>/
├── <同名>.go 或 cmd/     源码（单文件插件源码即 <插件>/<同名>.go）
├── go.mod                独立模块（可 cd 进去单独 go build）
├── config.json(.example) 自管配置；含密钥/部署值的真实 config.json 不入 git
├── data/                 运行时数据（token / cron.json / 锁 等），不入 git
├── README.md             启动 + 配置 + 排障
└── meta.json             agent 自写的一句话摘要（summary 必填），进 scout 索引
```

约定：路径一律**相对可执行文件所在目录**解析；状态落盘一律**原子写**（tmp+rename）。

---

## 七、新增 / 修改插件

1. 在本目录直接建扁平子目录，按「解耦契约」实现（独立进程、自带 `config.json`、`data/` 自管）。
2. 配好 `README.md` + 含非空 `summary` 的 `meta.json`。
3. 改完功能 → 更新 README / meta.json → `curl -X POST :8480/reindex` 刷索引。
4. 铁律：**改完源码 = 没干活；重编 + 真实启动验证 = 才干完**。

完整规范（含解耦契约逐条、Go 构建、索引判据）见 [`RULE.md`](RULE.md)。

---

## 八、文档索引

| 文件 | 作用 |
|---|---|
| [`RULE.md`](RULE.md) | 插件与技能仓管理规范（**唯一权威**） |
| [`../AGENTS.md`](../AGENTS.md) | 主程序项目指令（含关键默认值表、插件发现流程） |
| `<插件>/README.md` | 各插件的启动/配置/排障 |
| `<插件>/meta.json` | agent 自写简介，scout 索引信号 |
