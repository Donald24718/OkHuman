## 2b. 新两位的生成路线（**当前可用**，MiniMax H3 R2V，2026-11-03 实测）

```
UNETLoader(minimax_h3_ref2va_pruned_fp8_scaled) → SolAttnV100(默认参数) → BasicGuider
CLIPLoader(qwen3-vl-32b-minimax-h3-fp8-comfyui, type="minimax")
VAELoader(minimax_h3_video_vae_fp16)
LoadImage(两张现有母图当 ref) → MiniMaxH3ReferenceToVideo(width=1024,height=1024,length=22,ref_image_size="max")
KSamplerSelect(res_multistep) + BasicScheduler(normal, steps=20, denoise=1.0) + RandomNoise(seed)
→ SamplerCustomAdvanced → VAEDecode → SaveImage → 抽 _00011_.png
```

- 生成脚本在 `$OKHUMAN_DATA_DIR/avatars/gen_avatar2.py`（tmpfs，重启即失；要长期用照上面的节点图重连）
- 实测速度：1024×1024×22 帧挂 SolAttnV100 = **9.2–9.6 s/步**，20 步约 3 分钟/张
- prompt 分段照 comfyui skill 的写法：`subject_definitions` → `summary` → `retention_analysis` → `detailed_description` → `overall_soundscape` → `non_diegetic_music: none`
- `retention_analysis` 里明确写「只保留家族语言（比例/青绿眼/立领制服/开环三竖纹/OK 工牌/👌/纸灯笼/扫描线机房/画风），凡识别到具体某个人的发型与表情一律以 prompt 为准，不要从参考图抄」——这句是让两人**有个性**的关键。
