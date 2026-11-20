## 验收判据（三样，缺一不算完）

- **ASR 逐句**：按 `edl.narr` 的 `t0-0.35 ~ t0+dur+0.45` 切窗，逐句送 asr 插件（:8090）。
  返回带前缀 `language Chinese<asr_text>`，比对前要剥掉；标点差异忽略。
  **同音词不算错**（本系列「扬弃」必被听成「洋气」，IndexTTS2 读的是字面）。
- **响度**：`ffmpeg -af ebur128` 看 `Integrated I:`，目标 −14 LUFS（两遍 loudnorm 才准，单遍会 undershoot）。
- **画面**：抽 15 个时间点拼一张图，核对封面卡首帧、两张概念卡出现时机、片尾卡文案、字幕不压主体。
