# -*- coding: utf-8 -*-
"""
OkHuman IPython launcher（pipe 路线，已双平台实测）。

为什么不走 Jupyter/ZMQ：jupyter_client 在 Windows 上用 |Win32 Event| 做中断，
该 handle 活在 launcher 进程里，宿主拿不到；而 ipykernel 自身在 Windows 上
直接拒收 ZMQ interrupt_request（kernelbase.py: "Interrupt message not supported
on Windows"）。pipe 路线把"执行"与"接收控制指令"放进同一个 Python 进程，
于是可以直接用 signal 原语自中断 —— 见_interrupt_stage。

协议（stdin / stdout，均为 UTF-8 JSON Lines）：
  请求 {"id": N, "code": "..."}
  控制 {"cmd": "interrupt", "stage": k}   k=1,2,3 逐级升级
  控制 {"cmd": "shutdown"}
响应：
  {"id": N, "ok": bool, "stdout":..., "stderr":..., "result": {mime: data},
   "displays": [{mime: data}, ...],
   "error": {"ename":..., "evalue":...}, "count": N, "interrupted": bool}

displays 是 display() 主动推送的内容（区别于 result——cell 最后一个表达式的
求值结果）。二者必须分开：display() 可以在一个 cell 里调多次，而 result 至多一个。

宿主侧判定中断的铁律：IPython 的 run_cell 会**内部消化** KeyboardInterrupt，
把它变成 success=false + error_in_exec，所以 `interrupted` 字段可能保持 false。
必须同时认 `error.ename == "KeyboardInterrupt"`，否则超时会被误判成用户代码报错。

依赖：pip install ipython（**不需要** ipykernel / pyzmq / jupyter_client）
"""
import builtins
import io
import json
import queue
import signal
import sys
import threading
import traceback
from contextlib import redirect_stdout, redirect_stderr

from IPython.core.interactiveshell import InteractiveShell

_exec_q = queue.Queue()
_main_tid = [None]
# 当前 cell 的 display() 收集器；非执行期间为 None（收集只在 run_cell 内有效）。
_display_sink = [None]


class InputUnavailable(RuntimeError):
    """用户代码试图读取交互输入。宿主没有可交互的 stdin，只能快速失败。"""


# ---------------------------------------------------------------- 中断

def _interrupt_stage(stage):
    """级联中断的第 stage 级 —— 由宿主按"收到响应即停"的方式逐级升级。

    实测（详见 docs/ipython-最优解-级联中断实证.md）：
      stage 1  raise_signal(SIGINT)       Win: 全胜     Linux: 打不断 time.sleep
      stage 2  pthread_kill(主线程, SIGINT) Win: 无效    Linux: 全胜
      stage 3  interrupt_main()           两者都能打断字节码循环（兜底）

    之所以必须级联而非二选一：POSIX 上 raise_signal 近似 pthread_kill(自己)，
    投递给的是监听线程而非阻塞中的主线程；Windows 则相反且不支持 pthread_kill。
    """
    if stage == 1:
        signal.raise_signal(signal.SIGINT)
    elif stage == 2:
        signal.pthread_kill(_main_tid[0], signal.SIGINT)
    else:
        import _thread
        _thread.interrupt_main()


# ---------------------------------------------------------------- 输出

def _capture_display(shell):
    """把 display() 的内容收进协议，而不是让它掉进哑 publisher。

    实测（IPython 9.17.1）：
      - DisplayPublisher 只有 publish(data=..., metadata=...)，**没有**
        publish_display_data（旧文档/示例里那个名字已不存在），且调用是纯 kwargs；
      - 默认实现的兜底是把 repr 打到 stdout，于是模型只看到
        `<IPython.core.display.HTML object>`——富内容完全丢失；
      - 普通 cell 结果（最后一个表达式）**不会**走 publish，所以这里捕获到的
        一定是 display() 主动推送的，不会与 result 重复。
    """
    pub = shell.display_pub

    def _publish(data=None, metadata=None, *a, **k):
        sink = _display_sink[0]
        if sink is None or not isinstance(data, dict) or not data:
            return None  # 返回即吞掉默认实现，避免 repr 噪声混进 stdout
        out = {}
        for mime, val in data.items():
            try:
                out[str(mime)] = val if isinstance(val, str) else str(val)
            except Exception:
                continue
        if out:
            sink.append(out)
        return None

    pub.publish = _publish


def _enable_inline_graphics(shell):
    """让 matplotlib 出图可用，且**不必写 `%matplotlib inline`**。

    三件实测出来的事，缺一件就出不了图：

    1. `%matplotlib inline` 在这类非终端 shell 里会走到 InteractiveShell.enable_gui
       抛 NotImplementedError("Implement enable_gui in a subclass")——魔术以失败
       告终，用户只看到一个莫名其妙的报错。所以不靠它。
    2. 只调 configure_inline_support 只登记了 formatter，**没有切后端**：实测
       Figure 只有 text/plain、`_repr_png_` 为 False。
    3. 只切后端到 Agg（无头）也不够：`plt.show()` 是模型最常写的收尾方式，
       而 Agg 的 show() 只给一句 "FigureCanvasAgg is non-interactive, and thus
       cannot be shown"，图根本不会出来（实测端到端踩到）。

    所以两件事都要做：后端切成 inline（无头、内部用 Agg 渲染），再登记
    formatter。之后 `plt.show()` 与 `fig` 作末表达式都能产出 image/png。
    """
    try:
        import matplotlib
        import matplotlib_inline.backend_inline as _inline
        # 必须在用户 import pyplot 之前调用（启动时这里是唯一正确的时机）。
        matplotlib.use("module://matplotlib_inline.backend_inline", force=True)
        _inline.configure_inline_support(shell, "module://matplotlib_inline.backend_inline")
    except Exception:
        pass  # 没装 matplotlib / matplotlib-inline 就当没有这项能力


