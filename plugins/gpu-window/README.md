# gpu-window —— 让出/换回某个服务占的显存（把一张卡当分时槽位）

## 它解决什么

两张 V100 各被推理后端占满（各 ~14 GB），要出图（ComfyUI :8188）或跑别的 CUDA 活就挤不下。
停进程再冷启动要 13–40 秒且丢会话 KV。本插件用驱动的**进程级 checkpoint** 把某个服务占的显存整块搬到内存，
卡空出来给别人用，用完搬回来：**进程不死、权重与 KV 都不丢**。一张卡因此可以挂多个服务，轮流持有——**分时独占，不是同时共存**。

## 唯一的 pid 来源：显存归属

`map` 列出每张卡上「哪个 pid 占多少、对应 config 里哪个服务」。**不要拿 unit 的 MainPID 去切**：
多进程服务（本机 8080 = python 主进程 + `build-ab/strata` + `strata-vision`）打在 MainPID 上会**"成功但无效"**——状态变成 CHECKPOINTED 而显纹不动。

## 用法

```bash
/usr/local/go/bin/go build -o gpu-window .     # 只依赖 libcuda，不需要 CUDA 头文件
./gpu-window map                                # 每张卡的显存归属 + 哪个是登记的服务（pid 从这里取）
./gpu-window status <服务名|pid>
./gpu-window yield  <服务名|pid> [--force]      # 换出到内存 = 让卡
./gpu-window resume <服务名|pid>                # 换回来
./gpu-window window <服务名> --for 120 | --cmd '…出图命令…'   # 让卡 → 做事 → 无论成败都换回
```
退出码：`2`=闸门拦下（有实例正在 run）｜`3`=该服务标了 protected｜`1`=其它错误。

## 配置（`config.json`，与可执行文件同目录，权限 600）

`services[]` = `{name, match, protected, gates[]}`；`match` 对 `/proc/<pid>/cmdline` 做子串匹配（写一个或数组，多进程服务就把每个持有显存的进程都列上）。

- 服务名常常就是端口号。解析**先查服务名、查不到才当 pid**——先 `Atoi` 会让 protected 和 gates 全部失效（第一版就栽在这）。
- `protected: true` → 默认不许让卡（本机 8080 就是这么标的：「不要动 8080」是**配置**，代码里没有硬编码）。
- `gates[]` = `{url, field}`（field 默认 `running`）：推理后端自己没有"正在跑请求"的端点，就用它的**客户实例** `/status` 当闸门（8081→8452、8080→8451）。闸门探不到时不拦（那台没起属正常）。

## 两机对照（2026-11-08 实测）

`.2` 与 `.3` 驱动同为 **580.178.04**，`/usr/lib/x86_64-linux-gnu/libcuda.so.1` 都导出 `cuCheckpointProcess{GetState,Lock,Checkpoint,Restore,Unlock}` → 两边都能用、都能 `go build`（`.3` 有 gcc 与 go）。
`.3` 上 strata 同样是多进程（`build-ab/strata` ~12.3 GB + `build-vision-cuda/bin/strata-vision`）。**config 各写一份，不互抄**（进程名与实例端口都不同）。
本插件只管**本机看到的 GPU**；远端那张卡要 ssh 到那台跑同一个二进制（两台的单元与路径对照见 `host-crash-autostart/services.md`）。

## 边界（不粉饰）

- **只在服务空闲时切**。被换出的进程 CPU 侧照常活（读文件、打日志都行），但任何触碰显存的调用会阻塞 → 所以 `/v1/models` 仍返回 200、真生成请求挂到超时。别拿探活当可用判据。**被切出去的服务要是直接崩了（2026-11-08 实测：8081 在让卡窗口里收到一条真请求 → 段错误 139），就是有请求打进来了**——`gates` 只看提交那一刻，防不了之后来的；用完显卡把那个服务重新起一遍就好。
- **换回要求那张卡仍够它用** → 窗口里别让别的进程把整卡吃满。
- 换出的量要有 host 内存接得住（`.2` 约 136 GB 可用）。
- 成本与体量成正比（14 GB 级约 出 6 s / 回 3 s）；**实测数只写在 `host-crash-autostart/services.md` 的 8081 那条**，此处不重复。
- **不保证任何进程都兼容**（CUDA 对 UVM、GPUDirect 这类有限制）→ 新接入的服务先 `status`，再试一次 `yield`。

## 排障

| 现象 | 原因 / 办法 |
|---|---|
| `map` 什么都不输出 | nvidia-smi 的查询参数写错了：要 `--query-gpu=…` / `--query-compute-apps=…` |
| `getstate` 报 "OS call failed / not supported" | 传的 pid 不存在（多半是服务名被当成 pid 解析），或该进程本来不持有显存 |
| `yield` 显示成功但显存没变 | 挑错了 pid——打在 MainPID 上。用 `map` 取真正持有显存的那个 |
| `status` 显示 2 | = CHECKPOINTED，是**已成功让卡**，不是错误（枚举 0 RUNNING/1 LOCKED/2 CHECKPOINTED/3 FAILED） |
| `yield` 卡在 lock 不动 | 该进程正在跑请求（API 会等它静默）→ 先看它的 gates |

## 测试

`go vet ./...`、`go test ./...`（5 个，不依赖 GPU）：`match` 的字符串/数组两种写法、两段 nvidia-smi CSV 解析、**只挑出持有显存的 pid**（MainPID 陷阱）、闸门的 `running` 判定、state 枚举映射。
