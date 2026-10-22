# lan-bridge — OkHuman 局域网桥（手机用 WebUI）

## 它解决什么

手机想在同一个局域网里用 OkHuman 的 WebUI。但主程序**只监听 127.0.0.1 且完全没有鉴权**：
把 `server.host` 改成 `0.0.0.0` 就等于把 `/history`（会话全文）、`/enqueue`（指挥 agent）、
`/config`（改配置）白送给同网段任何人。

本插件是**唯一**监听局域网的东西：

- 主程序继续只听 localhost，**一行代码都不用改**；
- 鉴权只在这一处（token → HttpOnly cookie，或 `Authorization: Bearer`）；
- 能走哪些端点由**白名单**决定，默认拒绝；
- 顺带解决跨实例：浏览器只跟桥一个 origin 说话，桥在 localhost 侧随便切实例
  （这正是 okmon 内嵌 iframe 做不到的——实例不发 `Access-Control-Allow-Origin`）。

手机侧不需要装任何东西：浏览器打开 `http://<PC-IP>:8602/` → 输 token → 就是实例自己的 WebUI。
（WebUI 全部用相对路径 `fetch`，静态资源也在根，所以**原样转发就能跑**，无需为手机改版。）

## 构建 / 启动

```bash
cd "…/Frequently Used Plugins/lan-bridge"
GOPROXY=off GOSUMDB=off go build -o okhuman-lan-bridge .
# 前台
./okhuman-lan-bridge "$PWD"
# 脱离启动（推荐；三件套缺一不可）
(setsid ./okhuman-lan-bridge "$PWD" < /dev/null > data/lan-bridge.log 2>&1 &)
```

参数 = 插件目录（config.json 所在处）。停止：`pkill -f okhuman-lan-bridge`。

## 配置（`config.json`，权限 600）

| 键 | 含义 |
|---|---|
| `listen.host` | 默认 `0.0.0.0`（对外）。只想本机验证就填 `127.0.0.1` |
| `listen.port` | 默认 `8602` |
| `token` | **留空则首次启动自动生成 32 位十六进制（128 bit）并写回本文件**；日志里**不**打印 token |
| `instances[]` | `{id,url}` 列表，url 必须是 localhost（桥不转发任意上游） |
| `default_instance` | 没有实例 cookie 时用哪个 |
| `allow_config_write` | 默认 `false`：手机上不能 `POST /config` 改实例配置 |

改 token = 手机上的 cookie 立刻失效，要重新登录。

## 鉴权流程

- `GET /login` → 表单页；`POST /login`（表单，token **不进 URL/历史**）→ 发 `okh_bridge` cookie（HttpOnly、SameSite=Lax、30 天）→ 302 到 `/`
- 也支持 `GET /login?t=…`（方便点链接，但 token 会进浏览器历史，能用表单就别用）
- 非浏览器客户端：`Authorization: Bearer <token>`
- 未登录访问任何转发端点 → 302 到 `/login`（上游一次都不会被碰）
- token 比较用 `subtle.ConstantTimeCompare`

## 端点白名单（默认拒绝，不在表里一律 403）

`GET`：`/` `/favicon.ico` `/marked.min.js` `/sanitize.js` `/health` `/status` `/queue`
`/history` `/config` `/prompts` `/attachments` `/events` `/events/poll`
`POST`：`/enqueue` `/stop` `/reset` `/compress` `/strip-injects` `/inject`
（`POST /config` 仅在 `allow_config_write=true` 时放行）

桥自己的端点（不转发、不含会话内容）：`GET /healthz`（不鉴权，给 okmon/排障）、
`GET /pick`（鉴权，列实例）、`GET /pick?inst=<id>`（设 cookie 后 302 回 `/`）。

## 切实例

`/pick` 列出配置里的实例，点一下设 `okh_inst` cookie 并回首页。认不出的 cookie 退回
`default_instance`，不报错。

## Origin 改写（手机上发消息的关键）

主程序对写请求有守卫：`Host` 与 `Origin` 的 host 必须是本机地址（挡 DNS rebinding 与
跨站页面）。浏览器发给桥的是 `http://<局域网IP>:8602`，原样转过去会被实例判
「Origin 不是本机同源」→ 手机上**所有发消息都失败**。

桥因此把 `Origin` 改写成上游自己的 origin（`http://127.0.0.1:8451`），`Host` 也走上游地址。
**守卫的威胁模型没有被削弱**：恶意跨站页面根本进不了桥（鉴权 cookie 是 `SameSite=Lax`，
跨站 POST 不会带上），进不了桥就轮不到它写实例。实测对照：同一带外 Origin 的请求
直连实例仍返回 403。客户端不带 Origin（curl/脚本/插件）时桥不伪造。

## 流式输出

`/events`（SSE）**逐块 flush、不缓冲**，并带 `X-Accel-Buffering: no`。手机断开只断这条连接——
实例侧 run 早就与 handler 解耦（run 只往 `state.Evt` 内存缓冲放帧），掉线缺口由 `/history` 兜底。

## 手机侧

1. 手机连与 PC 同一个 WiFi/AP；
2. 浏览器开 `http://<PC 在该网段的 IP>:8602/`（本机 IP 用 `ip -4 -br addr` 看）；
3. 输 token；
4. 「添加到主屏幕」就接近一个 app。

## 已知边界（不粉饰）

- **HTTP 明文**：同 Wi-Fi 的人做 ARP 欺骗可以嗅探到 token。家用网挡"顺手连一下"够，挡不住有心人。要真安全：自签证书 + 手机信任，或走 WireGuard/Tailscale。
- **无失败限速**：token 是 128 bit 随机，暴力不可行，但没有失败计数/封禁。
- 手机**离开局域网（4G）就不可达**，那要另配 VPN。
- 没有推送：手机 app 关掉后不会"本轮完成"通知（要推送得另接 FCM/本地服务）。
- 现有 WebUI 是 2~3 列 grid、`@media` 查询 0 个 → 手机上能连能用，但**布局偏挤**；要好用得给 WebUI 补 `@media`，或另写单列移动版页面。
- 一次一个实例（cookie 全局），不能并排看两个。

## 排障

| 现象 | 原因 / 办法 |
|---|---|
| 手机连不上（超时） | ① PC 防火墙：`ufw` 若启用需 `sudo ufw allow 8602/tcp`；② **随身wifi/AP 开了"客户端隔离"**——很多随身wifi 会把 WiFi 客户端与 USB 主机互相隔离，需在 AP 设置里关掉；③ 手机不在同一网段 |
| 502 上游不可达 | 实例没起 / `instances[].url` 写错；`curl http://127.0.0.1:8451/health` 先验 |
| 403 | 端点不在白名单，或方法不符（如 `GET /strip-injects`） |
| 反复回到登录页 | cookie 被清 / token 改了 / 浏览器禁第三方 cookie（桥与页面同源，正常不该发生） |
| 流式不出现 | 中间加了会缓冲的反代；桥本身已逐块 flush，别再套一层 |

## 测试

`go test ./...`：鉴权（未登录 302 且不碰上游 / 错 token 401 不发 cookie / Bearer 可用）、
白名单（未知路径 403、方法不符不放行、`POST /config` 默认 403、开关打开才放行）、
实例切换（cookie 切上游、认不出退回默认）、**SSE 逐块 flush**（第一帧必须在上游第二帧之前就到达）、
上游不可达 502、`/healthz` 免鉴权、token 自动生成 + 落盘 600 + 重启不换。
