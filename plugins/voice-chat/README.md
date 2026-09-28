# voice-chat/ — 语音对话插件

对着麦克风说话 → **声纹验证（只认你的声音）** → ASR → OkHuman 对话 → TTS 朗读回复（Kokoro 晓依）。输入与输出解耦：播报期间麦克风不停录，播报回声由声纹门控滤除。回复**按条排队 + 消息内句子级流式播报**（跨实例 FIFO，2026-09-21）。
全自包含：二进制 / 模型 / Python 环境都在本目录内，不依赖任何外部项目。

移植自 `<副本目录>/与模型语音交流/`（2026-09-15）：agent 腿由 QwenPaw(:8088) 换成
语音话经 `POST /enqueue` 按唤醒词路由到对应实例；回复音频统一从各实例 `GET /events`（SSE 实时流）取，`/history` 轮询兜底。其余链路（VAD / 声纹 / ASR / TTS）不变。

## 播放架构（按条排队 + 句子级流式，2026-09-21）

**队列粒度 = 一条 agent 消息**：各实例的消息按产出顺序进同一个全局 FIFO（跨实例交错），一条播完取下一条。**消息内句子级流式**：agent 流式吐出一个完整句（句末标点）就立刻送 TTS 开播，不等整条/整轮结束。

- **音频源（主）：`events_listener`**——每实例一条 `GET /events` SSE 连接，实时收 run 事件：`run_start` 建轮次状态 → `delta.text` 累积、一到完整句就入该消息的句队列 → `tool_call / stopped / done` 为消息边界（冲刷尾句、全文打标、发结束哨兵）→ `run_end` 清轮次。语音唤醒发起的轮次用 `voice_tag` 时间戳判定（2s 窗）。
  - **重放保护**：连接时若 hello 报 `running=true`（run 进行中），跳过整轮重放、等 run 结束后重连——否则重放会把已完成的整轮重新入队（双播）。该 run 的消息由轮询兜底在 run 结束后补播。
- **音频源（兜底）：`backstop_poller`**——每 8s 轮询各实例 `GET /history`，**仅在实例空闲时**（`/status` 无活跃 run、无未结算后台任务——run 中 /history 可能被 WebUI 刷新截断致条目数失真）数新条目。流式路径已在消息边界打标（`mark_broadcast()`，30 分钟窗口）的跳过；未打标的挂起 1.5s 复核（防"条目已落库、流式打标未落地"的毫秒级竞态）仍无标才补播为一条消息（句子预载）。
- **播报员：`player` + `play_message`**——串行播队列项；每项先按 "小K说：/副本实例说：" 前缀（只加第一个非空句）→ `clean_for_speech` → ≤50 字分段 → Kokoro 合成 → aplay 播放；**播第 N 段时后台预取合成第 N+1 段**。消息队列饿死 60s 且该轮次已死（`RUN_LIVE` 表超 5 分钟）→ 弃尾，防实例宕机把播报员卡死。
- 超长保护：单消息累计 `max_reply_chars`（默认 5000）字后停止入队后续句子（全文仍打标，防轮询重播）。
- **全渠道广播开关**：WebUI "🔊 全量播报"（状态文件 `data/broadcast_off`，**默认开**）。开 → 所有渠道（QQ / WebUI 打字 / 定时任务 / 语音）的消息都播；关 → 只播语音唤醒发起的消息（非语音发起的第一句即门控丢弃）。"暂停服务"（`data/paused`）期间丢弃麦克风输入，agent/TTS 保持热备。

## TTS（Kokoro-82M，本地）

Kokoro 中文音色（8 个，config.json `kokoro_voice` 指定，**当前 zf_xiaoyi 晓依**，标准普通话女声；晓北偏东北/北京腔）：男 zm_yunjian/yunxi/yunxia/yunyang，女 zf_xiaobei/xiaoxiao/xiaoyi/xiaoni。

