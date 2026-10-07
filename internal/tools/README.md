# internal/tools — 元工具 `bash`（+ 可选 `ipython`）

> 一个进程 = 一个 agent，而 agent 的**一切外部操作**（命令执行、文件读写改、搜索、
> 抓被截断的 log 全文）都通过这一个工具完成。没有第二个工具。
>
> 本文基于对本包源码与测试的逐行核验（含实际 `go vet` / `go test` 运行），
> 标注了**代码实际行为**与**注释声称**的差异。
>
> 2026-10-04 起本包已按 build tag 拆分（平台无关层 + `bash_unix.go` /
> `bash_windows.go` 双实现），三平台均可构建；详见 §6.1。

---

## 1. 职责边界

| 本包负责 | 本包不负责 |
|---|---|
| 生成工具表（`Specs()`），注入 LLM 请求 | 解析 LLM 返回的 tool_call（在 `internal/agent`） |
| 执行 `bash` 工具（`ExecuteTool`）与可选的 `ipython` | 前台/后台赛跑与轮内搭车（在 `internal/agent`） |
| 超时、杀整组、输出封顶 | 超长结果的 log 落盘与截断（在 `internal/agent.writeToolLog`） |
| 配置热更新（`Configure`） | 配置解析（在 `internal/config`） |

**对外 API（全部导出符号）**

| 符号 | 作用 |
|---|---|
| `Configure(fgMS, timeoutMS int)` | 设超时并重建工具表；启动时一次，`tools` 段热更新时再调 |
| `Specs() []types.ToolSpec` | 取当前工具表（注入 LLM） |
| `ExecuteTool(name, args)` | 分发工具：`bash` 恒在；`ipython` 仅在 `ConfigureIPython` 启用后可用 |
| `ConfigureIPython(python string)` | 启用/禁用 ipython 并重建工具表；`""` = 不注册（工具表与未接入时逐字一致） |
| `ResetIPythonSession(key string)` | 新会话边界（`/reset`）调用：作废旧会话内核，否则变量会跨会话污染 |
| `SetIPythonArtifactDir(dir string)` | ipython 图片/PDF 的落盘根目录（`<dir>/ipython-output`） |
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

### 2.1 描述文本的四段构成（后两段是平台专属）

`buildTools` 拼出的描述不是一整块字符串，而是四段：

```
基础句（固定） + 超时句（数字来自配置） + detachHint(toSec) + tmpHint()
                                          ▲平台专属          ▲平台专属
```

后两个函数**必须分平台实现**（`bash_unix.go` / `bash_windows.go` 各一份），
因为它们讲的是「本平台怎么绕开超时」和「本平台临时文件写哪」——这两件事在
两个平台上答案完全不同：

| 函数 | Unix | Windows |
|---|---|---|
| `detachHint` | `setsid` 三件套（能真正脱离） | 如实告知**无法脱离**（Job 为内核级隔离） |
| `detachHintBrief` | 短版，进超时自述 | 短版，进超时自述 |
| `tmpHint` | **空串**（`/tmp` 在 Unix 真实可写） | 警告别写 `/tmp`，改用 `$TEMP` |

**铁律**：

- `tmpHint()` 在 Unix **必须返回空串**。Unix 侧描述里本来就有 `detachHint` 依赖的
  `/tmp/<名>.log`；若把 Windows 的「不要写 `/tmp`」串进来，Unix 上会自相矛盾
  （一边教用 `/tmp`，一边说别用）。
- 反过来，Windows 侧不得丢失该提示——那是目前**唯一有效**的规避手段（见 §6.2d）。
- 守护测试：`TestDetachHintPlatformCorrect`（防串台）、
  `TestToolDescription_NoTmpWarningOnUnix`、`TestToolDescription_WarnsAgainstTmp`、
  `TestTmpHint_NonEmpty`。
- 改 `detachHint` 的 Unix 文案会**动摇历史 KV 前缀**（其 sha256 基线见 §6.1）。

---

## 3. 一次 `bash` 调用的完整生命周期

