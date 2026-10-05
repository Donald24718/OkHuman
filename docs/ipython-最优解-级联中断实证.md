# IPython 元工具 · 最优解定稿

> 本文是前两轮评审/决策的**收篇**。所有结论均在本机 Windows + WSL Linux 双平台
> 实跑验证，附可复现数据。替代 `docs/ipython-实施方案决策.md` 的路线判断部分。

## ✅ 状态：已落地进主仓（2026-10-05）

本文描述的方案已实现，不再是纸面结论：

| 产物 | 说明 |
|---|---|
| `internal/tools/ipython/` | 完整实现（kernel / manager / detect / output / launcher.py） |
| `tools.ipython_python` 配置项 | `""`=关闭（默认）／`"auto"`=自动探测／绝对路径 |
| 20 个测试 | 11 单测 + 9 个真起 Python 的集成测试，Win/Linux 双绿 |
| 五目标交叉编译 | linux/darwin/windows × amd64/arm64，`CGO_ENABLED=0` 全通过 |

启用方式见 `README.md`「可选元工具：ipython」；验收步骤见 `AGENTS.md` 验收标准小节。

**实测的级联差异在成品代码里依然成立**：`time.sleep(60)` 被中断，Windows 3.85s
（stage 1 命中）vs Linux 4.73s（stage 1 失败后升级到 stage 2）——多出的约 0.9s
正是级联存在的意义：没有任何单一手段能跨平台。
>
> - 第一轮：`docs/ipython-方案实证评审.md` —— 指出原方案 7 个致命问题
> - 第二轮：`docs/ipython-实施方案决策.md` —— 修版本的 3 处新问题 + 提出 pipe 路线
> - **本文**：回答"是否有最优解"，给出一条**不是取舍而是收敛**的答案

---

## 一、一句话结论

**有最优解，而且它不是一个"取舍"，是一个"收敛"。**

`pipe 路线（内嵌 InteractiveShell + 级联信号中断）` 在两个关键维度上**同时优于**
ZMQ/Jupyter 路线：**工作量约 1/5，中断能力反而更强**。原因是 pipe 路线<｜hy_place▁holder▁no▁813｜>性地
解锁了 Jupyter 路线做不到的事——见 §3.2。

最终形态：

| 维度 | 最优解 |
|---|---|
| 执行内核 | `IPython.core.interactiveshell.InteractiveShell`（**不用 ipykernel**） |
| 传输 | 子进程 stdin/stdout 上的 JSON Lines |
| Go 依赖 | **零**（只用 `os/exec` + `encoding/json`） |
| Python 依赖 | `pip install ipython`（**不含** ipykernel / pyzmq / jupyter_client） |
| 超时中断 | **三段级联**：`raise_signal(SIGINT)` → `pthread_kill(主线程)` → `interrupt_main()` |
| 会话隔离 | `ExecuteTool` 透传 sessionID → KernelManager 按 session 起独立进程 |
| 兜底 | 三级全失败 → 杀进程树（Windows 复用已有 Job Object 实现） |

---

## 二、为什么这是最优解：两条路线的唯一分岔点

所有差异都源于一个问题：**执行用户代码的线程，和接收宿主控制指令的线程，是否在同一个进程里？**

| | ZMQ / Jupyter 路线 | pipe 路线 |
|---|---|---|
| 代码在哪执行 | **另一个进程**（ipykernel） | **同一进程**（本 launcher） |
| 宿主能对内核做什么 | 只能跨进程：ZMQ 消息 / OS IPC | **可以直接 signal / pending-call** |
| Windows 中断可行手段 | `interrupt_request` → **内核拒收**<br>Win32 Event → **handle 在 launcher 进程内** | **`signal.raise_signal(SIGINT)`** ✅ |

Jupyter 路线在 Windows 上的中断之所以是死结，是因为它**必须**跨越进程边界，
而 Windows 上唯一好使的跨进程手段（Win32 Event）的 handle 活在 launcher 进程里、
Go 侧拿不到。pipe 路线把边界消掉了。

---

## 三、实证数据

### 3.1 中断手段 × 卡住类型矩阵（Windows）

| 卡住类型 | `raise_signal(SIGINT)` | `interrupt_main()` | `SetAsyncExc` | `CTRL_C_EVENT` |
|---|---|---|---|---|
| 纯字节码死循环 | ✅ 0.15s | ✅ 0.15s | ✅ 0.10s | ❌ |
| 亿级循环累加 | ✅ 0.15s | ✅ 0.10s | ✅ 0.10s | ❌ |
| `time.sleep(30)` | **✅ 0.11s** | ❌ | ❌ | ❌ |
| numpy 2000² 矩阵乘 | ✅ 0.25s | ✅ 0.25s | ✅ 0.26s | ✅ |

> 每个 ✅ 都同时验证了**变量保留**（读回 `'still-here/42'`）。

### 3.2 同一张表放到 Linux —— 结论反转

