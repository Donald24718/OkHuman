# cron — 排障

- **到点没跑**：`GET /cron?inst=<id>` 看 `next_run_at` 是否在未来、`paused` 是否为 true；再看日志。
- **入队失败**：日志报 `实例 <id> /enqueue 无响应（10s）` = 该实例 HTTP 层卡死；`连接失败` = 实例没起或 url 配错。注意**入队成功不等于那一轮跑完**——插件不等它。
- **管理页 503**：缺 `webui/index.html`。
- **端口冲突**：改 config.json 的 `server.port`（okmon 服务表里同步改）。
