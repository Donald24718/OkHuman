# 运行时机制详解：工具超时转后台、结果回投、轮末校验

本文讲 OkHuman 主循环（`internal/agent/agent.go` 的 `runLoop`）里三件互相咬合的事：
**长命令怎么不阻塞对话**、**它的结果怎么回到模型眼前**、**模型收笔前为什么要再问一次**。
所有描述对应代码行为，非设计愿景。

---

## 1. 一次工具调用 = 与前台超时赛跑

工具执行放在 goroutine 里，主循环 `select` 等两个来源：

```go
execDone := make(chan string, 1)
go func() { r, err := tools.ExecuteTool(name, args); execDone <- ... }()
select {
case outcome := <-execDone:            // 超时前完成 → 正常回填
case <-time.After(FgTimeoutMS):        // 超时 → 转后台，不取消
}
```

**超时前完成**：结果原样回填成 `tool` 消息。超过 `tools.result_limit`（默认 50000 字符）
则截断，全文落盘，正文尾部附一句"完整结果 N 字符在 <路径>，用 bash 的 cat / grep / tail 查看"——
模型可以自己把全文抓回来，不丢数据。

**超时**（默认 `tools.fg_timeout_sec = 30`）：

- **不取消执行**。命令继续跑，这是整个机制的前提。
- 立即回填一条 `tool` 消息（保证 `assistant.tool_calls` 与 `tool` 消息严格配对，
  协议不破）：
  > （系统：此调用前台等待超过 30 秒未完成，已自动转入后台继续执行。无需等待，
  > 可继续其他工作或直接基于已有信息回复；任务完成后系统会自动通知结果。）
- 发两个事件：`tool_result`（带 `backgrounded: true`）与 `bg_start`。
- 把那条 `execDone` 通道作为 `Promise` 交给后台编排器登记。

**硬兜底**：`tools.timeout_sec`（默认 600 秒）是命令总时长上限，到点杀掉整个命令进程组。
也就是说"转后台"不是无限放任——前台等待不占模型，但命令本身仍有天花板。

> 一个副作用值得知道：转后台的 bash 命令若自己再 `setsid` 起脱离进程，那个进程不受
> `timeout_sec` 约束，也不在后台任务表里（见 §5 局限）。

---

## 2. 后台任务编排器（`internal/background`）

`BackgroundOrchestrator` 持一张 `map[call_id]*PendingBackgroundTask`：

| 字段 | 含义 |
|---|---|
| `Settled` / `SettledAt` | 结果是否已回来 |
| `OK` | 成功/失败（结果以工具失败前缀开头即判失败）|
| `Result` | 结果副本，表内最多存 256 KB（供 `/backgrounds` 查看）|
| `DurationMs` | 从转后台到出结果的耗时 |
| `Notified` | 完成通知是否已入队 |

注册时同时起一个 goroutine 阻塞在 `<-Promise`。结果一到：标记 settled → 调 `OnSettled`
回调（由 server 层实现：格式化通知 → push 进消息队列 → 唤醒 drain）→ 标记 `Notified`。
注意 `Result` 在表里是截断副本，**入队通知用的是完整结果**（超长时同样落盘 + 截断）。

`Unsettled()` 返回未 settle 的任务数——§5 的闸门就靠它。
`Clear()` 在会话重置时清空。

---

## 3. 结果怎么回到模型眼前：两条路

**路 A：轮内搭车（同一轮内，不加 LLM 调用）**
工具循环里，每批工具结果之后、每次 LLM 调用之前，调 `TakePendingUsers()` 取走运行期间
入队的全部消息，同车注入。这是**纯追加**：已缓存的上下文前缀不变，所以不产生额外的
KV 缓存代价，也不多发一次 LLM 调用。

注入时按前缀区分来源：

- 以 `（系统通知：后台任务完成` 开头 → 原样注入，**不加**【用户追加】前缀
- 其余 → 加 `【用户追加】` 前缀，让模型明确知道是"追加指令"而不是"新任务"

**路 B：下一次 run**
若本轮已经收笔，settle 的通知就留在消息队列里，下一次 drain 时作为新消息触发一轮新运行。

通知的统一格式（`FormatBackgroundNotice`）：

