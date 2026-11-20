本机 4 张卡（2026-10-17 实测）：

| nvidia-smi | 型号 | UUID | 常态占用 |
|---|---|---|---|
| 0 | GTX 750 Ti (sm_50) | `GPU-8ac0950f-8ebb-030f-b27d-7512fed688f3` | ~370 MiB（桌面） |
| 1 | V100-SXM2-16GB (sm_70) | `GPU-62782369-84cd-18f3-0a66-cc6922fffe27` | ~15.9 GiB（8080 strata / 小A 的 LLM） |
| 2 | V100-SXM2-16GB (sm_70) | `GPU-3673d0dd-1b7d-ddef-1f52-1a502f969dea` | 空闲（留给 ComfyUI） |
| 3 | V100-SXM2-16GB (sm_70) | `GPU-037e3de0-1489-1327-2e2e-b4069d4454b0` | ~12.7 GiB（8081 ninfer / 小B） |

