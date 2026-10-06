# 变更记录

本文件按时间倒序记录有外部可见影响的变更。新增/移除配置项、性能或安全相关变更必须登记（见 `AGENTS.md` 文档维护表）。

---

## [Unreleased] — 2026-10-06

### fix(agent): 四轮反思 —— 上一轮在错误的维度修内存问题，反而裁掉了检测能力

上一轮给 `trimCallRecs` 加了 `maxCallRecs = 64` 硬上限。方向错了：
内存瓶颈是"每条记录太大"，不是"保留几条"，而条数恰恰是检测能力所需。

- **回归（上一轮引入）**：`doom.warn_after` 设 100 / 200 时，滑窗只留 64 条 →
  `isSameRunLocked(n)` 永远凑不齐 n 条 → **检测永久失效**。用户把阈值调大反而
  彻底关掉了死循环检测，与意图完全相反（实测确认）。上限放宽到 4096。
- **根因修复**：`stableKey` 原本返回 `name + 完整参数 JSON`，而 Key **只用于相等比较**
  （`c.Key != first`），从不展示/解析——存全文纯浪费（实测 100KB 的命令产出 100018 字节 Key）。
  改为定长摘要（SHA-256 前 16 字节 hex），单条从"几 KB"降到"几十字节"，
  内存与参数长度脱钩，"限制条数"这个手段从此不再必要。
- **方法论教训（写进注释）**：上一轮的测试只断言"裁剪后有界"，没断言"裁剪后仍够
  isSameRunLocked 用"——**测试偏向了当时的结论**。补 `TestProbeDoomDetectableAtLargeThreshold`。

新增探针 2 组（累计 8 组），**变异验证 2/2 变红**。

### fix(agent): 三轮反思 —— 上一轮"在使用处强制不变量"执行得不彻底，另有两处耦合/文档不一致

上一轮立了铁律"不变量要在被使用的地方强制，不能只放 `Run` 入口"，但只对 `DataDir`
做了、漏了同类项；且未检查"兜底值是否与文档一致""正交关注点是否被同一个参数绑死"。

- **文档假话**：`defaultMaxToolRounds = 100`，而 `config/default.json` 与 `AGENTS.md`
  关键默认值表**都写 200**——"0/负 → 200"实际给的是 100。改为 200。
- **耦合陷阱**：`trimCallRecs` 的滑窗大小直接取 `doomWarnAfter+1`，且 `n<=0` 时直接
  `return` 一条不裁 → 把 `doom.warn_after` 设成 0（关闭死循环检测）会**顺带关掉内存保护**
  （`callRec.Key` 存完整参数 JSON，bash command 动辄几 KB，长任务真能堆满）。
  新增 `maxCallRecs = 64` 硬上限，与检测阈值解耦；阈值设很大（如 1000）也不再真留 1001 条。
- **漏做的就地防御**：`formatTruncatedWithHead` 的 `limit <= 0` 会让预算直接归零，正文被
  截成空（工具结果等于全丢）。与 `DataDir` 同类，补上就地取 `defaultResultLimit`。

新增探针 3 组，**变异验证 3/3 变红**。

### fix(agent): 二次反思（第一性原理）—— 修掉上一轮归一化自身引入的两处缺陷

上一轮"把 0/负数/空串统统兜成默认值"的归一化，经二次反思发现它把**用户显式意图**
当成非法值改掉了，且其中一项兜底本身就是错误位置。本轮修正：

- **语义灾难**：`doom.warn_after: 1` 时 `isSameRunLocked(1)` 对单元素序列恒为 true →
  **第一次**工具调用就吃"你已连续 1 次用完全相同的参数调用 X"的警告（实测确认）。
  → 判定改为 `n < 2` 一律"不是"（0/1 = 关闭检测），并**撤销**对 `DoomWarnAfter`
  的归一化——用户写 0 是想关掉死循环检测，不该被偷偷改成 3。
- **错误落盘位置**：`DataDir` 为空时，`filepath.Abs` 把空路径解析成**进程当前工作目录**
  → 工具日志真写进了 cwd（跑测试时即 `internal/agent/tool-results/`，污染仓库）。
  → `writeToolLogOK` 就地禁止：`DataDir` 空 = 不落盘、返回失败，提示文案如实告知
  "全文不可找回"，不给假路径；**撤销** `os.TempDir()` 兜底（临时目录会被系统清理，
  且与实例数据目录无关）。