```
args.command ──► 写入 <os.TempDir()>/okhuman-bash-<ms>-<hex>.sh (0700)
                     │   Linux 上即 /tmp；Windows 上是 %TEMP%（如 C:\Users\..\AppData\Local\Temp）
                     │
                     ├─ 为什么不进 argv：pkill/pgrep -f 按 argv 匹配，
                     │  旧 `bash -c <全文>` 会让任何命中命令文本的 -f 模式
                     │  匹配到包装壳自己 → 自杀 exit 143 / 自匹配假 pid（实测事故）
                     │
                     ▼
              platformExecutor().Run(scriptPath, timeoutSec, stdout, stderr)
                     │
        ┌────────────┴────────────┐
        ▼ Unix                    ▼ Windows
   独立进程组 Setpgid          Job Object（三级策略）
   AfterFunc → kill(-pid)      AfterFunc → TerminateJobObject
   WaitStatus.Signaled()       isSignaledExit（三源判定）
        └────────────┬────────────┘
                     ▼
              ExecInfo{Reason, Code, Degraded, Killed, Signaled}
                     ▼
    bash.go：head = "退出码: N" (+ [平台降级]) + 正文 + [stderr]
                     ▼
              defer os.Remove(scriptPath)
```

### 3.1 超时杀整组（防串行死锁）

两个平台都实现「杀整棵树」，只是机制不同：

| | Unix | Windows |
|---|---|---|
| 执行域 | 独立进程组（`Setpgid`） | Job Object |
| 杀灭 | `kill(-pgid, SIGKILL)` | `TerminateJobObject(job, ...)` |
| 「死于信号」 | `WaitStatus.Signaled()` | `isSignaledExit`（三源，见 §6.2） |

**Unix**：`Setpgid: true` 让命令成为**独立进程组组长**，超时执行
`syscall.Kill(-cmd.Process.Pid, SIGKILL)` —— 负号意味着杀**整组**。

**Windows**：`CreateProcess` 时用 `PROC_THREAD_ATTRIBUTE_JOB_LIST` 把子进程
**创建时即放进 Job**（零竞态，级 1）；若不可用则退到「启动后 `AssignProcessToJobObject`」
（级 2）或「仅杀顶层」（级 3）。降级时 `ExecInfo.Degraded` 非空，且会以
`[平台降级]` 出现在模型可见的返回文案里（**不静默降级**）。

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
| `timeout_seconds = 1e300`，上限 5s | **clamp 到 5 秒**（见下方溢出说明） |

`TestModelCanLowerButNotRaise` + `TestOversizedTimeoutNotInvertedTo1s` 覆盖上表。
数值参数经 `numArg` 收敛 ——
按 kind 覆盖**全部**整数/浮点家族（`int8/16/32/64`、`uint/uint8…uint64`、
`float32/64`）并兼容 `json.Number`（`UseNumber()` 场景）：只认少数几种会把
其余类型静默退回默认上限，**行为与调用方意图相反**。
`numArg` 同时**拒绝** NaN / ±Inf / 超出 2^53 的整数——`int(+Inf)` 在 amd64 上
溢出为最小 int64，会被 `timeoutSec < 1` 兜底误判成 **1 秒**，比调用方意图短数个数量级。

**超大有限值的 clamp（2026-10-04 审查 P0 修）**：`numArg` 的 Float64 分支只挡
NaN/±Inf，挡不住 `1e300` 这类「有限但远超 int64」。旧实现直接 `int(f)`，而
`int(float64)` 溢出是 Go 规范的 implementation-specific 行为——amd64 得最小 int64
（负数），arm64 可能饱和为最大 int64。amd64 上接着被 `timeoutSec < 1` 反转成
**1 秒**：模型意图「跑很久」变成「1 秒被杀」，**且跨架构结果不一致**。
修法：**先在 float64 域 clamp 到 `[1, limitSec]` 再转 int**，永不进溢出区。

