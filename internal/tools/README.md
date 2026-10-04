# internal/tools — 唯一元工具 `bash`

> 一个进程 = 一个 agent，而 agent 的**一切外部操作**（命令执行、文件读写改、搜索、
> 抓被截断的 log 全文）都通过这一个工具完成。没有第二个工具。
>
> 本文基于对 `tools.go` 源码与测试的逐行核验（含实际 `go vet` / `go test` 运行），
> 标注了**代码实际行为**与**注释声称**的差异。

---

## 1. 职责边界

| 本包负责 | 本包不负责 |
|---|---|
| 生成工具表（`Specs()`），注入 LLM 请求 | 解析 LLM 返回的 tool_call（在 `internal/agent`） |
| 执行 `bash` 工具（`ExecuteTool`） | 前台/后台赛跑与轮内搭车（在 `internal/agent`） |
| 超时、杀整组、输出封顶 | 超长结果的 log 落盘与截断（在 `internal/agent.writeToolLog`） |
| 配置热更新（`Configure`） | 配置解析（在 `internal/config`） |

**对外 API（全部导出符号）**

| 符号 | 作用 |
|---|---|
| `Configure(fgMS, timeoutMS int)` | 设超时并重建工具表；启动时一次，`tools` 段热更新时再调 |
| `Specs() []types.ToolSpec` | 取当前工具表（注入 LLM） |
| `ExecuteTool(name, args)` | 执行工具，目前只认 `bash` |
| `ToolFailPrefix` | `"工具执行失败: "`，agent 与后台据此判定成败 |

---

## 2. 工具表是「由配置现生成」的

这是本包最重要、也最容易被误解的设计。

```
Configure(fgMS, timeoutMS)
        │
        ├─► toolTimeoutMS.Store(...)   ← 实际执行上限
        ├─► toolFgMS.Store(...)
        └─► buildTools(fgMS, timeoutMS) ─► toolsSpec.Store(...)  ← 注入模型的描述文本
```

**描述文本里的超时数字，来自同一个配置源**，不是写死的字符串：

```
"前台等待 %d 秒未完成自动转后台……命令总时长超 %d 秒被强制终止"   // fgSec, toSec
```

- **同配置 → 逐字同文本**：`TestConfigureFallbackAndStability` 断言两次
  `Configure(30000, 5000)` 产生的描述完全一致，保证 KV 前缀稳定。
- **改 `tools` 段 → 前缀作废一次**：这是**有意接受**的代价。旧实现曾出现
  「配置可改、描述写死 30s/600s」的两处不一致，模型基于假数字做决策。
  **宁可前缀作废，也不让描述与执行不一致。**

### 优先级与回落

`tools.timeout_ms` 的完整生效链：

```
内置默认 DefaultToolTimeoutMS(600000)
   └─< config/default.json (600000)
        └─< config/user.json 覆盖
             └─< 环境变量
```

`Configure` 内的**非法值回落**（`TestConfigureFallbackAndStability` 覆盖）：

| 输入 | 行为 |
|---|---|
| `timeoutMS <= 0` | 回落 `DefaultToolTimeoutMS`（600s） |
| `timeoutMS > MaxToolTimeoutMS`（24h） | 回落 `DefaultToolTimeoutMS` |
| `fgMS <= 0` | 回落 `DefaultFgTimeoutMS`（30s） |

> 上限设 24h 的理由：再大等于没有兜底。

---

## 3. 一次 `bash` 调用的完整生命周期

```
args.command ──► 写入 /tmp/okhuman-bash-<ms>-<hex>.sh (0700)
                     │
                     ├─ 为什么不进 argv：pkill/pgrep -f 按 argv 匹配，
                     │  旧 `bash -c <全文>` 会让任何命中命令文本的 -f 模式
                     │  匹配到包装壳自己 → 自杀 exit 143 / 自匹配假 pid（实测事故）
                     │
                     ▼
              exec.Command("bash", scriptPath)
                     + SysProcAttr{Setpgid: true}   ← 独立进程组
                     ▼
              cmd.Start() ──► stdout/stderr 各接 32MB cappedBuffer
                     ▼
              AfterFunc(timeoutSec) ──► syscall.Kill(-pid, SIGKILL)  ← 杀整组
                     ▼
              cmd.Wait() ──► 组装 "退出码: N" + 正文 + [stderr]
                     ▼
              defer os.Remove(scriptPath)
```

### 3.1 超时杀整组（防串行死锁）

`toolBash` 用 `Setpgid: true` 让命令成为**独立进程组组长**，超时执行
`syscall.Kill(-cmd.Process.Pid, SIGKILL)` —— 负号意味着杀**整组**。

原因（代码注释记载的真实事故）：agent 用 `nohup ... &` 起服务，后台子壳
握着输出管道写端不退出，`Wait` 等 EOF 永久挂住；而工具调用在
`internal/agent` 里是串行链 —— **一条命令挂住，后面全部堵死**（死锁放大器）。
杀整组把孤儿子进程（管道写端持有者）一并 SIGKILL，管道必关。

### 3.2 模型可下调、不可越权上调

| 情形 | 结果 |
|---|---|
| 不给 `timeout_seconds` | `timeoutSec = limitSec`（配置上限） |
| `timeout_seconds = 1` | 生效为 1 秒（下调） |
| `timeout_seconds = 99999`，上限 5s | **clamp 到 5 秒** |

`TestModelCanLowerButNotRaise` 覆盖上表。数值参数经 `numArg` 收敛 ——
按 kind 覆盖**全部**整数/浮点家族（`int8/16/32/64`、`uint/uint8…uint64`、
`float32/64`）并兼容 `json.Number`（`UseNumber()` 场景）：只认少数几种会把
其余类型静默退回默认上限，**行为与调用方意图相反**。
`numArg` 同时**拒绝** NaN / ±Inf / 超出 2^53 的整数——`int(+Inf)` 在 amd64 上
溢出为最小 int64，会被 `timeoutSec < 1` 兜底误判成 **1 秒**，比调用方意图短数个数量级。

