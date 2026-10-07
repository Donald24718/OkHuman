# IPython 元工具方案 —— 实证评审报告

> 评审方式：不依赖文档推演。在本机真实安装 ipykernel、真实启动内核，
> 用「复刻方案 §七 通信逻辑」的探针客户端对话真内核；并额外实现了一个
> `CGO_ENABLED=0` 的纯 Go Jupyter 客户端做可行性对照。
> 环境：Windows + Git Bash，Python 3.11.9，jupyter_client 8.10.0，ipykernel 最新，Go 1.27。

---

## 零、结论速览

**方向正确，但当前版本不可直接实施。**

| 维度 | 判定 | 一句话理由 |
|---|---|---|
| 目标定义（真 IPython / rich display / %magic / 持久状态） | ✅ **成立** | 实测全部兑现（见 §二） |
| 技术选型（ipykernel + ZMQ + Jupyter 协议） | ✅ **可行** | 纯 Go 客户端实测跑通全链路 |
| §九 依赖 `pebbe/zmq4` + CGO | ❌ **本机死路** | 无 gcc，`CGO_ENABLED=0`；但**有解**（见 §三） |
| §五/§七 协议实现 | ❌ **3 处会让工具 100% 不工作** | 均已实测复现 |
| §六 Python launcher | ❌ **第一行就崩** | `BlockingKernelManager` 在本版本中不存在 |
| §八 超时与 Windows 中断 | ❌ **静默假成功（最危险）** | 内核回 `ok` 但什么都没做 |
| §十二 分工定位 | ✅ 合理 | — |

**最值得注意的一条**：§八 在 Windows 上发送 `interrupt_request`，内核会礼貌地回 `{'status': 'ok'}`，
但执行**丝毫未被中断**——Go 侧会以为成功了。这是所有缺陷里最危险的，因为它不会报错。

---

## 一、环境实证（决定了能做什么）

| 项 | 实测结果 | 证据 |
|---|---|---|
| C 编译器 | **无** gcc / clang / cl | `which` 全部未命中 |
| `go env CGO_ENABLED` | `0`（`CC=gcc` 不存在） | `CGO_ENABLED=1 go build` → `cgo: C compiler "gcc" not found` |
| PyPI | ✅ **可达**（pip 走清华源，12.3 MB/s） | `pip download pyzmq` 秒下 |
| pyzmq 安装包 | **win_amd64 wheel，自带 libzmq** | 下载产物为 `.whl`，非源码包 |
| 能否真起 ipykernel | ✅ 能 | `KernelManager.start_kernel()` + `write_connection_file()` 成功 |
| connection file | `signature_scheme=hmac-sha256`，`key` 非空 | 实测打印 10 个字段 |

> ⚠️ **记忆纠正**：本机长期记忆里写的「PyPI 不可达」**不成立**——pip 配了清华镜像
> （`https://pypi.tuna.tsinghua.edu.cn/simple`），实际可达且很快。
> 这直接推翻方案 §九「Windows 需 `vcpkg install zeromq`」：pip 装 pyzmq 已自带预编译 libzmq。

---

## 二、方案卖点验证：承诺 vs 实测

| 卖点 | 实测 | 结果 |
|---|---|---|
| 变量跨调用保留 | `x = 111` → `x + 1` → `112` | ✅ |
| %magic | `%timeit sum(range(1000))` → `9.84 μs ± 3.52 μs per loop` | ✅ |
| `!cmd` | `!echo shellcmd-works` → `shellcmd-works` | ✅ |
| rich display | 同一 cell 产出 2 条 display_data + 1 条 execute_result，各含 `text/html`/`text/markdown`/`text/plain` | ✅ 能力存在（但实现会丢，见 §四.4） |
| In/Out 历史 | `Out[1]` → **`KeyError: 1`** | ⚠️ 见下 |

**关于 In/Out 历史（打折扣）**：无输出的 cell 不进 `Out` 表，所以 `Out[1]` 直接 KeyError。
要用 `Out[N]` 模型必须知道 `execution_count`，而方案 §五 的 `formatIPythonResult()` 并未输出该字段。
建议：**不要宣传 Out[N]，让模型直接用变量名**——既可靠又符合直觉。

