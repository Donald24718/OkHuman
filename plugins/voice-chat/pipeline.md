## 播放架构（按条排队 + 句子级流式，2026-09-21）

**队列粒度 = 一条 agent 消息**：小A/小B的消息按产出顺序进同一个全局 FIFO（跨实例交错），一条播完取下一条。**消息内句子级流式**：agent 流式吐出一个完整句（句末标点）就立刻送 TTS 开播，不等整条/整轮结束。

- **音频源（主）：`events_listener`**——每实例一条 `GET /events` SSE 连接，实时收 run 事件：`run_start` 建轮次状态 → `delta.text` 累积、一到完整句就入该消息的句队列 → `tool_call / stopped / done` 为消息边界（冲刷尾句、全文打标、发结束哨兵）→ `run_end` 清轮次。语音唤醒发起的轮次用 `voice_tag` 时间戳判定（2s 窗）。
  - **重放保护**：连接时若 hello 报 `running=true`（run 进行中），跳过整轮重放、等 run 结束后重连——否则重放会把已完成的整轮重新入队（双播）。该 run 的消息由轮询兜底在 run 结束后补播。
- **音频源（兜底）：`backstop_poller`**——每 8s 轮询各实例 `GET /history`，**仅在实例空闲时**（`/status` 无活跃 run、无未结算后台任务——run 中 /history 可能被 WebUI 刷新截断致条目数失真）数新条目。流式路径已在消息边界打标（`mark_broadcast()`，30 分钟窗口）的跳过；未打标的挂起 1.5s 复核（防"条目已落库、流式打标未落地"的毫秒级竞态）仍无标才补播为一条消息（句子预载）。
- **播报员：`player` + `play_message`**——串行播队列项；每项先按 "小A说：/小B说：" 前缀（只加第一个非空句）→ `clean_for_speech` → ≤50 字分段 → Kokoro 合成 → aplay 播放；**播第 N 段时后台预取合成第 N+1 段**。消息队列饿死 60s 且该轮次已死（`RUN_LIVE` 表超 5 分钟）→ 弃尾，防实例宕机把播报员卡死。
- 超长保护：单消息累计 `max_reply_chars`（默认 5000）字后停止入队后续句子（全文仍打标，防轮询重播）。
- **全渠道广播开关**：WebUI "🔊 全量播报"（状态文件 `data/broadcast_off`，**默认开**）。开 → 所有渠道（QQ / WebUI 打字 / 定时任务 / 语音）的消息都播；关 → 只播语音唤醒发起的消息（非语音发起的第一句即门控丢弃）。"暂停服务"（`data/paused`）期间丢弃麦克风输入，agent/TTS 保持热备。

## 链路

```
麦克风 --16k mono--> VAD 端点检测
  --> 声纹门控 (CAM++, :8091)        非本人声音直接丢弃
  --> ASR (Qwen3-ASR-0.6B CPU, :8090)
  --> 唤醒词路由 (config.json instances: 唤醒词→实例, 模糊匹配)
  --> OkHuman POST /enqueue 按唤醒词路由 (小A→8451 / 小B→8452)
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
