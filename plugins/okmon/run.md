# okmon — 启动、API、配置与服务表

三块能力：① 显卡使用（nvidia-smi 每卡利用率/显存/温度/功耗 + 显存占用进程，自动标注对应 OkHuman 服务）；② OkHuman 系 Go 常驻进程的状态/内存/CPU/运行时长，webui 里一键启停；③ 实例界面入口（iframe 内嵌各实例自己的 webui）；④ 自动追踪配置表之外的一切 Go 版 okhuman 进程（comm 精确 `okhuman`，标注"自动追踪"，可停无启动按钮，进程退出后自动消失）。Go 单二进制（`main.go`），无外部依赖，挂掉不影响主进程。

## 启动 / 停止

```bash
cd "<本目录>"
setsid ./okmon </dev/null > /tmp/okmon.log 2>&1 &     # 停止：pkill -x okmon
```

- webui：`http://127.0.0.1:8496/`（2 秒自动刷新）
- API：`GET /api/state`（全量状态 JSON，okhuman 实例条目带 `instance: true`）、`POST /api/services/start`、`POST /api/services/stop`（body `{"name":"<服务名>"}`）

## 配置（config.json）

```json
{ "port": 8496,
  "services": [
    { "name": "okhuman-8451", "label": "小E·生产", "port": 8451, "start": "./okhuman",
      "workdir": "~/hdd/OkHuman" },
    { "name": "attach", "label": "图片/视频注入", "cli": true, "match": "okattach" }
  ] }
```

- `port` > 0：按**监听端口**定位进程（最可靠，okhuman 实例必须用这个——两个实例 comm 同名）。
- 无 `port` 时用 `match`：≤15 字符且不含 `/`、空格 → `pgrep -x`（comm 精确名）；否则 `pgrep -f`（cmdline 模式，慎用，会误中路径里含该串的其他进程）。
- `start`：相对 `workdir` 的启动命令，okmon 以 `cd workdir && setsid <start>` 完全脱离方式拉起，日志进 `/tmp/okmon-<name>.log`；启动后轮询 12 秒确认进程/端口出现才报成功。
- `cli: true`：纯 CLI（无常驻进程），webui 不给开关按钮。
- **改 config.json 后需重启 okmon 生效**（`pkill -x okmon` 再拉）。
- 自动追踪条目名形如 `okhuman-extra-<pid>`，webui 停止按钮直接按该名调 `POST /api/services/stop`；无启动按钮（没有配置的 workdir）。

## 服务表

**权威是本目录 `config.json`**（改它即改行为，重启生效）。下表是 2026-10-28 该文件的实际内容，只为快速对照，不一致时以 config 为准：

| name | 定位 | 有启动按钮 |
|---|---|---|
| okhuman-8451 | 端口 8451，workdir `~/hdd/OkHuman`（小E，生产） | 是 |
| okhuman-8452 | 端口 8452，workdir `~/hdd/OkHuman-8452`（小C，副本） | 是 |
| qq-channel | comm `okhuman-qq-brid`，无监听端口 | 是 |
| browser | 端口 8702（`browserd 8702 --keep`） | 是（按需起，平时不在跑） |
| cron | 端口 8601（`okhuman-cron`） | 是（按需起） |
| scout | 端口 8480 | 是 |
| okhuman-lan-bridge | 端口 8602（手机侧，见 lan-bridge 插件） | 是（按需起） |
| attach / computer | 纯 CLI（`cli: true`），无常驻进程 | 否 |
| （自动）其他 okhuman | comm 精确 `okhuman` 的一切进程，自动追踪、可停不可起 | 否 |

不在表里的服务（asr :8090、tts :8092、voice-chat、ComfyUI :8188、推理后端 :8080）okmon 不管理，只在显卡面板里以进程名出现。
