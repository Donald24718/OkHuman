# cron — 管理 API（:8601）

| 端点 | 方法 | 参数 | 说明 |
|---|---|---|---|
| `/health` | GET | — | `{ok, instances:[{id,url,jobs}]}` |
| `/instances` | GET | — | 配置的实例列表 |
| `/cron` | GET | `?inst=<id>` | 该实例任务列表 + `recent_runs`（近 20 次运行记录） |
| `/cron/add` | POST | `{inst, prompt, name?, every_ms? \| at?}` | 循环（`every_ms`）或单次（`at`，unix 毫秒）二选一 |
| `/cron/remove` | POST | `{inst, id}` | 删任务 |
| `/cron/pause` | POST | `{inst, id}` | 暂停（循环任务置 `next_run_at=0`） |
| `/cron/resume` | POST | `{inst, id}` | 恢复（单次任务返回 404） |
| `/cron/run` | POST | `{inst, id}` | 立即入队一次，返回入队回执（不含 reply） |
| `/` | GET | — | 独立管理页（`webui/index.html`） |

任务视图字段：`id/name/prompt/kind(every|once)/every_ms/at/next_run_at/next_run_iso/last_run_at/paused/catchup/created_at`。`paused` = 循环任务且 `next_run_at==0`。
