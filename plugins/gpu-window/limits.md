# gpu-window — 边界与排障

## 边界（不粉饰）

- **只在服务空闲时切**。被换出的进程 CPU 侧照常活（读文件、打日志都行），但任何触碰显存的调用会阻塞 → 所以 `/v1/models` 仍返回 200、真生成请求挂到超时。**别拿探活当可用判据。**被切出去的服务要是直接崩了（2026-11-08 实测：8081 在让卡窗口里收到一条真请求 → 段错误 139），就是有请求打进来了——`gates` 只看提交那一刻，防不了之后来的；用完显卡把那个服务重新起一遍就好。
- **换回要求那张卡仍够它用** → 窗口里别让别的进程把整卡吃满。
- 换出的量要有 host 内存接得住（`.2` 约 136 GB 可用）。
- 成本与体量成正比（14 GB 级约 出 6 s / 回 3 s）；**实测数只写在 `host-crash-autostart/services.md` 的 8081 那条**，此处不重复。
- **不保证任何进程都兼容**（CUDA 对 UVM、GPUDirect 这类有限制）→ 新接入的服务先 `status`，再试一次 `yield`。**本机 8081(`kvmem-ninfer`) 实测（2026-11-14）：`yield` 成功（6 s）、`resume` 必失败——`restore: OS call failed or operation not supported`，卡完全空着也一样** → 它的「换回」只能靠重启它的 systemd 单元（`systemctl --user restart ninfer-8081`，装载约 1 分钟）。给它让卡之前先接受这个代价：那段时间它是**真不可用**（`/v1/models` 仍 200，生成请求挂到超时）。

## 排障

| 现象 | 原因 / 办法 |
|---|---|
| `map` 什么都不输出 | nvidia-smi 的查询参数写错了：要 `--query-gpu=…` / `--query-compute-apps=…` |
| `getstate`/`restore` 报 "OS call failed / not supported" | 两种原因：①传的 pid 不存在或服务名被当 pid 解析；②该进程**不支持驱动 checkpoint 还原**（见上面 8081 那条）——`status` 显示 CHECKPOINTED 而卡是空的，就是②，别再重试 `resume` |
| `status` 显示 2 | = CHECKPOINTED，是**已成功让卡**，不是错误（枚举 0 RUNNING/1 LOCKED/2 CHECKPOINTED/3 FAILED） |
| `yield` 卡在 lock 不动 | 该进程正在跑请求（API 会等它静默）→ 先看它的 gates |
