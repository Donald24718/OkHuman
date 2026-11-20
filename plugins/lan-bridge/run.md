# lan-bridge — 构建、启动、配置

主程序**只监听 127.0.0.1 且完全没有鉴权**：把 `server.host` 改成 `0.0.0.0` 就等于把 `/history`（会话全文）、`/enqueue`（指挥 agent）、`/config` 白送给同网段任何人。本插件是**唯一**监听局域网的东西：主程序继续只听 localhost、一行代码不改；鉴权只在这一处；能走哪些端点由白名单决定（默认拒绝）；浏览器只跟桥一个 origin 说话，桥在 localhost 侧切实例（这正是 okmon 内嵌 iframe 做不到的——实例不发 `Access-Control-Allow-Origin`）。

## 构建 / 启动 / 停止

```bash
cd "…/Frequently Used Plugins/lan-bridge"
GOPROXY=off GOSUMDB=off go build -o okhuman-lan-bridge .
./okhuman-lan-bridge "$PWD"                                   # 前台
systemctl --user restart lan-bridge-8602                                # 常态：持久 unit 托管（enabled，开机自启）
systemctl --user stop lan-bridge-8602                                 # 停止（unit 名下别 pkill）
```

参数 = 插件目录（config.json 所在处）。

## 配置（`config.json`，权限 600）

| 键 | 含义 |
|---|---|
| `listen.host` | 默认 `0.0.0.0`（对外）。只想本机验证就填 `127.0.0.1` |
| `listen.port` | 默认 `8602` |
| `token` | **留空则首次启动自动生成 32 位十六进制（128 bit）并写回本文件**；日志里**不**打印 token。改 token = 手机上的 cookie 立刻失效，要重新登录 |
| `instances[]` | `{id,url}` 列表，url 必须是 localhost（桥不转发任意上游） |
| `default_instance` | 只用于两种回落：单上游的桥、人的落地页（多上游的机器请求未点名一律 400） |
| `allow_config_write` | 默认 `false`：手机上不能 `POST /config` 改实例配置 |

## 失败与限制

- **改 `config.json`（`instances[]`/`token` 等）后必须重启桥**，没有热加载。按端口取 pid：`ss -ltnp | awk '/:8602 /'`；❌ 别 `pgrep -f lan-bridge`（会打到其它实例的桥）。
- 本机 = 持久 user 单元 **`lan-bridge-8602`**（2026-11-19 起，enabled，`Restart=on-failure` 5s；日志 append 到 `~/logs/lan-bridge-8602.log`）；改过 config → `systemctl --user restart lan-bridge-8602`。❌ 别用 `setsid` 起桥、也别手工另起一个：setsid 不脱 systemd cgroup，本实例单元被 restart 时桥被连坐杀（11-05 实测），手工起的还会和 unit 抢 8602。（`.3` 上那两个 `lan-bridge-8451/8452` 是 **ey 的** 单元，与本机无关，见 `lan-ssh/ey-instances.md`。）