> （系统通知：后台任务完成——你此前调用 bash 时前台等待超时，已转入后台继续执行，
> 现已成功（耗时 250.7s）。结果如下：
> 退出码: 0
> …）

失败则写"现已失败"。这条通知是模型判断"外部工作结束了"的唯一信号，所以措辞里明确带上
工具名、成败、耗时。

---

## 4. 轮末校验（`end_check`）：收笔前再问一次

**触发条件**：模型输出结束且**没有 tool_calls** —— 这被视为本轮的停止信号。

**动作**：存下 assistant 消息 → 发 `end_check` 事件 → 注入一条 user 校验提示 →
再发一次 LLM 调用。

**两个分支**：

- 模型仍无 tool_calls → 确认真的做完了，正式收笔。**本轮回复取校验前的正文**
  （真实答案），不是校验应答。
- 模型发出 tool_calls → 说明有未完成事项，马上继续工具循环。

**防乒乓**：每次停止至多校验一次；校验后仍停就直接收笔，不再追问。
**可关**：`agent.end_check = false` → 无工具调用即收笔（旧行为）。

**措辞就是判据**（这条是踩坑换来的）。旧文案问"目前有没有还未完成的事情"，
模型会把它读成"整个任务做完了没有"——只要还有东西没跑完，答案永远是"有"，
于是每次收笔都被推回去。现文案问的是：

> 此刻有没有**必须你现在就做的**动作？有 → 发出工具调用；没有 → 不再调用工具，一句话收笔。

差别在"此刻"和"必须你现在就做"：等待外部结果不算"有"。

---

## 5. 闸门：有在飞后台任务时，连问都不问

光靠措辞不够——模型仍可能误判。所以机制层直接拦：

```go
n := 0
if opt.UnsettledBackground != nil { n = opt.UnsettledBackground() }   // = BG().Unsettled()
if opt.EndCheck && n > 0 {
    emit({"type":"end_check_skipped", "detail": "有 N 个后台任务未 settle → 不追问，直接收笔"})
    return a.finishText(resp, emit, totals)
}
```

**为什么值得专门加**：实测一次 5 GiB 模型下载期间，模型被追问十余次，
每一次都是一次完整的 LLM 推理——而下载这件事一次都不需要模型。追问不产生价值，
只烧算力，还占着推理服务的排队位置。

**已知局限（重要）**：闸门只统计**经编排器注册**的转后台任务。模型自己用 `setsid`
完全脱离启动的进程（比如起一个常驻服务）不在任务表里，`Unsettled()` 看不见它们。
这类场景仍会追问一次——此时靠 §4 的措辞兜底。

---

## 6. 配置与可观测

| 配置 | 默认 | 作用 |
|---|---|---|
| `tools.fg_timeout_sec` | 30 | 前台等待上限（秒），超时转后台 |
| `tools.timeout_sec` | 600 | 命令总时长硬上限（秒），到点杀进程组 |
| `tools.result_limit` | 50000 | 回填给模型的字符上限，超出截断+落盘 |
| `agent.end_check` | true | 轮末校验开关（热加载，下轮运行生效）|
| `doom.warn_after` | 3 | 连续 N 次完全相同调用 → 注入警告，警告后仍重复 → 强停 |
| `heartbeat.enabled` | false | 空闲到点自动开启一轮 run（类型必须是布尔，否则 `POST /config` 直接 400）|
| `heartbeat.interval_min` | 30 | 空闲多少分钟后触发；run 期间不计时，run 结束才重置计时 |
| `heartbeat.prompt` | — | 到点发给模型的心跳提示词（必须是字符串）|
| `llm.timeout_sec` | 300 | 非流式请求整体超时（秒）|
| `llm.stream_idle_sec` | 120 | **主循环**流式空闲超时（秒）：连续这么久没有任何 wire 字节（含 `: ping` 注释帧）→ 本轮报错收尾。此前主循环没有可配口径：空闲超时回落到 `max(timeout, 10 分钟)`，后端卡住就白挂 10 分钟 |
| `context.stream_idle_sec` | 90 | **压缩流**空闲超时（秒），无 wire 字节 → 重试 |

