# asr — 独立 ASR 服务（Qwen3-ASR-0.6B）

从 `voice-chat` 拆出来的独立插件（2026-10-16）。只想要"音频→文本"时不必拉起整套语音对话。

## 是什么

llama.cpp **CPU build** 的 `llama-server` + Qwen3-ASR-0.6B（bf16 GGUF + mmproj），
对外是 **OpenAI 兼容接口**，任何程序 POST 音频即可，不依赖 OkHuman。

## 启动 / 停止

```bash
./start.sh        # 起 llama-server :8090，探活到 HTTP 200 才返回
./stop.sh         # 停（pkill 模式限定 Qwen3-ASR，不误伤别的 llama-server）
./transcribe.sh 音频.wav        # 直接转写一个文件，stdout 出纯文本
```

## 接口

```bash
curl -s http://127.0.0.1:8090/v1/audio/transcriptions \
  -F "file=@speech.wav" -F "model=qwen3-asr-0.6b" -F "language=Chinese"
# -> {"type":"transcript.text.done","text":"language Chinese<asr_text>识别结果","usage":{...}}
```

**取文本要剥两层前缀**：`language Chinese` 与 `<asr_text>`（2026-10-28 实测响应里两段都在；
`transcribe.sh` 已处理，自己拼 curl 时别只剥 `<asr_text>`）。响应还带 `type` 字段，别按整段 JSON 当文本用。

## 配置（config.json）

| 键 | 默认 | 说明 |
|---|---|---|
| `port` | 8090 | 服务端口 |
| `model` / `mmproj` | models/ 下两个 gguf | 相对插件目录 |
| `threads` | 16 | `-t` |
| `ctx` | 8192 | `-c` |
| `model_name` | qwen3-asr-0.6b | 请求里 `model` 字段的取值 |

## 实测（本机 2026-10-16）

- 7.4 秒音频 → 转写 **1.7 秒**（CPU，16 线程）
- 模型加载约 30 秒（期间 `/v1/models` 返回 **503 "Loading model"**，不是就绪信号）

## 两台机器上的实况（2026-11-06 实测，两台都能本机跑，不必跨机）

| | 生产实例（本机） | 另一台（副本） |
|---|---|---|
| 二进制与模型 | 本插件目录内 `bin/llama-server` + `models/`（同结构） | 同：`同一插件目录（配置里换根路径）`，`models/Qwen3-ASR-0.6B-bf16.gguf`(1.5 G)+`mmproj-…`(378 M) |
| 端口 | 8090 | 8090（各绑本机回环，互不冲突），已实测 `:8090`=200 |
| 常驻方式 | **不是 systemd 单元**（脱管进程），重启不自启 → 用 `./start.sh` | 同样不是单元 |
| 查它在不在 | `pgrep -f 'llama-server.*8090'` + `curl -sf http://127.0.0.1:8090/health` | 同 |

## 排障

- **`start.sh` 卡在"启动中"**：模型加载中会返回 503，脚本只认 HTTP 200，会一直等到 120 秒。
  看 `tail -f /tmp/asr_plugin.log`。
- **`libggml*.so` 找不到**：`bin/` 的 RUNPATH 指向编译时目录，必须靠 `LD_LIBRARY_PATH=$HERE/bin`
  （`start.sh` 已设）。手工起 llama-server 时别忘了。
- **识别把英文词听错**（TTS 合成音尤其明显：`TTS`→`DDS`、`words`→`Voice`、
  `8092`→"八千零九十二"）：模型固有误差，不是服务问题。真人语音明显好于合成音。
- **端口被占**：改 `config.json` 的 `port`，同时记得 voice-chat 那边 `ASR_URL` 也要改。

## 与 voice-chat 的关系

voice-chat 不再自带 ASR：它的 `start.sh` 探活 :8090，不通就调用本插件 `start.sh`。
本插件挂了 voice-chat 的识别会报错重试；voice-chat 挂了不影响本插件。

## 用法坑（2026-10-27）

`transcribe.sh` 第一行就 `cd "$(dirname "$0")"`，所以**必须传绝对路径**；
传相对路径时 curl 取不到文件，脚本**静默返回空**（不报错），容易被误判成"模型没识别出来"。
整片 90 秒一次转写没问题（16 kHz 单声道 wav）。

- 别自己拼 HTTP multipart 调接口（boundary/字段名易错且失败静默）→ 一律走 `./transcribe.sh <绝对路径wav>`。
