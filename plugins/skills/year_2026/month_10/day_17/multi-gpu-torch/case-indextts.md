## 实测案例：IndexTTS-2.5 语音克隆（2026-10-17 首测，2026-10-25 复测）

完整跑法、接口、耗时与输出规格在 `Rarely Used Plugins/year_2026/month_11/day_02/tts/references/index-tts.md`
（TTS 能力面的入口在那边，别在这里重复维护）。这里只留多卡视角的结论：

```bash
cd <模型盘>/index-tts
CUDA_VISIBLE_DEVICES=GPU-3673d0dd-1b7d-ddef-1f52-1a502f969dea \
  .venv/bin/python 脚本.py     # 按 UUID 暴露单卡，进程里就是 cuda:0
```

- fp32 权重常驻 6.5 GiB（`use_qwen_emo=True` 时 8.2 GiB），16G 卡绰绰有余；加载 21–27 秒；
- 9~14 字句 4~5 秒、28~44 字句 8~12 秒；**CPU 模式一定能跑（`device="cpu"`，2026-10-25 实测慢约 4 倍），卡被占时是保底**；
- 精度：2.5 的类（`indextts.infer_v2_5`）**只有 `use_bf16`，没有 fp16 路径** → V100 上 `use_bf16=False` 走 fp32。
  注意 2.0 的类（`indextts.infer_v2`）参数名是 `use_fp16`，两个类同名 `IndexTTS2`，**别照抄参数名**；
- 用完停进程，GPU2 是留给 ComfyUI 的。