- serve：`kokoro_serve.py`（`kokoro_venv/`，stdin 一行文本 → outdir 出 wav，模型常驻），CPU RTF≈0.2~0.8（比实时快，段间预取合成不空转）
- 英文：v1.0 中文管线默认删掉英文段，serve 里包了一层 G2P——拉丁字母段经 espeak IPA 转换后正常朗读（2026-09-16 加，发音偏中式英语腔属正常）
- 播放：常驻 aplay 进程读 stdin 管道（单条连续音频流，段间零间隙零重叠；`aplay -q -D pipewire`）。2026-09-16 两轮实测复盘：① aplay 播完一段 wav 即正常退出（"常驻"是假象，实际每段重起），尾部缓冲竞态会 BrokenPipe（318 段 75 次）但自动恢复、听感可接受；② 曾改「每段一次性 aplay」→ aplay 退出时 PipeWire 缓冲尾部还在播，段段边界重叠，用户听到回音 → 已恢复常驻单流。防双播：mark_broadcast() 返回「是否新打标」，语音管道与全量播报轮询器都只在首次见到某条回复时入队（2026-09-16 修：原竞态会同一句播两遍）。模型/音色首次运行自动从 hf-mirror 下载（缓存在 ~/.cache/huggingface）
- 换音色：改 `kokoro_voice` 后重启 voice_loop

## TSE 目标说话人提取（2026-09-16 已删除）

SpEx+ 声纹分离（129M 模型 + serve + 代码）已于 2026-09-16 清理：宿舍实战效果不如预期，纯声纹门控已够用。若将来需要"剔除同时说话的他人声音"再重建（会话存档里有完整实现记录）。

## 架构控制台 WebUI

`http://127.0.0.1:8495/`（随 start.sh 一起启动，纯 stdlib 无依赖）：

- **状态总览**：主循环 / TTS / 声纹门控 / ASR / 暂停状态，3 秒自动刷新
- **改配置**：改任意配置项 → "保存并应用"（校验后写 config.json 并重启 voice_loop，
  约 10s TTS 模型重载）；只保存不重启需手动
- **暂停/恢复**：暂停 = 麦克风输入丢弃（agent/TTS 服务保持热备，恢复零等待）；
  实现是 `data/paused` 状态文件，voice_loop 每轮检查
- 日志尾部实时展示（/tmp/voice_loop.log）

## 链路

```
麦克风 --16k mono--> VAD 端点检测
  --> 声纹门控 (CAM++, :8091)        非本人声音直接丢弃
  --> ASR (Qwen3-ASR-0.6B CPU, :8090)
  --> 唤醒词路由 (config.json instances: 唤醒词→实例, 模糊匹配)
  --> OkHuman POST /enqueue 按唤醒词路由 (示例：小K→8451 / 小B→8452)
  <-- 各实例 GET /events (SSE 实时流) 收回复音频 + /history 轮询兜底
  --> TTS (Kokoro-82M serve, stdin 管道, 模型常驻, 段间预取)
  --> 喇叭 (aplay, 串行不叠音)
```

**线程结构（2026-09-21）**：`mic_worker`（常听，输入随时可进）→ 每实例
`agent_worker`（语音话 `POST /enqueue`，fire-and-forget）+ 每实例
`events_listener`（SSE 实时按条构建消息、句级入队）→ `player`（按条串行；
消息内按句流式，**播第 N 段时后台预取合成第 N+1 段**，CPU 不闲）+
`backstop_poller`（/history 兜底补漏）。播报内容是 agent 的**全部消息正文**
（中间叙述 + 最终回复），不只是最后一条。


**注意**：语音轮次进 OkHuman 的**主会话**——与 WebUI / QQ 频道共享同一上下文
（语音里说的话 agent 都记得，反之亦然）。这是解耦契约的固有语义，不是 bug。

## 启动 / 停止

```bash
./start.sh      # 声纹门控 + ASR + voice_loop（含 Kokoro TTS serve，首次模型加载约 10-30s）
./stop.sh       # 全部停止（模式已限定本插件路径，不误伤 OkHuman 的两个 LLM 服务）
```

**运行位置：本目录**（`plugins/voice-chat`）。
运行时资产（venv / kokoro_venv / asr / 声纹注册）都在本目录内，首次使用先跑 `./setup.sh`。

`start.sh` 幂等：门控 / ASR 已在跑就跳过；OkHuman 不可达时只警告不拦启动
（语音轮次会逐次报错重试，拉起来 OkHuman 即可恢复）。

## 配置（本目录 config.json，config.example.json 为模板）

