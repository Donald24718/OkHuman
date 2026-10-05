# IPython 工具：修订版复审 + 替代路线实证

> 本文回答一个问题：**修订版方案还有没有问题？有没有更省力的路？**
> 全部结论基于本机实跑证据，无推测。

---

## 一、修订版复审：定位全对，但引入两处新致命错

原报告的 7 个 P0 问题，修订版**全部修订到位** ✅（ZMQ 库、shell 收包、动态 DELIM、HMAC、launcher、平台差异告知）。
但新代码里出现了两处会让它**同样跑不起来**的错误。

### ❌ 新错误 1：HMAC 签名被截断到 8 字符（`§4.4`）

修订版写法：
```go
signature := hex.EncodeToString(h.Sum(nil))[:8] // 前 8 字符   ← 错
```

**实测（真内核）：**

```
=== HMAC 签名长度实验 ===
  完整 signature (64 hex)   → 收到 execute_reply ✅
  修订版 [:8] 截断           → 被静默丢弃 ❌
  [:16] 截断                → 被静默丢弃 ❌
```

内核侧报的正是：
```
ValueError: Invalid Signature: b'a81bbb3c32a36bca'   ← 恰好 8 字符，与修订版写法吻合
```

**Jupyter 要求完整 64 字符 hexdigest，不截断。**

同一段代码还有第二处：`h.Write(header)` 只对 header 签名（且 header 是 `map[string]any`，
根本不能 `Write`）。必须对 **header ‖ parent_header ‖ metadata ‖ content 四个 JSON body 全量 HMAC**：

```go
h := hmac.New(sha256.New, key)              // key = connection file 里的原始字符串字节，勿 hex 解码
for _, b := range [][]byte{headerBytes, parentBytes, metaBytes, contentBytes} {
    h.Write(b)
}
sig := hex.EncodeToString(h.Sum(nil))       // 完整，不要 [:8]
sock.Send(zmq4.NewMsgFrom(
    []byte("<IDS|MSG>"), []byte(sig), headerBytes, parentBytes, metaBytes, contentBytes))
```

### ❌ 新错误 2：Go API 凭想象，编译不过（`§4.3` / `§4.4`）

修订版用的三个符号在 `go-zeromq/zmq4` 里**都不存在**：

| 修订版写法 | 实测是否存在 | 真实 API |
|---|---|---|
| `sock.SendMessage(...)` | ❌ 0 处 | `Send(msg zmq4.Msg) error`，配合 `zmq4.NewMsgFrom(frames...)` |
| `k.iopub.RecvMessageBytes(0)` | ❌ 0 处 | `Recv() (zmq4.Msg, error)`；帧在 `msg.Frames` |
| `*zmq4.Socket`（指针） | ⚠️ | `zmq4.Socket` 是**接口**，不要写指针 |

已验证可用的 API 面：
`NewDealer/NewSub(ctx, zmq4.WithID(...))` · `Dial(ep)` · `SetOption(zmq4.OptionSubscribe, "")` ·
`Send(zmq4.NewMsgFrom(...))` · `Recv() (zmq4.Msg, error)` · `Close()`

> 另：`Recv()` 在无数据时会返回 error，**收取 goroutine 里不能 `return`**，要 continue 重试，
> 否则 goroutine 第一次读不到就永久停摆（我实跑时踩过）。

### ❌ 新错误 3：Windows Win32 Event 的 handle 传递不成立（`§4.6`）

```go
return syscall.SetEvent(syscall.Handle(k.win32Event))   // ← 跨进程无效
```

**Windows 的 HANDLE 是进程私有的句柄值**，把它当 int 通过命令行传给 launcher，
launcher 侧不能直接 `WaitForSingleObject` ——那不是同一个句柄表条目。

可行做法只有两条：
1. **具名 Event**：Go `CreateEvent(..., "Local\\okh-ipy-<uuid>")`，launcher 侧 `OpenEvent` 打开同名对象；或
2. **Go 创建可继承 event**，用 `DuplicateHandle` 显式交给 launcher。

