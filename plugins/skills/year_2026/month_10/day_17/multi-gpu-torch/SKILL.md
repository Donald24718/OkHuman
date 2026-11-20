# 多卡上跑 PyTorch：选卡与显存排障（母文档只做路由）

服务常驻在哪张卡、临时让卡 → 条目 `host-crash-autostart` 的 `services.md` 与 `gpu-window` 插件；这里只管自己起的 CUDA 进程。

## 要做什么 → 读哪份

| 要做什么 | 文件 |
|---|---|
| 四条铁律：卡序号不等价、按 UUID 只暴露一张、两种 out of memory 怎么分、旧架构没有 bf16 还会被误报 | `rules.md` |
| 排障顺序（照顺序来，别跳步）、跑着跑着变慢先查什么 | `diagnose.md` |
| 一个完整实测案例：零样本音色克隆放多卡上怎么跑 | `case-indextts.md` |
| 本机环境事实（卡与 torch 版本） | `env.md` |
| 另一台机器上的差异 | `peer.md` |

## 遇到什么现象 → 读哪份

| 现象 | 文件 |
|---|---|
| 指名某张卡，实际跑到另一张 | `rules.md` |
| 报 out of memory，但显存看着还够 / 刚加载就装不下 | `rules.md` |
| 问低精度支不支持，框架报的和实测不符 | `rules.md` |
| 不报错但明显变慢、算力上不去 | `diagnose.md` |
| kernel launch、cuda error 之类看不懂的报错 | `diagnose.md` |
