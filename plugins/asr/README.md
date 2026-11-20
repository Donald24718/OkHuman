# asr — 独立 ASR 服务（Qwen3-ASR-0.6B）

从 `voice-chat` 拆出来的独立插件（2026-10-16）。只想要"音频→文本"时不必拉起整套语音对话。llama.cpp **CPU build** 的 `llama-server` + Qwen3-ASR-0.6B（bf16 GGUF + mmproj），对外是 **OpenAI 兼容接口**，任何程序 POST 音频即可，不依赖 OkHuman。**本文件不含端口、参数与调用细节。**

## 要做什么 → 去哪个文件

| 要做 | 文件 |
|---|---|
| 起停服务、改端口/线程/模型、查它在不在（两台都本机跑） | `run.md` |
| 转写一个文件、调接口、把返回里的前缀剥掉 | `use.md` |
| 起不来、缺库、听错词、结果为空、端口被占 | `trouble.md` |

## 遇到现象 → 去哪个文件

| 现象 | 文件 |
|---|---|
| `start.sh` 一直"启动中" / 503 | `trouble.md`（模型加载中） |
| 转写返回空 | `trouble.md`（相对路径 → 静默空） |
| 英文词、数字被听错 | `trouble.md`（模型固有误差，合成音尤甚） |
| 返回值里混着 `language Chinese`、`<asr_text>` | `use.md`（剥两层前缀） |