def _render(shell, obj):
    """借 IPython formatter 做 rich display；返回 MIME -> data。"""
    try:
        fmt, _ = shell.display_formatter.format(obj)
    except Exception:
        return None
    if not isinstance(fmt, dict) or not fmt:
        return None
    out = {}
    for mime, data in fmt.items():
        try:
            out[mime] = data if isinstance(data, str) else str(data)
        except Exception:
            continue
    return out or None


def _run_one(shell, req):
    code = req.get("code", "")
    buf_out, buf_err = io.StringIO(), io.StringIO()
    resp = {"id": req.get("id"), "ok": True, "stdout": "", "stderr": "",
            "result": None, "displays": [], "error": None, "count": None,
            "interrupted": False}
    _display_sink[0] = resp["displays"]
    try:
        with redirect_stdout(buf_out), redirect_stderr(buf_err):
            r = shell.run_cell(code, store_history=True)
        resp["stdout"] = buf_out.getvalue()
        resp["stderr"] = buf_err.getvalue()
        resp["count"] = getattr(r, "execution_count", None)
        resp["ok"] = bool(r.success)
        if not r.success:
            err = r.error_in_exec
            resp["error"] = {"ename": type(err).__name__ if err else "Error",
                             "evalue": str(err) if err else "execution failed"}
        elif r.result is not None:
            resp["result"] = _render(shell, r.result)
    except BaseException as e:
        resp["ok"] = False
        resp["interrupted"] = isinstance(e, KeyboardInterrupt)
        resp["error"] = {"ename": type(e).__name__, "evalue": str(e)}
        resp["stdout"] = buf_out.getvalue()
        resp["stderr"] = buf_err.getvalue()
    finally:
        # 出错也要保留已 display 的内容：cell 前半段画了图、后半段抛异常时，
        # 那些图是模型唯一的线索。
        _display_sink[0] = None
    return resp


# ---------------------------------------------------------------- 主循环

def _stdin_reader(stop):
    """后台线程：执行请求入队，控制指令就地处理。"""
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(req, dict):
            continue
        if "cmd" in req:
            cmd = req.get("cmd")
            if cmd == "interrupt":
                stage = req.get("stage", 1)
                if isinstance(stage, int) and 1 <= stage <= 3:
                    try:
                        _interrupt_stage(stage)
                    except Exception as e:  # 某平台不支持该手段时降级到下一级
                        sys.stderr.write("stage %d failed: %r\n" % (stage, e))
                        sys.stderr.flush()
            elif cmd == "shutdown":
                stop.set()
                return
        else:
            _exec_q.put(req)


def main():
    shell = InteractiveShell.instance(display_banner=None)

    # 抑制 REPL 回显：结果我们已经通过 JSON 的 result 字段回传，前台再打印一遍
    # 纯属重复、白白占用 token，还会与用户自己的 print 混在一起。
    #
    # ⚠️ 只能做**实例级** patch，绝不能替换 shell.displayhook 对象本身：
    # ExecutionResult.result 是由 hook.__call__ 经 self.exec_result 回填的
    # （IPython/core/displayhook.py:244 fill_exec_result）。换一个新 hook 实例
    # 会把 exec_result 的绑定丢掉，导致 run_cell 返回的 r.result 恒为 None（实测）。
    # 只禁用回显不影响 Out[N] 历史（In/Out 仍是 IPython 的能力）。
    hook = shell.displayhook
    hook.write = lambda s: None
    hook.write_output_prompt = lambda *a, **k: None
    hook.write_result_prompt = lambda *a, **k: None

    # 交互输入不可用：stdin 是宿主协议通道，用户代码里的 input() 永远等不到一行
    # （实测 6s 无响应，最后只能靠宿主超时 + 级联中断救回来，白耗一轮）。
    # 与其挂死，不如立刻失败并把替代方案写进异常消息——模型看到就知道该改写法。
    # 顺带覆盖 pdb / %debug / getpass，它们底层同样读 stdin。
    def _no_input(*a, **k):
        raise InputUnavailable(
            "本工具没有交互输入：stdin 已被宿主协议占用。"
            "请把值直接写进代码（如 x = 42），不要 input()。"
            "需要人输入时请在你的回复里向用户提问。")

    builtins.input = _no_input
    try:
        import getpass
        getpass.getpass = lambda *a, **k: _no_input()
    except Exception:
        pass

    _capture_display(shell)
    _enable_inline_graphics(shell)

    _main_tid[0] = threading.get_ident()

    stop = threading.Event()
    threading.Thread(target=_stdin_reader, args=(stop,), daemon=True).start()

    say = lambda obj: (sys.stdout.write(json.dumps(obj) + "\n"), sys.stdout.flush())
    say({"id": 0, "ready": True,
         "python": sys.version.split()[0],
         "ipython": __import__("IPython").__version__})

    while not stop.is_set():
        try:
            req = _exec_q.get(timeout=0.3)
        except queue.Empty:
            continue
        say(_run_one(shell, req))


if __name__ == "__main__":
    main()
