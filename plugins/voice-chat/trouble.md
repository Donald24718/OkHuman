## 排障

- **日志**：`/tmp/voice_loop.log`（主循环）`/tmp/speaker_gate.log` `/tmp/voice_loop_asr.log` `/tmp/voice_loop_tts_kokoro.log`
- **TTS 没声音 / 报 "tts server dead"**：看 `/tmp/voice_loop_tts_kokoro.log`。serve 起不来时
  import 阶段的崩溃 traceback 会落在这个文件（例：phonemizer 版本 API 差异、espeak 数据缺失）。
  正常日志形如 `[kokoro] wrote out_NNNN.wav (X.Xs 音频, 合成用时 X.Xs, RTF=X.XX)`。
- **总被声纹拦**：调低 `gate_threshold`（0.5 起试），或补 enroll 几段安静环境的录音。
- **唤醒词老不命中**：看 `/tmp/voice_loop.log` 里 `[wake] 未含唤醒词` 的原文——ASR 把唤醒词识别成什么了；换成 ASR 能稳定转写的词（“小A”/“小B” 实测稳；“小欧” 会被听成 “小小”）。
- **第一次用先看门控分数**：`/tmp/voice_loop.log` 里 `[gate] 本人声音 (score=...)`——score 应 ≥ 0.6；若你自己的声音都过不了，重新 enroll 一段安静环境的录音或调低阈值。
- **别人也能驱动**：调高 `gate_threshold`（0.7+）。
- **ASR 慢**：CPU 跑 0.6B 模型，单句通常 2~10s；机器满载（两个 27B 服务在跑）时会更慢。
- **说话没反应**：看 `/tmp/voice_loop.log` 有没有 `[mic] 检测到语音`——没有就是 VAD
  阈值问题（环境噪声大时底噪估计会自适应，安静房间试一次冷启动）。