**配置粒度契约**：`tools.timeout_ms` 以**毫秒**为单位，但执行上限按**秒向下取整**
（`limitSec := int(toolTimeoutMS.Load()) / 1000`）。向下取整保证实际等待不超过配置值
（超时语义是「最多等多久」）。故 `timeout_ms=1999` → 上限 **1 秒**（静默丢弃 999ms），
`timeout_ms<1000` → `limitSec=0` → 回落默认 600 秒。

### 3.3 输出封顶（32MB/流，审计 H3）

`cappedBuffer`：保留前 32MB，超出**丢弃但持续读取**并计数。

两个关键点：

1. **Write 永不报错、永不阻塞** → 管道保持排空，子进程不会阻塞在写管道上。
   若改成「超限即返回 error」，子进程会因管道写失败/阻塞而挂起。
2. **截断说明放在正文之前**：agent 的 `ResultLimit` 截断保留头部，
   说明放开头才能存活，模型据此知道丢弃量。

---

## 4. 起长驻服务的脱离（Unix 三件套 / Windows 无解）

**先分清三件事**——「跑得久」和「永久长驻」是两个问题：

| 需求 | 怎么做 | Unix | Windows |
|---|---|---|---|
| 命令比 30 秒慢，但 600 秒内跑得完 | **什么都不用做**：前台超时自动转后台，跑完通知回来 | ✅ | ✅ |
| 需要超过 600 秒 | 调大 `tools.timeout_ms` | ✅ | ✅ |
| 要**永久**长驻（服务跑几天、跨会话存活） | **真正脱离进程组**（本节） | ✅ `setsid` 三件套 | ❌ 做不到 |

> **「自动转后台」≠「脱离」。** 后台任务仍由 OkHuman 持有——`toolBash` 的
> goroutine 还在跑，进程仍在同一个进程组 / Job 里，**仍受 `tools.timeout_ms`
> （默认 600 秒）总上限约束**，到期照样杀整树（Windows 上就是
> `TerminateJobObject`），且计时从**命令启动**算起，不是从转后台算起。
> 后台只是「模型不必傻等」，不是免死金牌。`internal/background` 里没有任何
> 超时/取消逻辑，只有 `<-Promise` 干等——超时依然归本包管。

本节只讲上表**第三行**，且**仅 Unix 适用**。Windows 见 §4.1。

```bash
(setsid <启动命令> < /dev/null > /tmp/<名>.log 2>&1 &)
#        ▲三         ▲四                      ▲五
#  1 setsid  2 脱离父 shell 的管道  3 重定向 stdout/stderr  4 重定向 stdin  5 后台
```

- **只 `setsid` 不重定向**：脱离子进程仍持有输出管道写端 → `Wait` 收不到 EOF
  永久挂住，且超时杀组打不到它 → **上限彻底失效**。
- 脱离后服务独立存活，不受前台超时/转后台影响。
- 之后用 `curl` 探活、`cat` 读日志。

### 4.1 Windows：**没有脱离这回事**

（澄清：本节针对的是「永久长驻」那一档。前两档——自动转后台、调大
`tools.timeout_ms`——在 Windows 上**都照常可用**，不受影响。）

Windows 侧把命令放进**系统级 Job 容器**，超时会连同后代一起终止，且
**没有任何逃逸手段**：Windows 无 `setsid` 等价物，Job Object 是**内核级
containment**，实测 `&` / `nohup` / 双 fork / `disown` / `cmd start` **全部被
`TerminateJobObject` 清除**。

因此 Windows 的工具描述**不能**照抄本节的三件套——那会给出在本平台做不到的
建议。正确做法是如实告知：长驻服务（网站/监控等）**不要用本工具启动**，
改由用户在工具之外用 Windows 原生方式启动（资源管理器双击、系统服务、
用户在终端手动 `Start-Process`），再用 `curl` 探活、`cat` 读日志。

这段措辞就是 `detachHint` 的 Windows 实现（见 §2.1）。

---

## 5. 超时结果的自述文本

> 注意区分两条通知：**前台 30 秒到期**时模型收到的是 agent 侧的
> 「此调用已自动转入后台继续执行」（`internal/agent` 产出，进程没被杀）；
> 本节说的是**总上限 `tools.timeout_ms` 到期**、进程真被杀掉时的文案。