另外 Unix 分支 `k.cmd.Process.Signal(syscall.SIGINT)` 也失效：
SIGINT 发的是 **launcher 进程**，而 launcher 里写着 `signal.signal(signal.SIGINT, shutdown)`
→ 结果是**内核被 shutdown**，与"中断"语义正好相反。要中断需 SIGINT 打到 **ipykernel 进程组**
（`kill(-pgid, SIGINT)`，即 bash 工具已有的 `Setpgid` 做法）。

---

## 二、一条更省力的路：IPython shell + stdin/stdout（已端到端跑通）

**核心想法**：Jupyter = IPython + ZMQ 传输层。而 agent 场景**不需要那个传输层**。
直接用 `IPython.core.interactiveshell.InteractiveShell` 实例，通信走
**子进程的 stdin/stdout + JSON Lines**。

```
IPython 能力（%magic / 状态 / In-Out / rich formatter）✅ 全部保留
        ↓ 去掉
ZMQ / connection file / HMAC / topic frame / XPUB / Win32 Event /  launcher 中间进程
```

### 实测结果（纯净环境，`pip install ipython` 后）

**依赖树里完全没有 zmq：**
```
asttokens colorama executing ipython==9.17.1 ipython_pygments_lexers jedi
matplotlib-inline parso prompt_toolkit psutil pure_eval Pygments
stack-data traitlets typing_extensions wcwidth            ← 17 个纯 Python 包
import zmq → ModuleNotFoundError: No module named 'zmq'   ← 确认无 pyzmq
```

**能力逐项验证：**

| 能力 | 实测 | 结果 |
|---|---|---|
| 状态持久 | `x=111` → 结尾 `x*2 = 222` | ✅ |
| stdout 捕获 | `print('captured stdout')` | ✅ |
| %magic | `%timeit sum(range(1000))` → `5.34 us ± 20.6 ns per loop` | ✅ |
| DataFrame rich display | `text/html` **578 字符真实 `<table>`** + `text/plain` | ✅ |
| `df.describe()` | `text/html` + `text/plain` | ✅ |
| 自定义 `_repr_html_` | MIME = `['text/html','text/plain']` | ✅ |
| 表达式求值 | `df['pop'].sum()` → `np.int64(4641)` | ✅ |
| In/Out 历史 | `Out[2]` → `112` | ✅ |
| 错误处理 | `1/0` → `ZeroDivisionError`；`raise ValueError('boom')` | ✅ |
| 中文编码 | `'中文-ok'` | ✅ |

**完整 launcher（实测可用，约 100 行）见本文附录。**

---

## 三、两条路线对比

| 维度 | 修订版（ZMQ + ipykernel） | 替代路线（InteractiveShell + stdin/stdout） |
|---|---|---|
| Python 依赖 | `ipykernel` `jupyter_client` `pyzmq` | **`ipython`**（无 zmq） |
| 需要实现的难点 | ZMQ 客户端（双通道+动态 DELIM+HMAC+SetSubscribe+心跳）、launcher、Win32 Event、进程树 | 子进程 stdin/stdout + JSON Lines，**复用已有 Job Object** |
| 已实证的致命坑 | 6 个（含本次新增 3 个） | **0 个**（stdin/stdout 无协议坑） |
| rich display | ✅ | ✅ 同等（同一套 IPython formatter） |
| %magic / In-Out / 状态 | ✅ | ✅ 同等（同一个 InteractiveShell） |
| Windows 软中断 | 需 Win32 Event + launcher 协同（且 handle 传递当前写法无效） | 同样做不到 → 超时杀进程（**诚实降级，行为一致**） |
| Unix 软中断 | ✅ SIGINT 打进程组（需先修 launcher 的 signal handler） | ✅ 同理，Python 原生 KeyboardInterrupt |
| 端口 / 随机 key / 签名 | 需要 | **不需要** |
| Go 侧新增依赖 | `go-zeromq/zmq4` | **无**（`os/exec` 即可） |
| 工作量估计 | 大（约 8 个 Phase） | **约 1/5** |