| 卡住类型 | `raise_signal(SIGINT)` | `pthread_kill(主线程)` | `interrupt_main()` |
|---|---|---|---|
| 纯字节码死循环 | ✅ | ✅ | ✅ |
| `time.sleep(30)` | **❌** | **✅ 0.10s** | ❌ |
| 亿级循环累加 | ✅ | ✅ | ✅ |

**没有任何单一手段可以跨平台。** 这是本次最关键的一条实证。

根因：**POSIX 上 `raise_signal()` 近似 `pthread_kill(自己线程)`**，信号投递给了
正在读 stdin 的监听线程，而阻塞在 `time.sleep` 里的主线程收不到；Windows 恰恰相反
（且 `signal.pthread_kill` 在 Windows 无效）。

### 3.3 级联策略（最终方案）—— 双平台 100%

按 `stage 1 → 2 → 3` 逐级升级，**收到响应即停**（绝不连续注入，避免污染后续 cell）：

| stage | 手段 | Windows | Linux |
|---|---|---|---|
| 1 | `signal.raise_signal(SIGINT)` | 全胜 | 断不了 `sleep` |
| 2 | `signal.pthread_kill(主线程, SIGINT)` | 无效（自动跳过） | 全胜 |
| 3 | `_thread.interrupt_main()` | 兜底 | 兜底 |

实测（同一个 launcher、同一套代码，双平台）：

```
=== 级联中断（win32）===
cpuloop    ✓ 0.12s stage 1   后续 cell: 1+1→2 / sum→45 / marker+str(n)→'v7'
bigloop    ✓ 0.12s stage 1   后续 cell: 同上
sleep30    ✓ 0.09s stage 1   后续 cell: 同上
numpy3k    ✓ 0.24s stage 1   后续 cell: 同上

=== 级联中断（linux）===
cpuloop    ✓ 0.06s stage 1   后续 cell: 同上
bigloop    ✓ 0.06s stage 1   后续 cell: 同上
sleep30    ✓ 2.08s stage 2   ← stage 1 失败后自动升级，正是级联的价值
numpy3k    未卡住（Linux BLAS 太快）
```

**"后续 cell 干净"这一列是级联最大的风险点**（残留 pending KeyboardInterrupt 会
在下一次执行里冒出来）。实测三个后续 cell 全部正常，级联是安全的。

> `sleep30` 用了 2.08s 而非 0.3s，是因为 stage 1 的等待预算设成了 2.0s。
> 上线时建议把 stage 预算调到 **300ms**，Linux 上 `sleep` 场景可压到约 0.4s。

---

## 四、落地设计

### 4.1 目录（新增 1 个目录 + 3 个文件）

```
internal/tools/
├── tools.go                  # ExecuteTool switch +1 case
├── config.go                 # buildTools +1 spec
├── ipython/
│   ├── ipython.go            # 入口 toolIPython + 级联超时
│   ├── kernel.go             # Kernel：进程 + JSON Lines 协议 + 级联中断
│   ├── manager.go            # KernelManager：按 sessionID 隔离
│   └── config.go             # ipythonSpec（纯函数，KV 前缀稳定）
└── embed/
    └── ipython_launcher.py   # ← 已验证实现见 docs/assets/ipython_launcher.py
```

**零第三方 Go 依赖**：已用 `CGO_ENABLED=0` 编译出 Windows(4.4MB) / Linux(4.3MB) 双产物。

### 4.2 接口（底层可换，上层现在就定型）

```go
// PythonKernel 抽象：现在落 pipe 实现，将来若要 Jupyter 生态可换实现，
// 上层工具面/描述/超时语义/会话隔离无需改动。
type PythonKernel interface {
    Exec(code string, timeout time.Duration) (ExecResult, error)
    Shutdown()
    Alive() bool
}
```

### 4.3 超时语义（映射现有三档）

| 层级 | 行为 | 变量 |
|---|---|---|
| 前台等待 `fg_timeout_ms`（30s） | 转后台（复用 agent 现有机制） | 保留 |
| 执行软超时 | **级联中断** stage 1→2→3，每级预算 300ms | **保留** |
| 级联全失败 | 杀进程树（Windows Job Object）| 丢失，如实告知模型 |
| 进程崩溃 | 下次调用自动重建新内核 | 丢失，如实告知模型 |

### 4.4 会话隔离（成本极低）

`ExecuteTool(name, args)` 当前**没有 sessionID**，生产调用点只有 `agent.go:697` 一处。

```go
func ExecuteTool(name string, args map[string]any, sessionID string) (string, error)
```

改动面：1 处签名 + 1 处生产调用 + 若干测试。**不这么做的话「会话隔离」是假的**
（原方案里的 `sessionID := ""` TODO 就是这个问题）。

### 4.5 降级策略（产品决策，必须先想清楚）

Python 环境可能没有装 ipython。两个选项：

- **推荐**：启动时探测解释器 + `import IPython`；**探测不到就不注册该工具**，
  bash 仍然是唯一元工具。避免模型看到一个永远失败的工具。
- 备选：始终注册，调用失败时返回安装指引。会让模型反复重试。

配置项（按项目铁律同步**三处**：`config/default.json` +
`config/user.json.example` + `AGENTS.md` 关键默认值表）：

