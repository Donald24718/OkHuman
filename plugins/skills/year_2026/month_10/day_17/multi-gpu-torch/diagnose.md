## 排障顺序（照这个顺序，别跳步）

1. 看报错有没有 `Tried to allocate` → 没有就直接去铁律二（选卡），**不要调任何显存参数**；
2. 有 → 打印真实占用：`torch.cuda.memory_allocated(<正确的 device 序号>)`。**注意不传序号时统计的是 current device（默认 cuda:0），会看到假的 0.00 GiB**（2026-10-17 我自己踩过）；
3. 确认精度：V100 用 fp32/fp16，不用 bf16；
4. 最后才考虑 batch / 分段 / 步数 / `PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True`。

## 变慢先查功耗墙，别当软件退化

- 判据顺序：`nvidia-smi -i <n> --query-gpu=clocks.sm,clocks.max.sm,power.limit,power.draw,utilization.gpu --format=csv`。
  本次 GPU2 `power.limit=150W` 把 `clocks.sm` 压到 810MHz（max 可达 1900+），H3 步速 1.7s/it → 22–36s/it。
- **重启服务无效**（且重启后首条要重载 32B 文本编码器，更慢）。先确认 `power.limit` / 是否被别的进程占功耗，
  再考虑改代码。