> **时间类配置一律以秒为单位**（2026-10-18 统一）。旧的 `*_ms` 键仍被接受：启动加载与
> `POST /config` 都会自动换算（向上取整，500ms → 1s），并在响应/日志里回显换算说明；
> 同一补丁里新旧键并存时以新键为准。秒键有范围守卫（`timeout_sec ≤ 86400`、
> `stream_idle_sec ≤ 3600`）——把旧的毫秒数值填进秒键（`timeout_ms=300000` 当秒就是
> 3.5 天）会被 400 挡下，而不是接受一个荒谬的超时。

热加载分区：`llm`、`context`、`tools`、`doom`、`agent`、`heartbeat`（`POST /config` 改完即时生效，
`agent` 分区在下一轮运行时生效）。

**事件流**（`GET /events`，WebUI 据此渲染卡片）：
`tool_call` → `tool_result`（可能带 `backgrounded`）→ `bg_start` → `user_piggyback` →
`end_check` / `end_check_skipped` → `doom_warn` / `doom_stop` → `done`

**查在飞任务**：`GET /backgrounds` → `{"count":N,"running":bool,"tasks":[{call_id,tool,started_at,settled,ok,duration_ms,result_chars}]}`

---

## 7. 三件事怎么咬合（一次完整时间线）

```
assistant  tool_calls: bash(sleep 200)
tool       （系统：此调用前台等待超过 30 秒…已自动转入后台…）      ← 不阻塞，模型可继续干别的
assistant  （无 tool_calls，收笔）
           → 闸门查 Unsettled()=1 → end_check_skipped → 本轮结束，不追问
… 命令跑完 …
user       （系统通知：后台任务完成——…现已成功（耗时 200.3s）…）   ← 路 B，触发新一轮
assistant  基于结果继续干活
assistant  （无 tool_calls，收笔）
           → 闸门查 Unsettled()=0 → end_check → 注入校验提示
assistant  （仍无 tool_calls）→ 正式收笔，回复取校验前正文
```

设计取向一句话：**模型不该等 I/O，但也不该被追问去替 I/O 操心。**

---

## 8. 上下文的手动处置（2026-10-18）

**曾经有、现已删除的机制**：LLM 回 4xx 时的三级回退阶梯（降级最近一条附件 →
降级全部附件 → 兜底硬截断上下文）。问题在触发条件：400/413/415 是按某一个后端的
响应行为写死的，换推理后端同一件事可能报别的码 —— 阶梯既不触发也拦不住，等于把
猜测固化进控制流；而"自行截断上下文"是不可逆的破坏性动作，不该由机器在猜错时做。
现在 LLM 报错就原样抛出、本轮终止，处置交给人按按钮。

| 端点 | 干什么 | 备注 |
|---|---|---|
| `POST /compress` | 手动压缩上下文：不等 token 阈值，按 `context.keep_recent_chars` 的同口径把最旧一批交给 LLM 压成 summary | 与自动压缩同一条路径、同样落两个文件；run 进行中返回 409（压缩换掉会话结构，与在飞 run 交错会让下一轮看到意外上下文）|
| `POST /strip-injects` | 手动剥离全部外部注入附件：侧车永久回写成文本提示，之后的调用不再发原附件内容 | 永久、不是本轮；run 进行中**允许**（坏附件卡住 run 时这是自救路径，只改引用解析结果，不动会话结构）|
| `POST /reset` | 换会话前先把旧会话全文落盘，响应里回 `record` 路径 | 重置会整体换掉 Manager，不存旧会话原文就无处可寻 |

WebUI 的「重置会话」右边就是这两个按钮；心跳倒计时常驻左侧栏底部（切到任何页面都看得见）。

**落盘文件**（`data.dir` 下，与 `session.json` 同目录）：

- `session-records/ctx-<会话号>-<时间戳>-seq<N>.txt` —— 落盘时刻的完整会话
  （系统提示词 + summary + 全部消息原文）。文件头标 `触发=压缩` 或 `触发=重置会话`。
- `session-summaries/<同名基座>.summary.txt` —— 压缩产出的总结文本，与上面的存档同名基座配对。

压缩成功后，summary 尾部会附这两条路径，agent 需要被压掉的细节时用 `bash cat/grep`
按需抓回 —— 上下文变小，但原文不丢。
