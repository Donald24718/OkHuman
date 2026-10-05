# 变更记录

本文件按时间倒序记录有外部可见影响的变更。新增/移除配置项、性能或安全相关变更必须登记（见 `AGENTS.md` 文档维护表）。

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
