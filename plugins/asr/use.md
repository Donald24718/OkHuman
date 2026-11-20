# asr — 怎么调、拿到的是什么

## 接口

```bash
curl -s http://127.0.0.1:8090/v1/audio/transcriptions \
  -F "file=@speech.wav" -F "model=qwen3-asr-0.6b" -F "language=Chinese"
# -> {"type":"transcript.text.done","text":"language Chinese<asr_text>识别结果","usage":{...}}
```

**取文本要剥两层前缀**：`language Chinese` 与 `<asr_text>`（2026-10-28 实测响应里两段都在；`transcribe.sh` 已处理，自己拼 curl 时别只剥 `<asr_text>`）。响应还带 `type` 字段，别按整段 JSON 当文本用。

## 一律走脚本，别自己拼 multipart

`./transcribe.sh <绝对路径 wav>` → stdout 出纯文本。**必须传绝对路径**：脚本第一行 `cd "$(dirname "$0")"`，传相对路径时 curl 取不到文件、脚本**静默返回空**（不报错），容易被误判成"模型没识别出来"（2026-10-27）。自己拼 HTTP multipart 易错在 boundary/字段名且失败同样静默。整片 90 秒一次转写没问题（16 kHz 单声道 wav）。

## 实测耗时（本机 2026-10-16）

- 7.4 秒音频 → 转写 **1.7 秒**（CPU，16 线程）
- 模型加载约 30 秒（期间 `/v1/models` 返回 **503 "Loading model"**，不是就绪信号）
