# tts — 独立 TTS 服务（Kokoro-82M，中英混读）

从 `voice-chat` 拆出来的独立插件（2026-10-16）。只想要"文本→语音"时不必拉起整套语音对话。

## 启动 / 停止 / 用

```bash
./start.sh                       # 起常驻 HTTP 服务 :8092（模型后台加载，就绪前 /health 返回 ok:false）
./say.sh "要念的文本" [输出.wav]  # 直接合成；服务没起会自动拉起；stdout 打印 wav 路径
./stop.sh
```

## 接口（零依赖，只用标准库）

```bash
curl -s -X POST http://127.0.0.1:8092/tts \
  -H 'Content-Type: application/json' \
  -d '{"text":"你好，English too","voice":"zf_xiaoyi","format":"json"}'
# -> {"path":"/tmp/tts_http_out/tts_xxxx.wav","seconds":1.23,"ms":456}

curl -s "http://127.0.0.1:8092/tts?text=你好&format=wav" -o out.wav   # 直接拿音频字节
curl -s http://127.0.0.1:8092/health                                   # {"ok":true,...}
```

合成加锁串行（Kokoro 非线程安全），并发请求排队即可。

## 配置（config.json）

| 键 | 默认 | 说明 |
|---|---|---|
| `port` / `host` | 8092 / 127.0.0.1 | HTTP 服务 |
| `voice` | zf_xiaoyi | 音色（晓依；请求里 `voice` 可覆盖） |
| `outdir` | /tmp/tts_http_out | wav 输出目录 |

## 两种协议，别搞混

- **`server.py`（HTTP，:8092）**：给外部调用方用，本插件 `say.sh` 走这条。
- **`serve.py`（stdin 一行文本 → outdir 出 wav）**：**voice-chat 仍在用这条**，
  就绪标记是日志里的 `[kokoro] Ready`。改协议要同时改 voice_loop 的 `KokoroTTSServer`。
- 两者共用 `engine.py`（模型加载 + 中英混读 G2P），只有一份实现。

## 排障

- **`'http' is not a package`**：本目录下的文件不能叫 `http.py`——脚本目录在 `sys.path`
  最前面会遮蔽标准库 `http` 包（这个坑踩过，故改名 `server.py`）。
- **英文念不出来 / 静音**：Kokoro v1.0 中文管线走 legacy_call，`en_callable` 不被调用，
  英文会原样透传导致静音。`engine.py` 的 `ZhG2PWithEnglish` 用 espeak IPA 补拉丁段，
  别绕过它直接用 `KPipeline`。
- **首次合成慢**：模型加载 5–15 秒 + jieba 建缓存；常驻服务复用进程即可，
  别每次新起进程（`say.sh` 已自动复用）。
- 模型权重从 `hf-mirror.com` 拉（`HF_ENDPOINT` 已在 engine.py 里设默认）。

## 实测（本机 2026-10-16）

- 一句 30 字中英混读 → **7.42 秒**音频（24 kHz / 单声道 / 16 bit），CPU 合成 RTF≈0.35