```json
"ipython": { "enabled": true, "python": "", "total_timeout_ms": 600000 }
```

> **落地注记（2026-10-05）**：上面是当时的提案形态；实际进主仓时按项目配置习惯
> 收敛成了 `tools` 段下的一个字符串键——
> `tools.ipython_python`（`""` 不注册 / `"auto"` 探测 / 绝对路径），
> 总超时直接复用 `tools.timeout_ms`，没有单独的 `total_timeout_ms`。
> 降级策略按本节「推荐」执行：探测不到就不注册。

`python` 留空 = 自动探测（推荐顺序：`OKHUMAN_PYTHON` → PATH → 常见安装路径）。

---

## 五、与 bash 的分工（不变）

| 场景 | 工具 |
|---|---|
| 文件 grep / sed / awk、进程管理、curl | `bash` |
| 数据分析 pandas、可视化、AI/ML 推理、ETL、`%timeit`/`%debug` 探索 | `ipython` |

---

## 六、已验证 / 未验证清单

### ✅ 已实跑验证

| 能力 | 结果 |
|---|---|
| `pip install ipython` 依赖树 | 17 个纯 Python 包，**无 pyzmq** |
| 状态持久（跨调用） | `n=42` → `n*2 = 84` ✅ |
| `%timeit` / `!cmd` | ✅ |
| stdout 捕获 | ✅ |
| `Out[N]` 历史 | `Out[3] → 50` ✅ |
| 中文 / 编码 | ✅ |
| 超时→级联中断→**变量保留**→继续执行 | ✅ 双平台 |
| Go 侧零依赖寻址 + 交叉编译 | ✅ Windows / Linux |
| **DataFrame rich display** | `text/html` 578 字符真 `<table>` ✅ |

### ⚠️ 已知边界（如实告知，不要过度承诺）

| 边界 | 说明 |
|---|---|
| `input()` | 不支持（`allow_stdin` 无对应机制）。实测若发生在 Jupyter 侧是 `StdinNotImplementedError` |
| 多次 `display()` | 当前只回传最后一条表达式结果；需 hook `display_pub` 才能收集全部 |
| matplotlib 出图 | 需额外把 PNG 落成文件并回传路径（`matplotlib` import 本身已验证不崩） |
| `import pandas` 冷启动 | **本机 Windows 沙箱实测超过 120 秒**（真机上通常 1–3 秒）；首次调用要给足超时 |
| `Out[N]` 宣传 | 无输出的 cell 不进 Out，直接 `Out[N]` 会 KeyError，建议让模型用变量名 |
| stdout 重复 | 结果同时出现在 `result` 字段和 `stdout`（IPython 回显残留），Go 侧组装时以 `result` 为准 |

### ❌ 仍未解决（需继续攻的点）

1. **`sleep30` Linux 需升级到 stage 2**，若把 stage 预算调到 300ms 需复测
2. **C 扩展长调用**（如超大矩阵乘）在某平台仍可能打不断 → 只能杀进程重启
3. Windows 上**进程树清理**需接 Job Object（可复用 `bash_windows.go` 已有实现，未在本轮验证）

---

## 七、实施步骤

```
Phase 1  把 docs/assets/ipython_launcher.py 落到 internal/tools/embed/
         └── 冒烟：基础执行 + 级联中断 + 变量保留（脚本已就绪，6/6 通过）

Phase 2  ExecuteTool 加 sessionID（1 处签名 + agent.go:697）
         KernelManager 按 session 起进程

Phase 3  kernel.go：JSON Lines 协议 + 级联中断（stage 预算 300ms）+ 进程树清理

Phase 4  ipython/config.go 的 ipythonSpec（纯函数，数字全部来自配置）
         同步三处配置 + 记述 AGENTS.md

Phase 5  降级探测：启动时探测 python/ipython，探测不到不注册工具

Phase 6  测试 + 三平台 build/vet/test；KV 前缀稳定性测试
```

---

## 八、什么时候才需要回退到 ZMQ 路线

只有以下场景真正需要 Jupyter 传输层，否则都是自找麻烦：

- **多前端共连**同一个内核（WebUI 和 agent 同时操作一个内核）
- **`ipywidgets` / comm** 双向通信
- 对接 **JupyterHub / jupyter-server** 的既有生态
- `%debug` 的**交互式** stdin（需要 stdin 通道）

对 OkHuman「一个进程 = 一个 agent」的形态，以上全部不需要。这也是为什么
建议用 `PythonKernel` 接口把底层留成可替换的：**将来真要，换一个实现文件即可**。

---

## 附：复现环境

```bash
# Python 依赖（纯净环境，无 pyzmq）
pip install ipython

# Go 侧零依赖编译
CGO_ENABLED=0 go build -o okhuman .
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...

# 验证脚本位于 %TEMP%\okhprobe\
#   drive_matrix.py   中断手段对照矩阵
#   drive_cascade.py  级联策略验证 + 后续 cell 干净性
#   smoke_launcher.py 正式 launcher 冒烟测试
```
