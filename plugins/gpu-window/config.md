# gpu-window — 配置（config.json，与可执行文件同目录，权限 600）

`services[]` = `{name, match, protected, gates[]}`；`match` 对 `/proc/<pid>/cmdline` 做子串匹配（写一个或数组，多进程服务就把每个持有显存的进程都列上）。

- 服务名常常就是端口号。解析**先查服务名、查不到才当 pid**——先 `Atoi` 会让 protected 和 gates 全部失效（第一版就栽在这）。
- `protected: true` → 默认不许让卡（本机 8080 就是这么标的：「不要动 8080」是**配置**，代码里没有硬编码）。
- `gates[]` = `{url, field}`（field 默认 `running`）：推理后端自己没有"正在跑请求"的端点，就用它的**客户实例** `/status` 当闸门（8081→8452、8080→8451）。闸门探不到时不拦（那台没起属正常）。
- `match` 是**子串**：`comfyui` 那条写的是 `--listen 127.0.0.1`，所以用 `--listen 127.0.1.1` 起的 ComfyUI 在 `map` 里显示 `[未登记]`（2026-11-14）——不是没匹配上就没事，只能按 pid 操作，且 `window <服务名>` 用不了。

## 两机对照（2026-11-08 实测）

`.2` 与 `.3` 驱动同为 **580.178.04**，`/usr/lib/x86_64-linux-gnu/libcuda.so.1` 都导出 `cuCheckpointProcess{GetState,Lock,Checkpoint,Restore,Unlock}` → 两边都能用、都能 `go build`（`.3` 有 gcc 与 go）。`.3` 上 strata 同样是多进程。**config 各写一份，不互抄**（进程名与实例端口都不同）；本插件只管**本机看到的 GPU**，远端那张卡要 ssh 过去跑同一个二进制。
