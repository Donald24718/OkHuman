# agent-wire — agents 之间派活 / 回信

让一个 agent 在自己的回合里给**另一个 agent 派活**，并在对方跑完后拿到**它那一轮的最终正文**。
无守护进程：`send` 起一个跑完即退出的一次性 watcher。只有异步——同步等于卡着自己等，那和"不派活"没区别。

## 用法

```bash
W="<插件目录>/agent-wire"
$W list                        # 有哪些 agent、谁在线、谁正在 run（只读）
$W send agent-b "任务…" [--wait 900]   # 派活：立刻返回，不等
$W pending                     # 现在在等谁的（投过即删，不重复投）
```

对方跑完后，回信会以一条普通消息投回**发送方**实例的队列；OkHuman 的**轮内搭车**
（工具结果之后、下一次模型调用前取走）让它进入发送方**当前正在跑的这一轮**。

回信的头部由**插件拼**，发送方不参与写，多个 agent 各派任务时靠它对上：

```
【agent-wire 回信 · 来自 agent-b】
任务：<原消息前 80 字>
<对方那一轮的最终正文>
```

超时也回一条 `【agent-wire 回信超时 · 来自 agent-b】`，不会让发送方干等不知道结果。

## 规则（硬）

- **只给空闲的 agent 派活**：先读对方 `/status`，`running=true` 一律拒（退出码 2）。
- **不能给自己发**：自己此刻必然在 run。
- 取正文靠**事件游标**：先 `GET /events/poll?since=-1` 记下当前游标，派活后只看新批次 ——
  本轮内累加 `delta.text`（`thinking` 丢弃），收到 `run_end` 即整段正文。不会把上一轮误当成本轮。
  取不到时兜底读 `/history` 的最后一条 assistant 文本。

## 配置（`config.json`，与可执行文件同目录；实值不入库，参考 `config.json.example`）

- `self`：本实例在 `peers` 里的名字（用于拒绝"给自己发"、并作为回信目标）。
- `peers[]`：`{name,url[,token]}`。本机实例直连 `http://127.0.0.1:<端口>`；
  **跨机的实例走它那边的 lan-bridge**（`token` 用 `Authorization: Bearer` 带上）。
- 被访问一侧的 lan-bridge 白名单需含 `/status`、`/events/poll`、`/enqueue`（默认已放开）。

## 构建 / 运行

```bash
cd "<插件目录>" && go build -o agent-wire agent-wire.go   # 只用标准库
```

`data/waiting/`（等待任务与 watcher 日志）是运行时产物，不入库。

## 排障

- `拒：X 正在 run` —— 正常，规则如此。可用 `list` 先看状态，隔几秒再试。
- 派活成功但没收到回信：先看 `pending`（还在等 = watcher 还在跑）、再看 `data/waiting/<id>.log`；
  若事件形态变了，直接看 `curl -H "Authorization: Bearer <token>" "http://<桥>/events/poll?since=0"`
  里 `frames` 的真实结构（一帧是整块 SSE，需按 `data:` 行解析）。
- 跨机不可达：确认对端 lan-bridge 在监听、token 正确。
