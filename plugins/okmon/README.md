# okmon —— OkHuman 系进程监控

两块能力：
1. **显卡使用**：nvidia-smi 每卡利用率/显存/温度/功耗 + 显存占用进程（自动标注对应 OkHuman 服务）。
2. **OkHuman 系 Go 常驻进程**：okhuman 实例（8451/8452）+ Go 插件（qq-channel/browser/cron…），状态/内存/CPU/运行时长，**webui 里一键启动/停止**。
3. **自动追踪一切 Go 版 okhuman 进程**：配置表之外出现的 okhuman 进程（如一次性实例，comm 精确 `okhuman`）自动出现在进程表（标注"自动追踪"，带 pid/端口/内存/CPU），**可停止、无启动按钮**；进程退出后自动消失。

Go 单二进制（`main.go`），无外部依赖（标准库），挂掉不影响主进程。

## 启动

```bash
cd plugins/okmon
setsid ./okmon </dev/null > /tmp/okmon.log 2>&1 &
```

- webui：http://127.0.0.1:8496/（2 秒自动刷新）
- API：`GET /api/state`（全量状态 JSON）、`POST /api/services/start`、`POST /api/services/stop`（body `{"name":"<服务名>"}`）

## 配置（config.json）

```json
{ "port": 8496,
  "services": [
    { "name": "okhuman-8451", "label": "小K·生产", "port": 8451, "start": "./okhuman", "workdir": "<副本目录>/OkHuman" },
    { "name": "attach", "label": "图片/视频注入", "cli": true }
  ] }
```

- `port` > 0：按**监听端口**定位进程（最可靠，okhuman 实例必须用这个——两个实例 comm 同名）。
- 无 `port` 时用 `match`：≤15 字符且不含 `/`、空格 → `pgrep -x`（comm 精确名）；否则 `pgrep -f`（cmdline 模式，慎用，会误中路径里含该串的其他进程）。
- `start`：相对 `workdir` 的启动命令，okmon 以 `cd workdir && setsid <start>` 完全脱离方式拉起，日志进 `/tmp/okmon-<name>.log`；启动后轮询 12 秒确认进程/端口出现才报成功。
- `cli: true`：纯 CLI（无常驻进程），webui 不给开关按钮。
- 改 config.json 后需重启 okmon 生效（`pkill -x okmon` 再拉）。
- 自动追踪条目名形如 `okhuman-extra-<pid>`，webui 停止按钮直接按该名调 `POST /api/services/stop`；无启动按钮（没有配置的 workdir）。

## 当前服务表（2026-09-26）

| name | 说明 | 定位 |
|---|---|---|
| okhuman-8451 | 小K·生产 | 端口 8451 |
| okhuman-8452 | 副本实例·副本 | 端口 8452 |
| （自动）其他 okhuman | 一切 Go 版 okhuman 进程（一次性实例等） | comm `okhuman` 全量扫描，自动追踪 |
| qq-channel | QQ 频道 | comm `okhuman-qq-brid`（无监听端口） |
| attach | 图片/视频注入 | CLI |
| browser | 无头浏览器 | 端口 8702（`browserd 8702 --keep`） |
| cron | 定时任务 | 端口 8601 |
| computer | 桌面操作 xusectl | CLI |
| scout | 语义检索 | 端口 8480 |

## 排障

- **端口冲突**：okmon 启动日志（/tmp/okmon.log）会报；换 config.json 的 port。
- **启动报"12 秒内没起来"**：看它附带的 `/tmp/okmon-<name>.log` 尾 3 行；常见是该服务本身不是常驻（打印用法就退出）→ 改成 `cli: true`。
- **stop 误杀**：定位端口优先（okhuman 实例只按端口找），不会误伤 cmdline 里含 "okhuman" 的路径进程。
- **nvidia-smi 缺失/失败**：显卡面板为空，进程面板不受影响。