超时被杀时，`head` 会追加：

```
退出码: -1（超过 N 秒上限被强制终止；要跑更久：用 setsid + 重定向完全脱离进程组，
或调大配置 tools.timeout_ms）
```

设计意图：光说「已超时」不够 —— 模型分不清「命令自己失败」与「被上限杀掉」，
会误判重试。**给出上限值与出路，让模型在「脱离」和「调配置」之间做选择。**

### 5.1 超时判定的权威口径（2026-10-04 审查 P1 修）

**是否追加超时文案 = `isTimeoutKill(killed, signaled)` = 我们发过 kill **且** 进程死于信号。**

- `killed`：计时器回调是否成功发出过杀灭（Unix `SIGKILL` / Windows `TerminateJobObject`）。
- `signaled`：被回收的进程是否**死于外部终止**而非自行退出。**平台定义不同**：
  - Unix：`ExitError.Sys().(syscall.WaitStatus).Signaled()`（权威）。
  - Windows：`isSignaledExit`（三源判定，见 §6.2）——**不能用朴素的
    `code >= 0x80000000`**，因为 Git Bash 用 `signo<<8` 编码信号死亡，
    而我们自己的 `TerminateJobObject` 退出码是可控值。

两个条件**缺一不可**，原因（两平台**同构**）：

- **只看 `killed`**：命令恰好在计时器触发同一瞬间自行 `exit(0)` 时，
  杀灭会打在「已退出但尚未被回收」的对象上并**返回成功**（Unix: 杀僵尸组是成功的
  空操作；Windows: `TerminateJobObject` 对空 Job 同样成功，且**已退出进程的退出码
  不可变**）→ `killed=true` 而进程其实**正常退出** → 输出
  `退出码: 0（超过 N 秒上限被强制终止）` 的**自相矛盾文案**
  （旧实现实测相位扫描约 2–3% 命中）。即「Kill 成功」推不出「是我们杀死的」。
- **只看 `signaled`**：命令自己 `kill -9 $$`（Unix）也会 `Signaled=true`，
  但那不是本工具超时。Windows 上对应 `kill -9 $$` → 退出码 `0x900` → 三源②命中。

判定抽成纯函数 `isTimeoutKill` 以支持**确定性单测**：真实竞态是概率性的，
集成断言会漏（变异测试证实 40 次迭代抓不到把 `&& signaled` 删掉的回退——
**假绿**）。`TestIsTimeoutKillDecisionTable` 覆盖四种 `(killed, signaled)` 组合，
`TestBashTimedOutTextFromDoubleCondition` 覆盖四条端到端语义（含两个反例）；
Windows 另有 `TestIsSignaledExitThreeSources`（18 断言）锁死三源。变异测试验证其能捕获回退。
集成测试 `TestKilledRaceNoContradictoryZeroExit` 作端到端补充（`-short` 下跳过）。

回调里另保留 `done` 快路径（Wait 已返回 → 直接不做），减少无谓的 kill 尝试。
`ExitReason` **不取代双条件**：它由双条件推导、仅作交叉校验（不一致 → 加
`reason-mismatch` 降级说明）。**切勿写成 `timedOut := info.Reason == ExitTimedOut`**。

### 5.2 临时脚本的删除时机（契约，非缺陷）

`defer os.Remove(scriptPath)` 绑定的是 **`toolBash` 返回**，不是真正的进程退出：

- 命令转后台、或 `setsid` 完全脱离后，`toolBash` 已返回、脚本已被删除，而命令**仍在运行**。
- 这是**有意为之**：提前 unlink 不中断执行（已在运行的进程继续持有该 inode），
  且避免脚本堆积。

**契约**：任何「读取自己脚本文件」的用法（如 `bash $0` 自引用）在本工具下**不可靠**——
脚本在 `toolBash` 返回时即被删除。当前 `bash.go` 不涉及；后续新增工具
（如 `ipython.go`）若需要脚本持久化，必须自行管理生命周期，不要沿用 `defer os.Remove`。

---

