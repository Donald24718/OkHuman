# qq-channel — 起停、重建、配置与运行时数据

## 启动 / 停止 / 重建

```bash
# 本机实际跑法：systemd user 服务（日志在 journal，不在 /tmp）
systemctl --user restart qq-bridge.service ; journalctl --user -u qq-bridge.service -n 80
# 临时起一个测试 = 接管租约，正在跑的那个让位退 0 且不会被自动拉起（11-11 踩过一次）→ 测完要 restart 交回
cd "<本目录>" && ./okhuman-qq-bridge
# 改完源码后重建（Go 工具链 /usr/local/go，go 在 /usr/local/bin）
cd "<本目录>" && go build -o okhuman-qq-bridge okhuman-qq-bridge.go
```

**改完源码 = 没干活，重编 + 真实启动验证 = 才干完**（2026-09-20 老 bug 复发的根因就是忘了 go build，详见 `LESSONS.md`）。

## 配置（本目录 config.json，config.json.example 为模板）

- `data_dir`：运行时数据目录。**相对路径按可执行文件所在目录解析**（`pluginRoot()`：exe 目录里有 config.json 即用之，否则回退 `.`）；绝对路径直接用
- `instances[]`：要接入的 OkHuman 实例（`{ id, url }`），id 不可重复
- `bots[]`：QQ 开放平台凭据 + 绑定：`app_id` / `client_secret` / `channel_id`（c2c 形如 `c2c:<openid>`）/ `instance` / `max_msgs_per_min`
- `markdown_enabled`：markdown 消息（md=2）开关
- `client_secret` 不入日志；config.json 已 gitignore（真实凭据不入库）

## 数据与状态（本目录 data/，已 gitignore）

- `channels/qq/tokens/<botId>.json`：access_token 盘缓存（原子写；QQ 返回的 `expires_in` 兼容字符串/数字两种形态，`json.Number` 解析——2026-09-19 修的移植 bug）
- `channels/qq/msg_seq`：c2c 消息序号持久化（QQ 要求单调递增，重启从盘恢复，归 1 可能被拒）
- `channels/qq/bridge.gen`：单实例**让位令牌**（2026-11-08 起取代 bridge.lock；内容 = 当前持有者的随机令牌，不含 PID）。新桥写它即接管，旧桥 2s 内自检令牌不是自己就退 0 —— **再起一个不必先杀旧的**，也不会再有陈旧锁挡门

## 目录

```
okhuman-qq-bridge        可执行文件（gitignore，go build 产物）
okhuman-qq-bridge.go     全部源码（单文件）
config.json / config.json.example
data/                    运行时数据（token / msg_seq / 让位令牌）
archive/                 迁移前日志存档（gitignore）
```

## 失败与限制

- ⚠️ **data_dir 在启动时算定**：移动本目录后，存活进程仍写旧绝对路径（2026-09-21 重排插件目录后数据落回 plugins 根目录就是它）。挪目录必做三件事：杀旧桥 → 迁移 data（token/msg_seq 取新的）→ 新路径重拉。`readlink /proc/<pid>/cwd` 可确认进程实际位置。