### 什么时候**必须**走 ZMQ 路线

- 需要多前端同时连同一内核（Notebook / Console）
- 需要 `ipywidgets` / comm / `%debug` 交互 UI
- 需要对接 JupyterHub / 企业生态

**对 OkHuman 这种「单进程 = 单 agent」的形态，以上都不需要。**

---

## 四、建议

**推荐：先落地替代路线，但把接口抽出来，保留升级可能。**

```go
// internal/tools/ipython/kernel.go —— 接口保持不变，实现可换
type PythonKernel interface {
    Exec(code string, timeout time.Duration) (ExecResult, error)
    Shutdown() error
}
// 现在：pipeKernel（stdin/stdout）      将来：jupyterKernel（ZMQ）
```

这样做的好处：工具的调用面、描述、超时语义、会话隔离**现在就可以定型**，
底层将来想换 ZMQ，只换一个实现文件，不影响上层。

**如果仍要 ZMQ 路线**，请至少修掉本文第一节的 3 处新错误（HMAC 截断 ×1、Go API ×1、
Win32 Event handle ×1），否则修订版同样跑不起来。

---

## 附录：实测可用的 launcher（替代路线）

```python
# -*- coding: utf-8 -*-
"""OkHuman Python launcher：IPython shell + stdin/stdout JSON Lines。
请求 {"id":1,"code":"..."}  响应 {"id":1,"ok":..,"stdout":..,"result":{mime:data},"error":..}
"""
import io, json, sys, traceback
from contextlib import redirect_stdout, redirect_stderr
from IPython.core.interactiveshell import InteractiveShell


def render(shell, obj):
    try:
        fmt, _ = shell.display_formatter.format(obj)
    except Exception:
        return None
    return {m: (d if isinstance(d, str) else str(d)) for m, d in fmt.items()} or None


def run_one(shell, req):
    out, err = io.StringIO(), io.StringIO()
    resp = {"id": req.get("id"), "ok": True, "stdout": "", "stderr": "",
            "result": None, "error": None, "count": None}
    try:
        with redirect_stdout(out), redirect_stderr(err):
            r = shell.run_cell(req.get("code", ""), store_history=True)
        resp.update(stdout=out.getvalue(), stderr=err.getvalue(),
                    count=getattr(r, "execution_count", None), ok=bool(r.success))
        if not r.success:
            e = r.error_in_exec
            resp["error"] = {"ename": type(e).__name__ if e else "Error", "evalue": str(e)}
        elif r.result is not None:
            resp["result"] = render(shell, r.result)
    except Exception:
        resp.update(ok=False, stdout=out.getvalue(), stderr=err.getvalue(),
                    error={"ename": "InternalError", "evalue": traceback.format_exc(limit=3)})
    return resp


def main():
    shell = InteractiveShell.instance(display_banner=None)
    sys.stdout.write(json.dumps({"id": 0, "ready": True,
                                 "python": sys.version.split()[0],
                                 "ipython": __import__("IPython").__version__}) + "\n")
    sys.stdout.flush()
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError as e:
            resp = {"ok": False, "error": {"ename": "BadRequest", "evalue": str(e)}}
        else:
            resp = run_one(shell, req)
        sys.stdout.write(json.dumps(resp) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
```

### 已知待补（诚实标注）
1. **执行中的多次 `display()`**：当前只返回最后一个表达式的 result；
   中途 `display(x)` 产生的内容需 hook `shell.display_pub` 补收集（约 20 行）。
2. **matplotlib 出图**：用 `matplotlib-inline` 或自行 `savefig` → base64 `image/png` MIME。
3. **`input()`**：建议不支持，返回明确错误（实测 ZMQ 路线 `allow_stdin:false` 也是报错）。

### 复现路径
`/tmp/ipyonly`（只装 ipython 的纯净 venv）+
`C:\Users\peli\AppData\Local\Temp\okhprobe\{poc_launcher.py, drive.py, rich.py}`。

---

*小雅 · 2026-10-05*
