## 依赖（2026-10-16 拆分）

| 能力 | 提供方 | 端点 / 协议 | 本插件如何用 |
|---|---|---|---|
| ASR | `Frequently Used Plugins/asr` | OpenAI 兼容 `POST :8090/v1/audio/transcriptions` | `start.sh` 探活 :8090，不通则调用其 `start.sh`；`ASR_URL` 在 voice_loop 里 |
| TTS | `Rarely Used Plugins/year_2026/month_11/day_02/tts` | `serve.py` 的 stdin 一行文本 → outdir 出 wav（**不是**它的 HTTP :8092） | voice_loop 用 `tts_dir`（默认 `../tts`）定位 `venv/bin/python` + `serve.py` |
| 声纹门控 | 本插件 `speaker/` | `POST :8091/verify` | 自带，未拆 |

只想单独用识别或合成，不必启动本插件：`asr/transcribe.sh 音频.wav`、`tts/say.sh "文本"`。
本插件停止不会停掉 asr / tts（它们是共享服务，各自 `stop.sh` 管理）。

## 配置（本目录 config.json，config.example.json 为模板）

| 键 | 默认 | 说明 |
|---|---|---|
| `okhuman_url` | `http://127.0.0.1:8451` | 对话目标 OkHuman 实例 |
| `gate_threshold` | `0.60` | 声纹余弦阈值（嫌别人混进来调高；嫌自己被拦调低 0.5） |
| `mic` / `speaker` | `default` / `0,0` | ffmpeg alsa 输入设备 / aplay 输出设备 |
| `max_reply_chars` | `500` | 回复超此字数截断朗读（防喇叭念到天荒地老；想听全改大） |
| `kokoro_voice` | `zf_xiaoyi` | TTS 音色（见上文音色列表）；改后重启 voice_loop 生效 |
| `instances` | 小A→8451, 小B→8452 | 路由实例，每个实例绑一个唤醒词（也是它的“名字”）。喊哪个唤醒词 → 这句话只发给**对应**实例（小A→8451、小B→8452），回复加固定前缀逐个 TTS 播放（“小A说：…”/“小B说：…”）。用法：①一句话内说完（“小A 帮我看看…”）②先只说唤醒词 → 提示音 → 15 秒内说内容（发给该实例）。只配一个实例时唤醒门控自动关闭（你的语音直达，无前缀）。唤醒词要选 ASR 能稳定转写的（“小A”/“小B” 实测稳；“小欧” 会被听成 “小小”，不可用） |

追加你的声纹样本：`curl "http://127.0.0.1:8091/enroll?wav=/path/新录音.wav"`
（门控运行中；样本存 `speaker/enrollments/`，当前已注册 XiaoHei/XiaoK/XiaoO/ey/weiXie 五条）。

## 目录

| 路径 | 内容 |
|---|---|
| `voice_loop.py` | 主循环：麦克风→VAD→声纹→ASR→OkHuman→TTS→喇叭 |
| `speaker/speaker_server.py` | 声纹门控服务 (CAM++, :8091) |
| `speaker/kaldi_fbank.py` | vendor 的纯 python Kaldi fbank（去 torchaudio 依赖） |
| ~~`asr/`~~ | 已拆到 `Frequently Used Plugins/asr`（llama-server CPU build + Qwen3-ASR-0.6B，:8090） |
| ~~`kokoro_serve.py`~~ | 已拆到 `Rarely Used Plugins/year_2026/month_11/day_02/tts/serve.py`（stdin 管道协议不变） |
| ~~`kokoro_venv/`~~ | 已拆到 `Rarely Used Plugins/year_2026/month_11/day_02/tts/venv`（torch cpu + misaki + phonemizer） |
| `venv/` | 统一 Python 3.12 环境（torch cpu 等） |

## 解耦契约自检

- [x] 不 import OkHuman 任何模块（纯 Python，OkHuman 只在运行时作为 HTTP 服务存在）
- [x] 独立进程（门控 / ASR / TTS / 主循环各是独立进程），生命周期由 start/stop.sh 管
- [x] 配置自管（本目录 config.json），数据自管（临时 wav 落 /tmp/voice_loop_tts，声纹 enrollments 在本目录）
- [x] 独立可跑：`cd plugins/voice-chat && ./start.sh`，不需要 OkHuman 源码树任何文件
