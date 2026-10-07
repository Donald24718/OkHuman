# qq-channel/ — QQ 官方渠道桥（okhuman-qq-bridge）

把 OkHuman 实例接入 QQ 的**独立常驻进程**（Go，TS 版 bridge.ts + qq.ts 的 1:1 移植）：
QQ 消息进 agent、agent 回复回 QQ。无监听端口，只有出站连接（QQ 网关 WebSocket + 实例 HTTP/SSE）。

## 链路（常驻转发管道）

- **上行**：QQ 消息 → `POST {实例}/enqueue { message }`（入队即返，不等回合结束）
- **下行**：桥常驻订阅 `GET {实例}/events`（SSE 广播）→ 按事件边界整段拆气泡发：
  - thinking 整段 ≤2950 字（`[思考]` 前缀）
  - text 整段 ≤3000 字
  - tool_call → `[工具]`、tool_result → `[结果]`
  - finish → flushAll + reply 兜底、error → `[出错]`
  - 轮内 60s 静默 → 15s 轮询 `/history` 补发回尾 → 10min 放弃
- 多 bot：`bots[]` 每项绑定一个实例（`instance` 字段），限流 `max_msgs_per_min`（默认 20）
- 重连自愈：QQChannel 内部自动重连/重认证（heartbeat 30s / 重连 3s / 重认证 30s）
- 收尾：SIGINT 优雅关闭所有 WS；SIGTERM 立即退出

## 启动 / 停止 / 重建

```bash
# 启动（日志落 /tmp/okhuman-qq-bridge.log；/tmp 是 tmpfs，重启清空）
cd "<本目录>" && setsid ./okhuman-qq-bridge > /tmp/okhuman-qq-bridge.log 2>&1 &

# 停止（SIGTERM 立即退）
kill <pid>

# 改完源码后重建（Go 工具链 /usr/local/go，go 在 /usr/local/bin）
cd "<本目录>" && go build -o okhuman-qq-bridge okhuman-qq-bridge.go
```

**改完源码 = 没干活，重编 + 真实启动验证 = 才干完**（2026-09-20 老 bug 复发的根因就是忘了 go build，见 LESSONS.md）。

## 配置（本目录 config.json，config.json.example 为模板）

首次使用：`cp config.json.example config.json`，填入你自己的 app_id / client_secret / channel_id。

- `data_dir`：运行时数据目录。**相对路径按可执行文件所在目录解析**（`pluginRoot()`：exe 目录里有 config.json 即用之，否则回退 `.`）；绝对路径直接用
- `instances[]`：要接入的 OkHuman 实例（`{ id, url }`），id 不可重复
- `bots[]`：QQ 开放平台凭据 + 绑定：`app_id` / `client_secret` / `channel_id`（c2c 形如 `c2c:<openid>`）/ `instance` / `max_msgs_per_min`
- `markdown_enabled`：markdown 消息（md=2）开关
- `client_secret` 不入日志；config.json 已 gitignore（真实凭据不入库）

## 数据与状态（本目录 data/，已 gitignore）

- `channels/qq/tokens/<botId>.json`：access_token 盘缓存（原子写；QQ 返回的 `expires_in` 兼容字符串/数字两种形态，`json.Number` 解析——2026-09-19 修的移植 bug）
- `channels/qq/msg_seq`：c2c 消息序号持久化（QQ 要求单调递增，重启从盘恢复，归 1 可能被拒）
- `channels/qq/bridge.lock`：单实例锁（O_EXCL，防双桥双份转发/双份入队；内容 = 持锁 pid）

⚠️ **data_dir 在启动时算定**：移动本目录后，存活进程仍写旧绝对路径（2026-09-21 重排插件目录后数据落回 OkHuman_p 根目录就是它）。挪目录必做三件事：杀旧桥 → 迁移 data（token/msg_seq 取新的）→ 新路径重拉。

## 目录

```
okhuman-qq-bridge        可执行文件（gitignore，go build 产物）
okhuman-qq-bridge.go     全部源码（单文件）
config.json              运行配置（gitignore，真实凭据）
config.json.example      配置模板
data/                    运行时数据（token / msg_seq / 锁）
archive/                 迁移前日志存档（gitignore）
LESSONS.md               踩坑档案（token 解析 / 忘重编 / 挪目录数据落旧路径 / 日志迁 /tmp）
```

## 排障

1. **"响应无 access_token"**：先对三个时间——源码 mtime、二进制 mtime、日志里 bug 出现的时间；二进制比源码旧 = 没重编（09-20 案例）。二进制是新的仍报错 = 看 QQ 原始响应体（别信错误信息，09-19 案例实际是 `expires_in` 类型不符）。详见 LESSONS.md
2. **数据写到旧路径**（挪目录后）：见上文 ⚠️——杀进程、迁数据、新路径重拉；`readlink /proc/<pid>/cwd` 可确认进程实际位置
3. **双份转发 / 重复入队**：bridge.lock 被残留进程占着（`cat` 锁文件看 pid），杀掉后再启动
4. **消息被 QQ 拒**：msg_seq 回退——确认 `data/channels/qq/msg_seq` 存在且比上次发送值大
5. **QQ 发了没反应**：看 /tmp/okhuman-qq-bridge.log 有没有"已订阅 /events"（每个 bot×实例一条）；再手动 `curl -X POST {实例}/enqueue` 验证实例可达

## 手动媒体投递（outbox，2026-09-24）

正文 `[File: <路径>]` 标记自动发文件已整块删除（2026-09-24：纯副作用——思考段引用
路径会误发，当天一图发 3 次）。发文件一律走 outbox 显式投递：

```bash
cp <文件> "<本目录>/data/channels/qq/outbox/<botAppID>/"
# botAppID 见 config.json bots[].app_id（即 bots[].app_id）
```

放进目录 = 向该 bot 的 c2c 渠道发一条媒体消息（桥 2s 轮询拾取）。成功 → 归档
`outbox/<botAppID>/sent/<文件>.<unix秒>`；失败（token 不可用/上传/发送报错）→ 留在原位
下轮重试。图片扩展名自动识别为图片消息，其它为文件消息。

历史坑（09-24 修）：① QQ `/v2/users/{openid}/files` 返回的 `file_info` 是 **base64 字符串**
不是对象，按 map 解析恒 nil → “缺 file_info”；② 文件消息 `msg_seq` 原用 WS 会话 seq
（`c.seq`），与文本消息的 `c2cMsgSeq` 不同源，回退被 QQ 拒 → 统一 `nextC2CSeq()`。

## 解耦契约自检

- [x] 独立进程、独立二进制，不 import 任何 OkHuman 模块
- [x] 与主进程仅 HTTP 三条线：`POST /enqueue`、`GET /events`（SSE）、`GET /history`（兜底轮询）
- [x] 数据全在本目录 `data/`，不写主进程数据目录
- [x] 挂掉只断 QQ 渠道，主进程与其他插件不受影响