### 3.3 输出封顶（32MB/流，审计 H3）

`cappedBuffer`：保留前 32MB，超出**丢弃但持续读取**并计数。

两个关键点：

1. **Write 永不报错、永不阻塞** → 管道保持排空，子进程不会阻塞在写管道上。
   若改成「超限即返回 error」，子进程会因管道写失败/阻塞而挂起。
2. **截断说明放在正文之前**：agent 的 `ResultLimit` 截断保留头部，
   说明放开头才能存活，模型据此知道丢弃量。

---

## 4. 起长驻服务的「脱离三件套」

工具描述里反复强调的模式，**必须三件都做**：

```bash
(setsid <启动命令> < /dev/null > /tmp/<名>.log 2>&1 &)
#        ▲三         ▲四                      ▲五
#  1 setsid  2 脱离父 shell 的管道  3 重定向 stdout/stderr  4 重定向 stdin  5 后台
```

- **只 `setsid` 不重定向**：脱离子进程仍持有输出管道写端 → `Wait` 收不到 EOF
  永久挂住，且超时杀组打不到它 → **上限彻底失效**。
- 脱离后服务独立存活，不受前台超时/转后台影响。
- 之后用 `curl` 探活、`cat` 读日志。

---

## 5. 超时结果的自述文本

超时被杀时，`head` 会追加：

```
退出码: -1（超过 N 秒上限被强制终止；要跑更久：用 setsid + 重定向完全脱离进程组，
或调大配置 tools.timeout_ms）
```

设计意图：光说「已超时」不够 —— 模型分不清「命令自己失败」与「被上限杀掉」，
会误判重试。**给出上限值与出路，让模型在「脱离」和「调配置」之间做选择。**

---

## 6. 实测记录与已知问题

以下均为**本次核验实际执行**的结果，非注释转述。

### 6.1 ⚠️ Windows 下本包无法构建

```
$ go vet ./internal/tools/
internal\tools\tools.go:158:41: unknown field Setpgid in struct literal of type syscall.SysProcAttr
internal\tools\tools.go:174:21: undefined: syscall.Kill
FAIL
```

`Setpgid` 与 `syscall.Kill` 是 Unix 专属调用。**与 `AGENTS.md` 验收标准一致：
验收需在 Linux 环境进行。** 若要支持 Windows，需抽象「执行域」并按 build tag
拆双实现（Job Object 杀整组 / 进程树终止）。

### 6.2 ⚠️ `tools_test.go` 缺少平台构建约束

`timeout_test.go` 已正确加 `//go:build !windows`（提交 `c5c355d`），
但**同目录的 `tools_test.go` 没有**，而它：

- `TestBashViaTempScript` 读取 `/proc/<pid>/cmdline`（Linux-only）

→ 在 Windows 上该测试会失败。**建议补 `//go:build !windows`。**

### 6.3 ⚠️ 注释与代码不一致：临时脚本删除时机

代码注释（L150-151）写：

> 文件在**进程退出后**删除（绑退出，不绑 30s 前台超时……绑退出是保留取证价值）

**实际行为**：`defer os.Remove(scriptPath)`（L156）绑定的是
**`toolBash` 函数返回**，而函数在 `cmd.Wait()` 返回时（即**前台命令结束**时）返回。

因此注释描述的「绑退出」语义**没有实现**：命令一旦转后台、或 `setsid`
脱离后仍在跑，脚本在 `toolBash` 返回时就已被删除 —— 所谓「保留取证价值」
在最需要取证的场景（长驻服务）恰好不成立。

> 影响：低（脚本内容 = 命令文本，agent 侧仍留有 tool_call 记录），
> 但**注释会误导后续维护者**，建议改代码（改用独立 goroutine 等待）
> 或改注释（如实说明绑前台返回）。

---

## 7. 测试

```bash
# 仅 Linux 可跑全量（见 6.1 / 6.2）
go test ./internal/tools/

# FakeLLM 模式不烧 token
OKHUMAN_FAKE=1 ./okhuman
```

| 测试 | 覆盖 |
|---|---|
| `TestCappedBufferWithinCap` | 未超限内容 = 原文 |
| `TestCappedBufferOverCap` | 恰好填满 + 全超出 + 说明在正文前 + 计数 |
| `TestCappedBufferPartialOver` | 部分超出（保留 4 + 丢 2） |
| `TestBashViaTempScript` | 命令文本不在 argv、输出正常、无脚本残留 |
| `TestConfigureDrivesDescription` | 配置值同时驱动描述文本 |
| `TestConfigureFallbackAndStability` | 非法值回落 + 同参数逐字同文本 |
| `TestConfiguredLimitKillsProcessGroup` | 上限生效 + 结果自报上限与出路 |
| `TestModelCanLowerButNotRaise` | 可下调、不可越权上调 |

---

## 8. 修改本包时请注意

1. **改超时语义 → 必须同步改描述文本**。描述由 `buildTools` 现生成，
   数字来自参数；不要在任何地方写死秒数。
2. **改 `tools` 段配置 → 前缀作废一次，属预期**。不要为「省前缀」而让
   描述与执行分家。
3. **不要去掉 `cappedBuffer` 的「持续读取」语义**，否则子进程会挂。
4. **不要去掉 `Setpgid` + `-pid`**，否则串行链死锁回归。
5. **跨平台改动**：本包当前是 Unix-only（见 6.1），动它要先想清 Windows 策略。
6. **新增导出符号 → 更新本文档第 1 节 API 表**。