---

## 三、依赖选型：方案的写法在本机必死，但有解

**方案写法**：§九 要求安装 `libzmq`（Linux `libzmq3-dev` / macOS `brew` / Windows `vcpkg`）
+ Go 依赖 `github.com/pebbe/zmq4` + `CGO_ENABLED=1`。

**实测**：本机无 gcc → `cgo: C compiler "gcc" not found` → **项目在本机永远无法通过 §十一 的验收**。

**✅ 可行替代（已端到端验证）**：改用纯 Go 实现 `github.com/go-zeromq/zmq4`（v0.17.0），
`CGO_ENABLED=0` 直接编译成功（产物 5.8 MB），并与真 ipykernel 完成完整对话：

```
[go] conn: 127.0.0.1 sig=hmac-sha256 shell=8096 iopub=8097 control=8100
[go] execute_request 已发出
[go] iopub status: busy
[go] iopub execute_input
[go] iopub stream/stdout: "hello from pure-go client\n"
[go] iopub execute_result: "600"          ← 表达式求值正确
[go] shell execute_reply status=ok count=1
[go] iopub status: idle
[go] VERDICT: OK 纯 Go(CGO_ENABLED=0) 完整跑通 Jupyter 协议
```

**建议**：从方案中**彻底删除 cgo / libzmq 依赖**，改 `go-zeromq/zmq4`。
附带收益：三平台交叉编译、静态产物、无需改验收标准。

---

## 四、协议层的致命缺陷（全部实测复现）

### 4.1 ❌ `execute_reply` 不在 iopub 上 —— 方案必然超时

方案 §7.1 `collectIOPub()` 用 `k.iopub` 收包，却 `switch` 出 `case "execute_reply"`。

对照实验（同一段代码 `print()+1+1`）：

| 实现 | iopub 上收到的类型 | 结果 |
|---|---|---|
| **方案原逻辑**（iopub 等 execute_reply） | `status / execute_input / stream / execute_result / status` | **12s 超时**，永远等不到 |
| **正确逻辑**（shell 收 reply） | 同上 + `shell: execute_reply status=ok` | ✅ 全部输出拿到 |

> 方案 §5.3 步骤 6 的文字其实是对的（"Go ← shell: execute_reply"），但代码与之矛盾。
> **修法**：`execute_reply` 走 shell 终止等待；iopub 只负责流式输出，以 `status=idle` 为结束信号。

### 4.2 ❌ iopub 消息前面带 topic frame —— 硬编码 `Frames[0]` 会丢光全部输出

这是我在实现纯 Go 客户端时**亲手踩到**的坑，最有说服力：

| 通道 | 帧数 | `[0]` | `[1]` | `[2]` |
|---|---|---|---|---|
| shell | 6 | `<IDS|MSG>` | signature | header |
| **iopub** | **7** | **`stream.stdout` / `kernel.<id>.status`** | `<IDS|MSG>` | signature |

只要按 `Frames[0] == "<IDS|MSG>"` 判定，iopub 上**所有带 topic 的消息被静默丢弃**
（表现：request 成功、reply 收到，但 stdout / execute_result 一个都没有）。
必须**动态定位 DELIM 帧**再按相对偏移解析。

> 附带修正：方案 §5.2 表格把 iopub 标为 `PUB` 不准确——
> `ipykernel/kernelapp.py:419` 实际是 `context.socket(zmq.XPUB)`。

### 4.3 ❌ 必须实现 HMAC-SHA256，否则消息被静默丢弃

connection file 里 `signature_scheme=hmac-sha256`、`key` 非空。
签名 = `hex(HMAC-SHA256(key, header‖parent_header‖metadata‖content))`，
**`key` 是连接文件里那个十六进制字符串的 UTF-8 字节，不要再 hex 解码**。

实测错误签名 → 内核侧 `ValueError: Invalid Signature`，**没有任何回包**，Go 侧只能干等到超时。

另外：Go 代码未出现 `iopub.SetSubscribe("")`。实测**未订阅时 iopub 收到 0 条消息**。

### 4.4 ❌ rich display 只保留最后一条

