# okmon —— OkHuman 系进程监控

三块能力：显卡使用（nvidia-smi）、OkHuman 系 Go 常驻进程状态与一键启停、实例界面入口（iframe 内嵌各实例自己的 webui）、自动追踪一切 Go 版 okhuman 进程。Go 单二进制，无外部依赖，挂掉不影响主进程。**本文件不含端口、参数与配置键。**

## 要做什么 → 去哪个文件

| 要做 | 文件 |
|---|---|
| 起停 okmon、调 API、改 config.json（端口/match 定位规则/start/workdir/cli）、看当前管哪些服务 | `run.md` |
| 用 okmon 打开某个实例的界面；能不能做跨实例统一视图 | `embed.md` |
| 排障：端口冲突、起不来、按钮没反应、stop 误杀、显卡面板空、改了页面没变 | `trouble.md` |

## 遇到现象 → 去哪个文件

| 现象 | 文件 |
|---|---|
| 启动按钮点了没反应、报"12 秒内没起来"、日志是空的 | `trouble.md`（workdir 含空格） |
| 改 config 不生效 | `run.md`（要 `pkill -x okmon` 再拉） |
| 改了 webui.html 页面还是旧的 | `trouble.md`（go:embed，必须重编） |
| 想在一个页面里看两个实例 / 跨实例聚合 | `embed.md`（做不到，走后端代理） |
