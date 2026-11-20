# browser — 起实例、生命周期、画像与登录态

## 起停与生命周期

```bash
# 起一个实例（端口自定；约定 87xx，避开 845x=OkHuman 实例、8601=cron 管理页），后台跑
(setsid "plugins/browser/browserd" 8701 </dev/null >/tmp/browser-8701.log 2>&1 &)
# 持久实例（不自动关）
(setsid ".../browserd" 8702 --keep </dev/null >/tmp/browser-8702.log 2>&1 &)
# 查状态 / 用完关闭（browserd 5s 内退出并清理）
".../browserctl" 8701 status
".../browserctl" 8701 close
```

- **默认无头**；**默认 600s 内没有任何 browserctl 动作命令就自动关闭**（`--idle <sec>` 自定义；`--keep` 常驻直到显式 close/kill）。每个动作命令会刷心跳。
- 单实例单进程，不常驻全局——要几个实例起几个，各自独立。
- **`--headed` 不可用**：chromium 起来了但 CDP 8s 未就绪即放弃。要真实窗口：手工起 chromium（`DISPLAY=:0` + 自建 `--user-data-dir`），真实鼠标走 computer 插件。

## 共享实例（多 agent 共用一份登录态）

默认每个实例"即用即焚"：全新 profile，实例退出登录态即灭。要共用一份登录状态：

- `browserd <port> --shared`——端口自定（87xx）。共享实例**持久**（不空闲自动关）：登录态活在运行的进程里（内存 + 其 /tmp profile 目录），**进程被杀才灭**；重启机器 /tmp 清空，语义相同。
- **全局至多一个**：端口写在指针文件 `/tmp/okhuman-browser-shared.port`（O_EXCL 互斥），再开第二个会快速失败并提示用已存在的那个。
- 共享实例上的 `browserctl <port> close` 是**软关闭**：只关 tab（保留 1 个页面——页面归零会触发 chromium 清 session cookie），进程继续活、登录态保留。清掉全部状态：`kill <daemon_pid>`（daemon_pid 在 `/tmp/okhuman-browser-<port>.state` 里）。
- 临时实例与共享实例可并存，行为不变。

agent 找/起共享实例：

```bash
SHARED_PORT=$(cat /tmp/okhuman-browser-shared.port 2>/dev/null)
if [ -z "$SHARED_PORT" ]; then
  (setsid ".../browserd" 8731 --shared </dev/null >/tmp/browser-8731.log 2>&1 &)
  sleep 3   # 等 CDP 就绪
  SHARED_PORT=8731
fi
".../browserctl" "$SHARED_PORT" open https://example.com
```

**多 agent 同实例的 tab 隔离**：调 browserctl 时设 `OKH_BROWSER_AGENT=<自己的实例id>`（限字母数字 _ -），"当前活跃 tab"记录按 agent 分文件，互不串；建议各 agent 用 `new` 拿自己的 tab、用 `tab <index>` / `close_tab <index>` 显式操作。

## 登录态放哪儿：`--shared` vs `--data-dir`

| 需求 | 用什么 | 登录态活多久 |
|---|---|---|
| 多个 agent 共用一份、当次开机内复用 | `--shared` | 活在**运行的进程**里；杀进程 / 重启机器即灭 |
| 扫码登录一次，**跨重启 / 重开机**长期复用 | `--data-dir <dir>` | 画像落盘到该目录；cleanup 只删临时目录，**持久目录保留** |

两者可叠加（`--shared --data-dir <dir>`）。`--data-dir` 的目录里就是账号凭据：**放在插件目录之外**（别在 `plugins/` 里，否则会被 okp-sync 镜像带走、也可能被 scout 索引），权限 600，不进 git。

扫码登录一次、之后长期用（B 站为例，完整流程见 bilibili `SKILL.md` §1 路线 A）：

```bash
PROF=~/browser-profiles/bili; mkdir -p "$PROF"; chmod 700 "$PROF"
(setsid ".../browserd" 8751 --data-dir "$PROF" --idle 3600 </dev/null >/tmp/browser-8751.log 2>&1 &)
sleep 3
".../browserctl" 8751 open https://passport.bilibili.com/login
".../browserctl" 8751 screenshot --out /tmp/bili-qr.png   # okattach 发给主人扫码
".../browserctl" 8751 cookies bilibili                    # 扫完导出含 SESSDATA 的 Cookie 头
```

## 架构与状态文件

```
agent ──bash──> browserctl ──WS/CDP──> chromium(headless, :port)
                      │ 刷心跳
                      ▼
                 browserd（守护：空闲 600s 杀 chromium 退出 / --keep 持久）
```

- **browserd**：起 chromium（CDP 监听指定端口）、写心跳文件、空闲超时看门狗。
- **browserctl**：动作 CLI，每次短连接 CDP 执行一个动作后退出，每个动作刷心跳。
- **状态文件**（/tmp，进程生命周期同义，重启清空）：`okhuman-browser-<port>.hb`（心跳，mtime=最后活动时间）、`.state`（daemon_pid/chrome_pid/keep/started_at）、`.active[-<agent>]`（活跃 tab targetId）、`.taborder`（tab index 持久化顺序）、`.stop`（close 信号）。

## 依赖

- Go（`cmd/browserctl`、`cmd/browserd` 两个独立二进制；依赖仅 gorilla/websocket）
- 系统 chromium（默认 `/usr/bin/chromium`，`OKH_BROWSER_CHROME` 环境变量可覆盖）

## 失败与限制

- **路径含空格**（`Frequently Used Plugins`）——命令里一律带引号。不加引号的 `cd %s && setsid %s` 这类拼接脚本会在含空格目录上短路（okmon 起服务 2026-09-21 起一直失效即此因，已修）。
- 实例会自动关（默认空闲 600s）→ 隔了一会儿再用，先 `status` 确认还在，不在就重起（临时实例的 profile 也随之清）。
- 状态文件在 /tmp：重启机器后端口指针、登录态都没了，要重建。
