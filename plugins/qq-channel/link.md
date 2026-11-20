# qq-channel — 链路与解耦契约

把 OkHuman 实例接入 QQ 的**独立常驻进程**（Go，TS 版 bridge.ts + qq.ts 的 1:1 移植）：QQ 消息进 agent、agent 回复回 QQ。**无监听端口**，只有出站连接（QQ 网关 WebSocket + 实例 HTTP/SSE）。

## 上行

QQ 消息 → `POST {实例}/enqueue { message }`（入队即返，不等回合结束）。

## 下行

桥常驻订阅 `GET {实例}/events`（SSE 广播）→ 按事件边界**整段**拆气泡发：

- thinking 整段 ≤2950 字（`[思考]` 前缀）；text 整段 ≤3000 字
- tool_call → `[工具]`、tool_result → `[结果]`
- finish → flushAll + reply 兜底；error → `[出错]`
- 轮内 60s 静默 → 15s 轮询 `/history` 补发回尾 → 10min 放弃

## 多 bot、重连、收尾

- `bots[]` 每项绑定一个实例（`instance` 字段），限流 `max_msgs_per_min`（默认 20）
- 重连自愈：QQChannel 内部自动重连/重认证（heartbeat 30s / 重连 3s / 重认证 30s）
- SIGINT 优雅关闭所有 WS；SIGTERM 立即退出

## 解耦契约（改代码前自检）

- [x] 独立进程、独立二进制，不 import 任何 OkHuman 模块
- [x] 与主进程仅 HTTP 三条线：`POST /enqueue`、`GET /events`（SSE）、`GET /history`（兜底轮询）
- [x] 数据全在本目录 `data/`，不写主进程数据目录
- [x] 挂掉只断 QQ 渠道，主进程与其他插件不受影响