- 归一化项与"刻意不归一化"项现在都在 `normalizeRunOptions` 上逐条论证了取舍理由。

新增 `internal/agent/probe_reflect_test.go`（语义探针 3 组），四处修复**变异验证 4/4 变红**。

### fix(agent): 代码审查实证核验 —— 修复 11 项确认缺陷（`internal/agent`）

对 `internal/agent/agent.go` 的一轮"第一性原理"审查报告（16 条）逐条实证核验后落地。
判定表与证据见 `docs/agent-代码审查-实证核验-2026-10-06.md`。

- **致命**：`DoomWarnAfter <= 0` → `isSameRunLocked` 取 `tail[0]` **panic**（配置 `doom.warn_after: 0`
  即可触发，一次手滑打挂实例）。入口 `normalizeRunOptions` 兜默认值 + 函数内再防御一层。
- **高危**：4xx 回退阶梯第三级**顺序反了**——先 `Compose` 后 `ForceHardTruncate`，截断改不了
  已组装的切片，重试发的仍是被拒的超长请求，兜底形同虚设。改为**先截断再组装**。
- **高危**：`systemPrompt` 字段只写不读（热更新实际靠 server 额外调 `cm.SetSystemPrompt` 才生效）。
  删除该字段，`Agent.SetSystemPrompt` 直接委托给 `cm`（系统提示词单一真相源）。
- **中危**：异常截断重试缺上下文（未落那条"只有思考"的 assistant 消息）+ 未回滚已流出的 delta
  （前端把两次内容拼成脏内容）；doom 警告点名错（`A,A,A,B` 会说成 B）且同批立刻把 `warned`
  复位（模型每批末尾换个调用就**永远停不下来**）；后台任务 `StartedAt` 取在超时那一刻
  （耗时长少算整段前台等待）；工具 goroutine 无 `recover`（工具 panic 打挂进程）；
  `run_end` 早于清理（其后再收本轮压缩事件，前端轮次错乱）；
  `ResultLimit`/`FgTimeoutMS` 为 0 时行为崩坏（结果截空 / 随机转后台）；空响应返回空串（空气泡）。
- **低危**：`writeToolLogOK` 不保证绝对路径；消息队列靠文本前缀区分系统通知与用户追加
  → 改为结构化 `PendingMessage{Text, System}`（server 队列同步带 `system` 标记，
  前端 `user_piggyback` 优先用该字段）。
- **未改（复核后判定不成立 / 有意为之）**：轮次上限丢弃未执行的 `tool_calls`（存了就得补合成
  tool 结果，是噪音；不存则上下文干净且协议合法）；"并行工具调用"实为顺序执行（bash 共用工作
  目录、ipython 共用内核，并发会互踩 → 保留顺序执行，改注释）；`/stop` 与 run 起始的布尔复位
  竞态（语义上"停止只作用于在飞那一轮"是对的）。
- **测试**：新增 `internal/agent/audit_2026_10_test.go`（K~R 共 9 组），六处关键修复做了
  **变异验证**（把修复改回去 → 对应用例必须变红，实测全部变红）。

---

## [Unreleased] — 2026-10-05

### feat(tools): 新增 ipython 元工具（可选，默认关闭）

在单一元工具 `bash` 之外，新增 `ipython` 工具：一个**常驻 Python 进程**，变量、导入、函数跨调用保留，适合多步数据探索。

- **路线**：子进程 stdin/stdout 走 JSON-Lines，内联 embed 的 `launcher.py` 驱动 `IPython.InteractiveShell.instance()`。**不是 Jupyter kernel**——不实现 wire protocol，JupyterLab / VSCode / nbclient 连不上，无 comm / ipywidgets / nbformat。
- **依赖最小**：只需 `pip install ipython`，**不需要** ipykernel / pyzmq / ZMQ（本机无 gcc，cgo 绑定与 `pebbe/zmq4` 路线已实测否决）。
- **默认关闭**：`tools.ipython_python` 为空时不注册该工具，工具表与未接入时逐字一致，不影响既有 KV 前缀契约（由 `TestIPythonAbsentByDefault` 守护）。
- **配置**：`tools.ipython_python` = `""`（不启用）/ `auto`（探测第一个装了 IPython 的解释器）/ 解释器绝对路径。三处同步：`config/default.json`、`config/user.json.example`、`AGENTS.md`（`internal/config/config_test.go` 会拦截漏项）。WebUI 配置页已提供该字段（数据驱动，无需重编）。
- **中断**：三级级联——`raise_signal(SIGINT)` → `pthread_kill(主线程, SIGINT)` → 硬杀；中断后变量保留。
- **交互输入**：`input()` / `getpass()` 被接管为快速失败（抛 `InputUnavailable`），避免工具挂死（实测从 23.8s 降到 0.64s）。
- **图形与富输出**：`matplotlib` 强制 inline 后端（`module://matplotlib_inline.backend_inline`，仅 `configure_inline_support` 不够），`plt.show()` 出图自动落盘；`display()` 通过实例级 patch `publish` 捕获（IPython 9 无 `publish_display_data`），回显为 `display[N]`；`image/png|jpeg|gif|webp`、`application/pdf` 落盘到 `<dataDir>/ipython-output/`。