| 键 | 默认 | 说明 |
|---|---|---|
| `okhuman_url` | `http://127.0.0.1:8451` | 对话目标 OkHuman 实例 |
| `gate_threshold` | `0.60` | 声纹余弦阈值（嫌别人混进来调高；嫌自己被拦调低 0.5） |
| `mic` / `speaker` | `default` / `0,0` | ffmpeg alsa 输入设备 / aplay 输出设备 |
| `max_reply_chars` | `500` | 回复超此字数截断朗读（防喇叭念到天荒地老；想听全改大） |
| `kokoro_voice` | `zf_xiaoyi` | TTS 音色（见上文音色列表）；改后重启 voice_loop 生效 |
| `instances` | 示例：小K→8451, 小B→8452 | 路由实例，每个实例绑一个唤醒词（也是它的“名字”）。喊哪个唤醒词 → 这句话只发给**对应**实例，回复加固定前缀逐个 TTS 播放（“小K说：…”）。用法：①一句话内说完（“小K 帮我看看…”）②先只说唤醒词 → 提示音 → 15 秒内说内容（发给该实例）。只配一个实例时唤醒门控自动关闭（你的语音直达，无前缀）。唤醒词要选 ASR 能稳定转写的短词（避免易被误识的音） |

追加你的声纹样本：`curl "http://127.0.0.1:8091/enroll?wav=/path/新录音.wav"`
（门控运行中；样本存 `speaker/enrollments/`，请注册你自己的声纹样本（可多条））。

## 目录

| 路径 | 内容 |
|---|---|
| `voice_loop.py` | 主循环：麦克风→VAD→声纹→ASR→OkHuman→TTS→喇叭 |
| `speaker/speaker_server.py` | 声纹门控服务 (CAM++, :8091) |
| `speaker/kaldi_fbank.py` | vendor 的纯 python Kaldi fbank（去 torchaudio 依赖） |
| `asr/bin/` + `asr/models/` | llama-server CPU build + Qwen3-ASR-0.6B |
| `kokoro_serve.py` | Kokoro-82M TTS serve（stdin 管道协议） |
| `kokoro_venv/` | Kokoro serve 的 Python 环境（torch cpu + misaki + phonemizer） |
| `venv/` | 统一 Python 3.12 环境（torch cpu 等） |

## 排障

- **日志**：`/tmp/voice_loop.log`（主循环）`/tmp/speaker_gate.log` `/tmp/voice_loop_asr.log` `/tmp/voice_loop_tts_kokoro.log`
- **TTS 没声音 / 报 "tts server dead"**：看 `/tmp/voice_loop_tts_kokoro.log`。serve 起不来时
  import 阶段的崩溃 traceback 会落在这个文件（例：phonemizer 版本 API 差异、espeak 数据缺失）。
  正常日志形如 `[kokoro] wrote out_NNNN.wav (X.Xs 音频, 合成用时 X.Xs, RTF=X.XX)`。
- **总被声纹拦**：调低 `gate_threshold`（0.5 起试），或补 enroll 几段安静环境的录音。
- **唤醒词老不命中**：看 `/tmp/voice_loop.log` 里 `[wake] 未含唤醒词` 的原文——ASR 把唤醒词识别成什么了；换成 ASR 能稳定转写的词（“小K”/“副本实例” 实测稳；“救生艇实例” 会被听成 “小小”）。
- **第一次用先看门控分数**：`/tmp/voice_loop.log` 里 `[gate] 本人声音 (score=...)`——score 应 ≥ 0.6；若你自己的声音都过不了，重新 enroll 一段安静环境的录音或调低阈值。
- **别人也能驱动**：调高 `gate_threshold`（0.7+）。
- **ASR 慢**：CPU 跑 0.6B 模型，单句通常 2~10s；机器满载（两个 27B 服务在跑）时会更慢。
- **说话没反应**：看 `/tmp/voice_loop.log` 有没有 `[mic] 检测到语音`——没有就是 VAD
  阈值问题（环境噪声大时底噪估计会自适应，安静房间试一次冷启动）。

## 解耦契约自检

- [x] 不 import OkHuman 任何模块（纯 Python，OkHuman 只在运行时作为 HTTP 服务存在）
- [x] 独立进程（门控 / ASR / TTS / 主循环各是独立进程），生命周期由 start/stop.sh 管
- [x] 配置自管（本目录 config.json），数据自管（临时 wav 落 /tmp/voice_loop_tts，声纹 enrollments 在本目录）
- [x] 独立可跑：`cd OkHuman_p/voice-chat && ./start.sh`，不需要 OkHuman 源码树任何文件