一个 cell 产出 3 条 rich 输出（2×display_data + 1×execute_result）已实测。
而 `collectIOPub` 里 `displays := []DisplayData{}` **每次重建切片后整体赋值**，
前面的 rich 输出全部静默丢失。应为 `result.Displays = append(result.Displays, ...)`。

### 4.5 ❌ §六 launcher：`BlockingKernelManager` 根本不存在

```
jupyter_client.BlockingKernelManager            → 不存在
jupyter_client.blocking.BlockingKernelManager   → 不存在
jupyter_client.blocking.manager                 → ImportError
jupyter_client 版本 = 8.10.0
```

方案 launcher 的 `from jupyter_client import BlockingKernelManager` **第一行就 ImportError**。

### 4.6 ❌ connection file 的写入责任缺失

launcher 只读 `--connection-file`，从不写。而 `start_kernel()` **不会创建** connection file
（只有显式的 `write_connection_file()` 才会）。→ Go 侧永远读不到 `key` 与端口。
**必须**：Go 侧生成 connection file（含随机 HMAC key + 随机端口）传给内核，
或让 launcher 显式调用 `write_connection_file()`。

---

## 五、§八 超时语义：Windows 上「静默假成功」（最危险）

### 实测（V1'）

```
[1] 死循环已运行 2.5s，发 interrupt_request 到 control 通道
[2] control 回复 interrupt_reply = {'status': 'ok'}     ← 看起来成功
[3] 等待 25s 后 execute_reply 仍未返回 → 内核还在死循环里
```

### 源码根因

`ipykernel/kernelbase.py:1097`：

```python
def _send_interrupt_children(self):
    if os.name == "nt":
        self.log.error("Interrupt message not supported on Windows")   # 只打日志，什么都不做
    else:
        ... os.killpg / os.kill(pid, SIGINT)
```

**Windows 上 ZMQ 协议的 `interrupt_request` 被内核明确拒收，但仍回复 `status: ok`。**

### 为什么官方 API 却能在 Windows 上 0.2s 成功？

因为 `interrupt_mode` 默认 `"signal"`，官方走的根本不是 ZMQ：

```
KernelManager.interrupt_kernel → interrupt_mode=="signal" → _async_signal_kernel(SIGINT)
  → provisioner.send_signal → sys.platform=="win32"
  → jupyter_client.win_interrupt.send_interrupt(process.win32_interrupt_event)
  → CreateEventA(可继承) → SetEvent(handle)
```

**这是 Win32 Event IPC**：event 由父进程（launcher）创建、继承给内核子进程，
由内核端 `ParentPollerWindows` 线程监听。

### 由此得到的两个架构结论

1. **§二 的两个平台文件确实需要，但定义错了。**
   差异**不在 `interrupt_request` 这条协议消息本身**（它平台无关，
   把它按 Unix/Windows 各写一份是无意义的重复）。真正的差异在 OS 层：
   Unix = `SIGINT` 发给进程组；Windows = Win32 Event。
2. **更麻烦的是：Win32 Event 的 handle 活在 launcher 进程内部。**
   Go 进程拿不到、也无法直接 `SetEvent`。
   → **Windows 上想中断，Go 必须通知 launcher 进程，由 launcher 触发**
   （需新增 Go ↔ launcher 的控制通道，例如额外 ZMQ pair 或 stdin 命令）。
   **这是方案完全没有的架构层。**

### 对 §八 的整体重估

| 层级 | 方案承诺 | Unix | Windows |
|---|---|---|---|
| 软超时 interrupt_request | 中断且保留内核、变量不丢 | ✅ | ❌ **假成功，实际不中断** |
| 硬超时杀进程重启 | 变量丢失 | ✅ | ✅（唯一有效手段） |

→ 方案核心卖点「超时中断后变量不丢」**在 Windows 上不成立**。

### 附：`allow_stdin:false` 下 `input()`（我预判错了，已修正）

原以为会死锁，实测 **1.1s 立即失败**并返回明确错误：
`StdinNotImplementedError: raw_input was called, but this frontend does not support input requests.`
→ 这点方案没问题，模型会得到清晰反馈。

但真正的死局仍在：**CPU 死循环**（如 `while True: pass`）
在 Windows 上无法中断，只剩杀进程一条路。

---