### perf(tools): 精简 ipython 工具描述（−63 token/轮）

按「**写差异，不写常识**」重写 `buildIPythonSpec` 的描述：模型的 Python 先验已覆盖用法，
提示词只需交代本实例偏离标准行为之处。

- **删**用法说明与推销语：`%magic（如 %timeit）`、`!cmd 执行 shell`、`In/Out 历史与 DataFrame 富展示`、
  `适合数据处理/可视化/科学计算/AI-ML`、以及多余的 `真` 字（零信息修辞）。
- **留/改**五条猜不到或会猜错的契约：状态跨调用保留（`常驻进程`——正向陈述优于
  `不是每次新建` 这类双重否定）、
  超时双限（取 `DefaultSec()/MaxSec()`，仍不写死）、`超时先中断（变量不丢）· 无法中断时重启（丢变量）`、
  图片/PDF 落盘给绝对路径、`无交互输入：input() / getpass 会立刻报错，请把值直接写进代码`
  （先验有害，必须显式否定 + **给明确替代动作**，不能只陈述症状）。
- **留**路由句（原 `不适合 grep/sed 这类纯文本处理，那用 bash。` → 精简为 `grep/sed 纯文本处理，用 bash。`）。
- 参数 `code` 的描述同步精简（`…（最后一条表达式自动求值；可用 %magic 与 !cmd）` 删除）。
- 实测（配置中的线上后端，同一 system/user 只换 tools）：ipython 段 **250 → 184 token**（−66，26%），
  整请求 **2727 → 2661**。每条消息都付，200 轮会话省约 1.3 万 token。
- 代码注释里固化该取材铁律，防止后人重新把推销文案加回去。

### fix(tools): 修复与 ipython 相关的三个缺陷

- **会话隔离未接线**：`SetSessionKey` 此前全仓无生产调用 → `POST /reset` 现在会重置 ipython 会话（端到端验证：`/reset` 后读变量得 `NameError`）。
- **`humanBytes` 单位错档**：19139 字节曾被报为 "18.7 MB"，修正单位表（`TestHumanBytes` 守护）。
- **占位 repr 抢富内容**：`display(HTML)` 的 `text/plain` 是 `<…HTML object>` 单行占位，`pickResultText` 现让位给 `text/html`。

### fix(server): POST /config 反馈与错误摘要

- `applied` 文案改为条件分支：仅当 `tools.ipython_python` 实际变化时追加 `已启用（<路径>）` / `已停用` / `未启用（<原因>）`，避免每次保存都报。
- `auto` 探测失败的错误从完整 traceback 压成一行摘要（`summaryLine` + `truncateErr(…,200)`），不再把长堆栈显示到 WebUI。

### docs: 新增 ipython 文档并同步三处入口

- 新增 `docs/ipython-配置参考.md`（配置面完整梳理：唯一开关、各途径真假、`auto` 探测、热更新约束、暂不实现的途径）。
- 新增 `docs/ipython-方案实证评审.md`、`docs/ipython-实施方案决策.md`、`docs/ipython-最佳实践-级联中断实证.md`、`docs/assets/ipython_launcher.py`。
- 同步 `README.md`（启用方式、能力边界、多实例共享配置）、`AGENTS.md`（验收与排错、文档链接）、`internal/tools/README.md`（标题改为 "bash（+ 可选 ipython）"）。

### chore: 忽略其它会话的临时目录

`.gitignore` 新增 `.ipynb_checkpoints/`、`.reasonix/`（其它 agent 会话残留，不入库）。
