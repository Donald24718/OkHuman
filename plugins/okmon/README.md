# okmon —— OkHuman 系进程监控

三块能力：
1. **显卡使用**：nvidia-smi 每卡利用率/显存/温度/功耗 + 显存占用进程（自动标注对应 OkHuman 服务）。
2. **OkHuman 系 Go 常驻进程**：okhuman 实例（8451/8452）+ Go 插件（qq-channel/browser/cron…），状态/内存/CPU/运行时长，**webui 里一键启动/停止**。
3. **实例界面入口（2026-10-22 加）**：webui 顶部一排按钮，自动列出全部运行中的 okhuman 实例，
   点开就在同页 iframe 里加载**该实例自己的 webui** —— 不是复制品，功能 100% 等价，
   OkHuman 前端更新这里自动跟着更新。选择记在 localStorage，实例掉了自动切到别的。
4. **自动追踪一切 Go 版 okhuman 进程**：配置表之外出现的 okhuman 进程（如一次性实例，comm 精确 `okhuman`）自动出现在进程表（标注"自动追踪"，带 pid/端口/内存/CPU），**可停止、无启动按钮**；进程退出后自动消失。

Go 单二进制（`main.go`），无外部依赖（标准库），挂掉不影响主进程。

## 启动

```bash
cd "plugins/okmon"
setsid ./okmon </dev/null > /tmp/okmon.log 2>&1 &
```

- webui：http://127.0.0.1:8496/（2 秒自动刷新）
- API：`GET /api/state`（全量状态 JSON，okhuman 实例条目带 `instance: true`）、`POST /api/services/start`、`POST /api/services/stop`（body `{"name":"<服务名>"}`）

## 配置（config.json）

```json
{ "port": 8496,
  "services": [
    { "name": "okhuman-8451", "label": "生产实例", "port": 8451, "start": "./okhuman",
      "workdir": "." },
    { "name": "attach", "label": "图片/视频注入", "cli": true, "match": "okattach" }
  ] }
```

- `port` > 0：按**监听端口**定位进程（最可靠，okhuman 实例必须用这个——两个实例 comm 同名）。
- 无 `port` 时用 `match`：≤15 字符且不含 `/`、空格 → `pgrep -x`（comm 精确名）；否则 `pgrep -f`（cmdline 模式，慎用，会误中路径里含该串的其他进程）。
- `start`：相对 `workdir` 的启动命令，okmon 以 `cd workdir && setsid <start>` 完全脱离方式拉起，日志进 `/tmp/okmon-<name>.log`；启动后轮询 12 秒确认进程/端口出现才报成功。
- `cli: true`：纯 CLI（无常驻进程），webui 不给开关按钮。
- 改 config.json 后需重启 okmon 生效（`pkill -x okmon` 再拉）。
- 自动追踪条目名形如 `okhuman-extra-<pid>`，webui 停止按钮直接按该名调 `POST /api/services/stop`；无启动按钮（没有配置的 workdir）。

## 服务表

**权威是本目录 `config.json`**（改它即改行为，重启 okmon 生效）。下表是 2026-10-28 该文件的实际内容，只为快速对照，不一致时以 config 为准：

| name | 定位 | 有启动按钮 |
|---|---|---|
| okhuman-8451 | 端口 8451，workdir `.`（生产实例） | 是 |
| okhuman-8452 | 端口 8452，workdir `./okhuman-8452`（副本实例） | 是 |
| qq-channel | comm `okhuman-qq-brid`，无监听端口 | 是 |
| browser | 端口 8702（`browserd 8702 --keep`） | 是（按需起，平时不在跑） |
| cron | 端口 8601（`okhuman-cron`） | 是（按需起） |
| scout | 端口 8480 | 是 |
| okhuman-lan-bridge | 端口 8602（手机侧，见 lan-bridge 插件） | 是（按需起） |
| attach / computer | 纯 CLI（`cli: true`），无常驻进程 | 否 |
| （自动）其他 okhuman | comm 精确 `okhuman` 的一切进程，自动追踪、可停不可起 | 否 |

不在表里的服务（asr :8090、tts :8092、voice-chat、ComfyUI :8188、推理后端 :8080）okmon 不管理，只在显卡面板里以进程名出现。

## 实例界面的边界（诚实列）

- **一次只看一个实例**（切 iframe 的 src）。**没有**跨实例统一视图/统一搜索/群发——
  那要在 okmon 侧做代理聚合：浏览器跨端口 `fetch` 会被 CORS 挡，因为 **OkHuman 实例
  不发 `Access-Control-Allow-Origin`**（实测）。想做就走 okmon 后端代理，别在前端硬连。
- 内嵌能成立的前提是实例**不发** `X-Frame-Options` / `CSP frame-ancestors`（实测没有），
  且实例 UI 没有 framebusting JS（实测没有）。**OkHuman 哪天加上这些头，这里就失效。**
- 只有**有监听端口**的实例才出现在按钮里（纯 CLI 的没有界面可嵌）。
- 切换实例会重载该页（71KB HTML + 40KB marked.min.js，本机无感）。
- 2 秒自动刷新**不会**重载 iframe（`dataset.port` 守卫）——否则界面每 2 秒闪一次，没法用。

## 改 webui.html 必须重编

`webui.html` 是 `//go:embed` 烘进二进制的，**不是**运行时读盘。改完 HTML 不重编，
页面还是旧的（本次就踩了：改完 HTML 只重启进程，浏览器里 `#insts` 根本不存在）。
正确顺序：改 → `go build -o okmon .` → 重启 → 浏览器实测。

## 排障

- **端口冲突**：okmon 启动日志（/tmp/okmon.log）会报；换 config.json 的 port。
- **启动报"12 秒内没起来"**：看它附带的 `/tmp/okmon-<name>.log` 尾 3 行；常见是该服务本身不是常驻（打印用法就退出）→ 改成 `cli: true`。
- **启动按钮点了没反应 / 报"12 秒内没起来"且日志空**：workdir 含空格（插件在 `Frequently Used Plugins/` 下）——
  启动脚本必须对 workdir 与日志路径单引号包裹（`shellQuote`）。2026-10-04 修：未引号时
  `cd <带空格路径> && ...` 报 `cd: 参数太多`，`&&` 短路 → 服务根本没拉起，且报错落在 okmon
  自己的 stderr 而非 `/tmp/okmon-<name>.log`，所以日志看着是空的。09-21 插件目录重排后
  qq-channel/browser/cron/scout 的启动按钮其实一直失效，只是此前都是手工启动没暴露。
- **stop 误杀**：定位端口优先（okhuman 实例只按端口找），不会误伤 cmdline 里含 "okhuman" 的路径进程。
- **nvidia-smi 缺失/失败**：显卡面板为空，进程面板不受影响。
