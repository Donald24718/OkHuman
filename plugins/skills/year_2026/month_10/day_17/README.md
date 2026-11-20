# 2026-10-17 技能索引

- `multi-gpu-torch/` — 本机多卡跑 PyTorch：torch 卡序号≠nvidia-smi、必须按 UUID 暴露单卡、两种 "out of memory" 的判别（有无 Tried to allocate）、V100 sm_70 无 bf16 且 is_bf16_supported() 误报；IndexTTS-2.5 案例只留多卡视角结论（跑法与耗时已移到 tts 插件 references/index-tts.md）
