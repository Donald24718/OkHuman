# cron/ — 定时任务插件（okhuman-cron）

给 OkHuman 实例排定时任务的**独立常驻进程**（Go 单文件 `main.go`，标准库无第三方依赖）：
到点把任务 prompt 通过 `POST /enqueue` 投进该实例的消息队列，**只管发、不等那一轮跑完**
（2026-10-22 改）。插件拿到入队位置就返回；要看结果去实例自己的界面看。
每个实例独立维护自己的 CronStore，与主程序零代码耦合。

## 链路

- 调度循环 `tick_ms`（默认 1000ms）扫各实例 store，`next_run_at` 到期 → 触发
- 触发 = `POST {实例url}/enqueue { message: <prompt> }`，返回 `{position}` 即结束。
  入队是瞬时动作，HTTP 超时固定 10s（只防实例整个 HTTP 层卡死，**不是**等它跑完）
- **为什么不等**：旧的 `/chat` 同步等整轮，带来三个坑——① 插件的 goroutine 自旋挂到 600s、
  同实例其它任务被串行化；② "超时但其实跑了"的误判（run 绑 `context.Background()`，
  客户端断开不会取消那一轮，插件却已记成失败）；③ 等待窗口=整个 run，期间点的"暂停"
  会被跑完时的重排抹掉。只管发把这三条一起去掉了
- **代价**：插件拿不到 reply。`recent_runs` 里记的是入队位置，不是回复字数
- **重启补跑**：启动时 `rescheduleOnBoot` 检测错过的周期，`missed` 个只补 1 次，
  补跑次数上限 `catchup_max`（默认 3）；单次任务（`at`）过期同样补跑一次
- 多实例：`instances[]` 每项 `{ id, url }`，id 不可重复；任务按 `?inst=<id>` 分仓

## 启动 / 停止 / 重建

```bash
cd "<本目录>" && (setsid ./okhuman-cron < /dev/null > /tmp/okhuman-cron.log 2>&1 &)
curl -s http://127.0.0.1:8601/health          # 探活（列各实例 + 任务数）
pkill -x okhuman-cron                          # 停止
go build -o okhuman-cron .                     # 改完源码必须重编（改完源码 = 没干活）
```
也可由 okmon（:8496）一键启停，日志落 `/tmp/okmon-cron.log`。

## API（:8601）

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

任务视图字段：`id/name/prompt/kind(every|once)/every_ms/at/next_run_at/next_run_iso/last_run_at/paused/catchup/created_at`。
`paused` = 循环任务且 `next_run_at==0`。

## 配置（本目录 config.json，gitignore；模板 config.json.example）

`server.host/port`（8601）、`tick_ms`、`catchup_max`、
`data_dir`（相对路径按可执行文件所在目录解析）、`instances[]`（必填，缺/空/id 重复 → 启动即 fatalf）。

## 数据（本目录 data/，gitignore）

`data/<inst-id>/cron.json`：该实例全部任务，实时原子写（tmp+rename）。
运行记录（`recent_runs`，近 20 条）**随 cron.json 一起持久化**，重启不丢
（旧记录里的 `reply_chars` 字段已废弃，读回时按 0 处理，不会因此丢记录）。

⚠️ `data_dir` 在启动时算定：挪本目录后存活进程仍写旧绝对路径 → 杀进程、迁 data、新路径重拉。

## 排障

- **到点没跑**：`GET /cron?inst=<id>` 看 `next_run_at` 是否在未来、`paused` 是否为 true；再看日志
- **入队失败**：日志报 `实例 <id> /enqueue 无响应（10s）` = 该实例 HTTP 层卡死；
  `连接失败` = 实例没起或 url 配错。注意**入队成功不等于那一轮跑完**——插件不等它
- **管理页 503**：缺 `webui/index.html`
- **端口冲突**：改 config.json 的 `server.port`（okmon 服务表里同步改）

## 解耦契约自检

- [x] 独立进程、独立二进制，不 import 任何 OkHuman 模块
- [x] 与主进程只有一条线：`POST {实例}/chat`
- [x] 数据全在本目录 `data/`
- [x] 挂掉只断定时任务，主进程与其他插件不受影响