## 6. 实测记录与已知问题

以下均为**本次核验实际执行**的结果，非注释转述。

### 6.1 ✅ 跨平台构建（2026-10-04 已实现，原为「Windows 无法构建」）

```
$ go build ./... && go vet ./... && go test ./...     # Windows 本机
ok  okhuman/internal/tools    19.5s
$ go test ./...                                       # WSL Ubuntu
ok  okhuman/internal/tools    68.8s
```

**历史**：原实现把 `Setpgid` 与 `syscall.Kill` 直接写在 `bash.go` 里，Windows 编译失败
（`unknown field Setpgid` / `undefined: syscall.Kill`）。2026-10-04 按
`docs/跨平台-bash-工具完整方案-v2-实证修订.md` 分四阶段完成移植：

| 阶段 | 内容 | 结果 |
|---|---|---|
| 1 | 拆出平台无关层 `exec_domain.go` + 平台实现 `bash_unix.go` / `bash_windows.go` | ✅ |
| 2 | 外化 `executor` 接口（`platformExecutor()` + 测试注入点） | ✅ |
| 3 | Windows 执行器实现（Job Object 三级策略 + 真实 Git Bash 端到端） | ✅ |
| 4 | 文档同步（本节） | ✅ |

**平台分工**：

| 文件 | build tag | 职责 |
|---|---|---|
| `exec_domain.go` | 无 | 平台无关契约：`executor` 接口、`ExecInfo`、`ExitReason`、`isTimeoutKill` |
| `bash.go` | 无 | 平台无关主体：参数解析、写脚本、调接口、文案组装 |
| `bash_unix.go` | `unix` | `Setpgid` + `kill(-pgid)` 杀整组 + `WaitStatus.Signaled()` |
| `bash_windows.go` | `windows` | Job Object 三级策略 + `TerminateJobObject` 杀整树 |
| `findbash_windows.go` | `windows` | bash 探测（缓存 + `Stat` 校验 + 排除 WSL relay） |
| `winenv_windows.go` | `windows` | 环境构造：**PATH 前置 Git `usr\bin`** + `MSYS_NO_PATHCONV=1` 注入 + 手工 UTF-16 环境块 |

**Unix 语义逐字保留**：`isTimeoutKill` 双条件、定时器 `time.AfterFunc` + 双 atomic、
`kill(-pgid)` 全部未改——移植只做「搬运 + 外化」，不改判定逻辑。

### 6.2 ⚠️ Windows 的 `signaled` 必须三源判定（实证修正，勿退回）

**这是移植中最重要的坑**，详见方案 §8.1.4。简言之：

`TerminateJobObject(job, N)` 把退出码**逐字设为 N**；若传 1，退出码就是 1
→ `signaled=false` → `isTimeoutKill(true,false)=false` → **真超时被静默报成正常退出**。
另外 Git Bash 用 `signo<<8` 编码信号死亡（`kill -9 $$` → `0x900`），
**不是**高位 NTSTATUS，故「`code >= 0x80000000`」这一朴素公式会漏判。

```go
// bash_windows.go
const terminatedByUsCode = 0x0000FFFF   // 我们的 TerminateJobObject 传这个哨兵

signaled = code >= 0x80000000                        // ① 高位 NTSTATUS（崩溃 / Ctrl-C）
        || code == terminatedByUsCode                // ③ 本工具杀活进程
        || (code >= 0x100 && code&0xFF == 0 && code>>8 <= 64)  // ② Git Bash 信号码
```

**不可伪造性**：自然 `exit N` 被截断到低 8 位（恒 0..255），脚本无法造出 65535；
信号码低字节恒为 0。三条陷阱（僵尸组假成功 / 命令自杀 / 真超时）均已用真实 bash 验证。

### 6.2b ⚠️ Agent 通道必须前置注入 Git `usr\bin`（实证修正，勿删）

**元工具用的是非 login shell**（`bash <script>`，**无 `-l`**）→ 不读 `/etc/profile`
→ **PATH 完全继承父进程**。Agent 进程若从 Windows 原生血统启动（cmd / 资源管理器双击 /
系统服务），PATH 里没有 Git 的 `usr\bin`，后果：

