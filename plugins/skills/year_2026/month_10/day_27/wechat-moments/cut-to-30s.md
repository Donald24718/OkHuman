## 压成 ≤30 秒且要讲完核心思想时

- 时间码从**字幕文件**取（如 `work/subs3.ass` 的 `Dialogue:` 行），按句子边界切，不切词。
- 段间 `xfade=transition=fade:duration=0.3` + `acrossfade=d=0.3`，不硬切；结尾 `fade=out` 0.5s。
- 总时长 = Σ段长 − (段数−1)×0.3；提交前留 1–2 秒余量。
- 交付前用 asr 插件转写整片核对句子完整无截词：
  `Frequently Used Plugins/asr/transcribe.sh <wav>`（先 `start.sh`，探到 HTTP 200 再用）。