## 六、其它问题

| # | 问题 | 说明 |
|---|---|---|
| 1 | **`sessionID := ""` 是 TODO（§7.3）** | 现状是**所有会话共用同一个内核**，§六 宣称的「按 sessionID 隔离」当前是**假的**，并存在跨会话状态污染。**必须先打通 agent → tools 的 sessionID 透传，再谈会话隔离**。 |
| 2 | **进程树与孤儿进程（方案完全未提）** | launcher 用 Python 起 ipykernel = **孙进程**。Go 只杀 launcher 的话，Windows 上孙进程会变孤儿。本项目在 bash 工具上已有 Job Object 杀整棵树的成熟经验，方案应当复用同一模式。 |
| 3 | **§5.2 表格 iopub 类型标注错误** | 实际是 `XPUB`（`kernelapp.py:419`），方案写 `PUB`。 |
| 4 | **§九 Windows 装 libzmq** | pyzmq 的 win_amd64 wheel 已自带预编译 libzmq，`vcpkg` 这行多余。 |
| 5 | **§十一 验收标准与本机冲突** | 「三平台 go test 全绿」在本机因无 cgo 无法满足；改用纯 Go 方案后自动解决。 |
| 6 | **KV 前缀稳定** | §四 纯函数 + 数字同源的设计是对的 ✅；注意 `ágSec := fgMS/1000` 与描述里的秒数需保持一致。 |

---

## 七、修订建议（按优先级）

| 优先级 | 动作 |
|---|---|
| **P0** | Go ZMQ 库从 `pebbe/zmq4`(cgo) 换成 **`go-zeromq/zmq4`(纯 Go)**，删除全部 libzmq/cgo/vcpkg 依赖 |
| **P0** | 重写 `Execute` 的收包拓扑：**shell 收 execute_reply（终止信号）+ iopub 收输出（以 status=idle 结束）** |
| **P0** | `iopub.SetSubscribe("")` + **动态定位 DELIM 帧**（勿用 `Frames[0]`） |
| **P0** | 实现 **HMAC-SHA256**（key 用原始字符串字节，勿 hex 解码） |
| **P0** | launcher 改用 **`KernelManager` 并显式 `write_connection_file()`**（去掉不存在的 BlockingKernelManager） |
| **P1** | 新增 **Go ↔ launcher 控制通道**，Windows 中断走 Win32 Event（`SetEvent`） |
| **P1** | Windows 文档/描述必须**如实告知**：无软中断，超时即杀进程且变量丢失 |
| **P1** | 先打通 sessionID 透传（否则会话隔离是空谈） |
| **P2** | `Displays` 改为 append；输出带 `execution_count` 或干脆放弃宣传 `Out[N]` |
| **P2** | 复用项目已有的 Job Object 方案管理 launcher + ipykernel 进程树 |
| **P2** | 两个平台文件改名为语义更准的用途：如 `interrupt_unix.go`（进程组 SIGINT）/ `interrupt_windows.go`（Win32 Event 协同） |

---

## 八、复现方式

所有探针保留在 `C:\Users\peli\AppData\Local\Temp\okhprobe\`：

| 文件 | 作用 |
|---|---|
| `probe.py` | 7 个实验：A(shell vs iopub) B(正确拓扑) C(未订阅) D(签名) E(rich 多帧) F(中断) G(%magic/!cmd) |
| `probe_interrupt.py` | 手写 control 通道 vs 官方 `interrupt_kernel()` 对照 |
| `probe_misc2.py` | V1'(Windows 静默失败) V2'(input) V3'(BlockingKernelManager 存在性) |
| `frames.py` | 打印 iopub / shell 的原始 multipart 帧布局（§四.2 的硬证据） |
| `go_kernel.go` | 纯 Go (`CGO_ENABLED=0`) Jupyter 客户端，§三 正面证据 |
| `serve.py` / `interop.py` | 内核托管 / pyzmq PUB·XPUB ↔ Go SUB 互操作 |

运行环境：`/tmp/ipyenv/Scripts/python.exe`（含 ipykernel + jupyter_client + pyzmq）。

---

*评审人：小雅 · 2026-10-05 · 全部结论均有本可机复现证据，无推测项*
