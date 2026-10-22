# okmon —— OkHuman 系进程监控

两块能力：
1. **显卡使用**：nvidia-smi 每卡利用率/显存/温度/功耗 + 显存占用进程（自动标注对应 OkHuman 服务）。
2. **OkHuman 系 Go 常驻进程**：okhuman 实例（默认 8451，多实例各自端口）+ Go 插件（qq-channel/browser/cron…），状态/内存/CPU/运行时长，**webui 里一键启动/停止**。
3. **实例界面入口**：webui 顶部一排按钮，自动列出全部运行中的 okhuman 实例，点开就在同页
   iframe 里加载**该实例自己的 webui**——不是复制品，功能 100% 等价，主程序前端更新这里自动
   跟着更新。选择记在 localStorage，实例掉了自动切到别的。
3. **自动追踪一切 Go 版 okhuman 进程**：配置表之外出现的 okhuman 进程（如一次性实例，comm 精确 `okhuman`）自动出现在进程表（标注"自动追踪"，带 pid/端口/内存/CPU），**可停止、无启动按钮**；进程退出后自动消失。

Go 单二进制（`main.go`），无外部依赖（标准库），挂掉不影响主进程。

## 启动

```bash
cd plugins/okmon
setsid ./okmon </dev/null > /tmp/okmon.log 2>&1 &
```

- webui：http://127.0.0.1:8496/（2 秒自动刷新）
- API：`GET /api/state`（全量状态 JSON，okhuman 实例条目带 `instance: true`）、`POST /api/services/start`、`POST /api/services/stop`（body `{"name":"<服务名>"}`）

## 配置（config.json）

```json
{ "port": 8496,
  "services": [
    { "name": "okhuman-8451", "label": "主实例", "port": 8451, "start": "./okhuman", "workdir": "<副本目录>/OkHuman" },
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
| okhuman-8451 | 主实例 | 端口 8451 |
| okhuman-8455 | 第二实例 | 端口 8455 |
| （自动）其他 okhuman | 一切 Go 版 okhuman 进程（一次性实例等） | comm `okhuman` 全量扫描，自动追踪 |
| qq-channel | QQ 频道 | comm `okhuman-qq-brid`（无监听端口） |
| attach | 图片/视频注入 | CLI |
| browser | 无头浏览器 | 端口 8702（`browserd 8702 --keep`） |
| cron | 定时任务 | 端口 8601 |
| computer | 桌面操作 xusectl | CLI |
| scout | 语义检索 | 端口 8480 |

## 实例界面的边界（诚实列）

- **一次只看一个实例**（切 iframe 的 src）。**没有**跨实例统一视图/统一搜索/群发——那要在
  okmon 侧做代理聚合：浏览器跨端口 `fetch` 会被 CORS 挡，因为 **OkHuman 实例不发
  `Access-Control-Allow-Origin`**（实测）。想做就走 okmon 后端代理，别在前端硬连。
- 内嵌成立的前提是实例**不发** `X-Frame-Options` / `CSP frame-ancestors`（实测没有），且
  实例 UI 没有 framebusting JS（实测没有）。**主程序哪天加上这些头，这里就失效。**
- 只有**有监听端口**的实例才出现在按钮里（纯 CLI 的没有界面可嵌）。
- 2 秒自动刷新**不会**重载 iframe（`dataset.port` 守卫）——否则界面每 2 秒闪一次，没法用。

## 改 webui.html 必须重编

`webui.html` 是 `//go:embed` 烘进二进制的，**不是**运行时读盘。改完 HTML 不重编，页面还是旧的
（实测踩过：改完 HTML 只重启进程，浏览器里新加的 `#insts` 根本不存在）。正确顺序：
改 → `go build -o okmon .` → 重启 → 浏览器实测。

## 排障

- **端口冲突**：okmon 启动日志（/tmp/okmon.log）会报；换 config.json 的 port。
- **启动报"12 秒内没起来"**：看它附带的 `/tmp/okmon-<name>.log` 尾 3 行；常见是该服务本身不是常驻（打印用法就退出）→ 改成 `cli: true`。
- **stop 误杀**：定位端口优先（okhuman 实例只按端口找），不会误伤 cmdline 里含 "okhuman" 的路径进程。
- **nvidia-smi 缺失/失败**：显卡面板为空，进程面板不受影响。
