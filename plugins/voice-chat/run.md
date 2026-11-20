## 启动 / 停止

```bash
./start.sh      # 声纹门控 + 复用 asr 插件起 ASR + voice_loop（内部拉起 tts 插件的 serve，首次模型加载约 10-30s）
./stop.sh       # 全部停止（模式已限定本插件路径，不误伤 OkHuman 的两个 LLM 服务）
```

**运行位置：生产树**（`plugins/voice-chat`，2026-09-21 起；
运行时资产：本树只剩 `venv/`（编排层 + 声纹）与 `speaker/enrollments/` 声纹注册；
ASR 二进制与模型在 `asr/` 插件、Kokoro 环境与 serve 在 `tts/` 插件。lil 树同名目录是影子副本，
留作回滚，勿从那里启动）。

`start.sh` 幂等：门控已在跑就跳过；ASR 先看 :8090 是否可达，不通则调用 `asr/start.sh`（找不到插件只警告不拦启动）；OkHuman 不可达时同样只警告。
（语音轮次会逐次报错重试，拉起来 OkHuman 即可恢复）。

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
