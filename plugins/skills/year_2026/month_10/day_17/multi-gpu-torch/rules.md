## 铁律一：torch 的卡序号 ≠ nvidia-smi 的序号

2026-10-17 实测映射（torch 2.8.0+cu128）：

```
torch cuda:0 = nvidia GPU1 (62782369…)   ← strata 那张
torch cuda:1 = nvidia GPU2 (3673d0dd…)   ← 空闲那张
torch cuda:2 = nvidia GPU3 (037e3de0…)   ← ninfer 那张
torch 把 750 Ti 排到末位，且 sm_50 不被这个 build 支持
```

**照 nvidia-smi 的编号写 `device='cuda:2'` 会打到完全不同的卡上。** 别猜，跑这段拿映射：

```python
import torch
for i in range(torch.cuda.device_count()):
    p = torch.cuda.get_device_properties(i)
    print(i, p.name, 'sm_%d%d' % (p.major, p.minor), p.uuid)
```

## 铁律二：用 UUID 只暴露一张卡，脚本里一律写 `cuda:0`

这是唯一可靠的写法（2026-10-17 实测，一次就通）：

```bash
CUDA_VISIBLE_DEVICES=GPU-3673d0dd-1b7d-ddef-1f52-1a502f969dea \
  <venv>/bin/python 你的脚本.py     # 脚本里 device='cuda:0'
```

`CUDA_VISIBLE_DEVICES=2`（按 nvidia 序号）**在本机不可靠**——实测会出现"设了 2 但 torch 看到的还是别的卡"。用 `GPU-<uuid>` 形式则完全确定。

**为什么必须只暴露一张**：进程若给全部 4 张卡建 CUDA context，那张 sm_50 的 750 Ti 会被这个 torch build 拒绝，结果是 **kernel launch 失败，但 PyTorch 把它报成 `CUDA error: out of memory`**。显存其实一点没满。

## 铁律三：分清两种 "out of memory"（最有价值的一条）

| 报错形态 | 真因 | 对策 |
|---|---|---|
| `torch.OutOfMemoryError` + **`Tried to allocate N MiB`** | 真·显存不足 | 降精度/降 batch/分段/`empty_cache` |
| `torch.AcceleratorError: CUDA error: out of memory`，**没有 `Tried to allocate`** | **不是显存问题**，是 kernel launch 失败被误译（多卡 context / 架构不支持） | 按 UUID 只暴露一张目标卡 |

判别只需一眼：**有没有 `Tried to allocate` 这句话**。没有就别去调显存参数，先修选卡。

## 铁律四：V100 是 sm_70，没有 bf16；而且 torch 会误报

- Volta 只原生支持 FP16 / FP32 / INT8，**没有 BF16 单元**（要 Ampere sm_80 起）。
- **`torch.cuda.is_bf16_supported()` 在 V100 上会返回 `True`（误报）**——它查驱动能力不看架构。2026-10-17 实测：多卡暴露时返回 False，按 UUID 单卡暴露后返回 **True**，但实际 `torch.amp.autocast(..., dtype=bfloat16)` 仍抛 `RuntimeError: Current CUDA Device does not support bfloat16`。
- 结论：**这台机器上跑 torch 一律 fp32 或 fp16，别信 `is_bf16_supported()`。** 实测 fp32 还比 bf16 快（与 index-tts issue #785 的 V100 回执一致：bf16 11分40秒 vs fp32 10分13秒）。
