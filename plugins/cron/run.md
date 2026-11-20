# cron — 起停、重建、配置与数据

```bash
cd "<本目录>" && (setsid ./okhuman-cron < /dev/null > /tmp/okhuman-cron.log 2>&1 &)
curl -s http://127.0.0.1:8601/health          # 探活（列各实例 + 任务数）
pkill -x okhuman-cron                          # 停止
go build -o okhuman-cron .                     # 改完源码必须重编（改完源码 = 没干活）
```

也可由 okmon（:8496）一键启停，日志落 `/tmp/okmon-cron.log`。

## 配置（本目录 config.json，gitignore；模板 config.json.example）

`server.host/port`（8601）、`tick_ms`、`catchup_max`、`data_dir`（相对路径按可执行文件所在目录解析）、`instances[]`（必填，缺/空/id 重复 → 启动即 fatalf）。

## 数据（本目录 data/，gitignore）

`data/<inst-id>/cron.json`：该实例全部任务，实时原子写（tmp+rename）。运行记录（`recent_runs`，近 20 条）**随 cron.json 一起持久化**，重启不丢（旧记录里的 `reply_chars` 字段已废弃，读回时按 0 处理，不会因此丢记录）。

⚠️ `data_dir` 在启动时算定：挪本目录后存活进程仍写旧绝对路径 → 杀进程、迁 data、新路径重拉。