- `cat` / `grep` / `head` / `sed` / `awk` / `ls` / `tr` / `wc` / `date` → **全部
  `command not found`**；
- 更危险：`find` / `sort` **静默命中 `C:\WINDOWS\system32\` 下的 Windows 同名程序**
  （Windows `find.exe` 是字符串查找！）→ 给出错误结果而不报错。

**修法**（`winenv_windows.go` 的 `buildChildEnv`）：用 `findBash()` 的探测结果反推
`usr\bin` 并**前置**到 PATH（前置才压得住 System32 同名程序）。

**测试纪律**：环境构造拆成纯函数 `buildChildEnv(base)` 就是为了可测——
用 `childEnv()` 测会因它内部读 `os.Environ()` 而**假绿**（变异测试已抓出三次）。
**改动此函数后，务必跑 `TestAgentChannelCoreutilsAvailable` 的变异验证。**

### 6.2c ⚠️ Windows 两个「隐藏前提」缺陷（2026-10-04 bug 猎捕修复，勿退回）

移植后的 Windows 实现（基线中**不存在**，整套新增）有两处依赖了**未言明的前提**，
在 ASCII/单进程等常见输入下全绿，故长期潜伏。均以真实 bash 端到端实测定位。

**缺陷 A：管道搬运的关闭顺序颠倒 → 后台任务输出丢失**

```go
// ❌ 修复前：wait() 返回（顶层退出）后立刻关读端
waitCode := run.wait()
windows.CloseHandle(outR); windows.CloseHandle(errR)   // 后台任务仍持有写端！
wg.Wait()
// ✅ 修复后：先读到 EOF（wg.Wait）再关句柄
waitCode := run.wait()
wg.Wait()
windows.CloseHandle(outR); windows.CloseHandle(errR)
```

- **隐含前提**：*「顶层进程退出 ⇒ 写端全部释放」*。该前提**只在没有存活后代时成立**。
- **后果**：脚本把命令放后台（`cmd &`）时，后台任务在顶层退出后仍持写端；
  提前关读端使 `copyHandleTo` 的 `ReadFile` 立刻报错返回 → **输出丢失**。
  实测：100 行只收到 **1** 行（丢失 99）。
- **对齐依据**：Unix 用 `os/exec`，`cmd.Wait()` 内部**会等管道拷贝 goroutine 读到
  EOF**——这是标准库的"隐式正确"。自建 `CreateProcess` 时必须显式复刻该顺序。
- **守卫**：`TestE2E_BackgroundTaskOutputNotLost`（用 `sleep 0.1` 制造
  "顶层已退、后台仍持写端"的**窄窗口**；已通过变异验证）。
  ⚠️ 场景设计要点：**不能用 `wait` 让顶层等后台**——那会抹掉缺陷窗口，
  测试会**假绿**（已实测：变异不报红）。必须让顶层先退。

> **附：与之相关的一个"非缺陷"**——「顶层退出 + 后台长驻任务持写端」会让本步**长时间
> 等待**（等后台任务结束）。经实测，**Unix 侧行为完全相同**（`wait()` 语义就是等所有
> 写端释放），且 `detachHint` 已**文档化**该陷阱（"只 setsid 不重定向…Wait 收不到 EOF
> 会永久挂住"）。故 Windows **刻意不做**"强杀后台任务"的额外处理——那会偏离跨平台契约。

**缺陷 B：环境块 UTF-16 编码漏了代理对 → 非 BMP 字符损坏 / 进程起不来**

```go
// ❌ 修复前
for _, r := range e { u16 = append(u16, uint16(r)) }   // rune > 0xFFFF 静默截断
// ✅ 修复后
u16 = append(u16, utf16.Encode([]rune(e))...)
```

- **后果**（均为实测）：
  - emoji `U+1F33F` → `uint16(0x1F33F)=0xF33F` → 子进程收到乱码 `\uf33f`（**静默损坏**）；
  - CJK 扩展 B `U+20000` → `uint16(0x20000)=**0**` → **注入裸 NUL**，环境块中途被截断
    → `CreateProcess: The parameter is incorrect.` → **三级策略全失败，进程根本起不来**。
- **隐蔽原因**：`winenv_windows.go` 的注释**记录并避开了 NUL 坑**（不用
  `UTF16PtrFromString`），但**代理对坑**只在非 BMP 输入下触发 → ASCII/BMP 测试全绿，
  长期潜伏。
- **守卫**：`TestE2E_NonBMPEnvNotCorrupted`（ASCII / BMP 中文 / emoji / CJK 扩展 B / 混合
  五档，已通过变异验证）。

### 6.2d ⚠️ Windows 上不要写 `/tmp`（2026-10-05 实证，勿退回）

Windows 的 `/tmp` 是 MSYS 的 `usertemp` 挂载点，**挂载目标由 MSYS 运行时自行推导**。

**先说清楚：这层「`/tmp` → `%TEMP%` 的转义」MSYS 平时是做好的。** 本机实测
（2026-10-05）：login 与非 login（`bash <script>`，即 OkHuman 的真实形态）两种
形态下 `echo hi > /tmp/x.txt` **均成功**，文件真实落在
`C:\Users\peli\AppData\Local\Temp`，Windows 侧可见。**它不是常态故障。**

故障是**间歇性**的（同一条命令时好时坏）：当 MSYS 启动时挂载目标已被回收或
不存在，bash 报 `could not find /tmp, please create!`，此时写 `/tmp` 才失败
（`No such file or directory`）。

- **所有「注入环境变量」的修法全部无效**：`TMP` / `TEMP` / `TMPDIR` / `MSYS_TMP`
  四个都改不动 `/tmp` 的挂载目标（四组控制变量实测，目标纹丝不动）。
- **OkHuman 改不动 MSYS 挂载**。有效的杠杆只在**行为层**：靠 `tmpHint()`
  写进工具描述，让模型自己别写 `/tmp`、改用 `$TEMP` 或当前目录。
- 附带实证：WSL 里写到 `/tmp` 的探针文件，中间再取时**已经消失**——
  「`/tmp` 不可靠」在 Linux 侧同样成立（改用 `$HOME` 才稳定）。

> **⚠️ 未解决的待办**：模型若无视提示仍写 `/tmp` 仍会失败（描述是软约束）。
> 更麻烦的是——`bash.go` 自己写脚本的位置是 `os.TempDir()`，Windows 上
> = `%TEMP%`，而 `/tmp` 常挂载到此处 → 模型的 `rm -rf /tmp/*` 存在
> **误删正在执行的脚本**的自引用隐患。尚未修复。

### 6.3 临时脚本删除时机：注释与代码不一致（已修正为契约）

代码注释曾写「文件在**进程退出后**删除（绑退出，保留取证价值）」，但
`defer os.Remove(scriptPath)` 绑定的是 **`toolBash` 返回**（= 前台命令结束时），
两者语义不符 —— 命令转后台 / `setsid` 脱离后仍在跑，脚本此时已被删除。

**2026-10-04 处理**：注释与 README 已改为**如实描述**（绑 `toolBash` 返回，
提前 unlink 是有意为之、不中断执行），并把「脚本不持久」列为**显式契约**
（见 §5.2），避免后续工具误以为脚本文件可自引用。

> 影响：低（脚本内容 = 命令文本，agent 侧仍有 tool_call 记录）。
> 未改成「绑真正进程退出」：那需要独立 goroutine 跟踪进程生命期，
> 复杂度高于收益，且当前无消费方需要脚本留存。

---

## 7. 测试

```bash
# 全平台（Windows 本机 / Linux / WSL 均可）
go test ./internal/tools/

# FakeLLM 模式不烧 token
OKHUMAN_FAKE=1 ./okhuman
```

**按平台分层**：

| 层 | 文件 | 需要真实 bash？ | 验证什么 |
|---|---|---|---|
| 平台无关 | `executor_test.go` | 否（用 `fakeExecutor`） | 双条件判定、降级文案、`reason-mismatch` 交叉校验 |
| Unix 专属 | `tools_test.go` / `timeout_test.go`（`!windows`） | 是 | `/proc` 读取、杀进程组 |
| Windows 专属 | `bash_windows_test.go` / `bash_windows_e2e_test.go`（`windows`） | 是（Git Bash） | `isSignaledExit` 三源、超时杀整树、env 注入、句柄收敛 |

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
| `TestOversizedTimeoutNotInvertedTo1s` | `1e300`/`9.3e18`/`MaxFloat64` 不反转成 1 秒（P0 回归） |
| `TestIsTimeoutKillDecisionTable` | 超时判定四组合（确定性，P1 回归） |
| `TestBashTimedOutTextFromDoubleCondition` | 双条件 → 文案（含僵尸组假成功 / 命令自杀反例） |
| `TestBashDegradedSurfacedToModel` | 降级说明进入模型可见文案 |
| `TestKilledRaceNoContradictoryZeroExit` | 端到端无「退出码 0 + 超时」矛盾（概率性，`-short` 跳过） |
| `TestNumArgTypeCoverage` 等 | `numArg` 类型覆盖 / NaN/Inf / 2^53 守卫（见 §3.2） |
| `TestExecuteToolUnknownNameEscaped` | 未知工具名 `%q` 转义 |
| `TestDetachHintPlatformCorrect` | 平台专属文案不串台（防 Unix/Windows 互相混入） |
| `TestToolDescription_NoTmpWarningOnUnix` | Unix 描述无 Windows 的 `/tmp` 规避提示，且仍含 `/tmp/` 用法 |
| `TestToolDescription_WarnsAgainstTmp` | Windows 描述含 `/tmp` 警告与 `$TEMP` 替代（缺一模型就无从规避） |
| `TestTmpHint_NonEmpty` | Windows 侧 `tmpHint` 非空（空 = 提示丢失） |
| `TestIsSignaledExitThreeSources` | Windows `signaled` 三源判定（18 断言，防退回 §4.4 错误公式） |
| `TestTerminatedByUsCodeNotForgeable` | `exit 65535` 被截断 → 哨兵不可被脚本伪造 |
| `TestKillSelfIsNotTimeout` | `kill -9 $$` → `signaled=true` 但 `killed=false` → 不报超时 |
| `TestE2E_*` | Windows 真实 Git Bash：echo/退出码/含空格路径/env/1MB/超时杀整树 |

---

## 8. 修改本包时请注意

1. **改超时语义 → 必须同步改描述文本**。描述由 `buildTools` 现生成，
   数字来自参数；不要在任何地方写死秒数。
2. **改 `tools` 段配置 → 前缀作废一次，属预期**。不要为「省前缀」而让
   描述与执行分家。
3. **不要去掉 `cappedBuffer` 的「持续读取」语义**，否则子进程会挂。
4. **不要去掉 Unix 的 `Setpgid` + `-pid`、Windows 的 Job Object**，
   否则串行链死锁回归。
5. **不要在 `bash.go`（无 tag）里引入任何平台 API**——那会立刻破坏跨平台构建。
   新增平台逻辑必须放进 `*_unix.go` / `*_windows.go`。
6. **Windows `signaled` 三源判定不得简化**（见 §6.2）——改成
   `code >= 0x80000000` 会漏判真超时与真自杀。
7. **自建 `CreateProcess` 必须关句柄**：`CloseHandle(pi.Thread)` 立即、
   `defer run.cleanup()` 关 Process 与 Job。漏关 = 每次调用泄漏 2 个。
8. **新增导出符号 → 更新本文档第 1 节 API 表**。
9. **平台专属文案（`detachHint` / `detachHintBrief` / `tmpHint`）必须分平台实现**，
   不得在 `bash.go` 里用 `runtime.GOOS` 分支或写死任一平台的措辞（见 §2.1）。
   尤其别让 Windows 的「不要写 `/tmp`」漏进 Unix 描述——Unix 的
   `detachHint` 本身就教模型往 `/tmp/<名>.log` 写日志。
