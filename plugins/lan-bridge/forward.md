# lan-bridge — 鉴权、端点白名单、切实例、Origin 改写、流式

## 鉴权

- `GET /login` → 表单页；`POST /login`（表单，token **不进 URL/历史**）→ 发 `okh_bridge` cookie（HttpOnly、SameSite=Lax、30 天）→ 302 到 `/`
- 也支持 `GET /login?t=…`（方便点链接，但 token 会进浏览器历史，能用表单就别用）
- 非浏览器客户端：`Authorization: Bearer <token>`
- 未登录访问任何转发端点 → 302 到 `/login`（上游一次都不会被碰）；token 比较用 `subtle.ConstantTimeCompare`

## 端点白名单（默认拒绝，不在表里一律 403）

`GET`：`/` `/favicon.ico` `/marked.min.js` `/sanitize.js` `/health` `/status` `/queue` `/history` `/config` `/prompts` `/attachments` `/events` `/events/poll`
`POST`：`/enqueue` `/stop` `/reset` `/compress` `/strip-injects` `/inject`（`POST /config` 仅在 `allow_config_write=true` 时放行）

桥自己的端点（不转发、不含会话内容）：`GET /healthz`（不鉴权，给 okmon/排障）、`GET /pick`（鉴权，列实例）、`GET /pick?inst=<id>`（设 cookie 后 302 回 `/`）。

## 切实例

`/pick` 列出配置里的实例，点一下设 `okh_inst` cookie 并回首页。**挂多个上游时点名必填**：未点名或 id 认不出 → 400（静默落默认的后果是"回信来自另一个 agent"，看不见）。点名只认 `okh_inst` **cookie**——写成 `?okh_inst=` query 参数同样 400（2026-11-16 踩过，别据此判定"重启后桥指丢了"）。例外只有两种：桥只挂一个上游（没得选）；`/` 与静态资源——人先落到 `default_instance` 的页，同时把这次选择钉进 cookie，要换实例走 `/pick`。

## Origin 改写（手机上发消息的关键）

主程序对写请求有守卫：`Host` 与 `Origin` 的 host 必须是本机地址（挡 DNS rebinding 与跨站页面）。浏览器发给桥的是 `http://<局域网IP>:8602`，原样转过去会被实例判「Origin 不是本机同源」→ 手机上**所有发消息都失败**。桥因此把 `Origin` 改写成上游自己的 origin（`http://127.0.0.1:8451`），`Host` 也走上游地址。**守卫的威胁模型没有被削弱**：恶意跨站页面根本进不了桥（鉴权 cookie 是 `SameSite=Lax`，跨站 POST 不会带上）。实测对照：同一带外 Origin 的请求直连实例仍返回 403。客户端不带 Origin（curl/脚本/插件）时桥不伪造。

## 流式

`/events`（SSE）**逐块 flush、不缓冲**，并带 `X-Accel-Buffering: no`。手机断开只断这条连接——实例侧 run 早就与 handler 解耦（run 只往 `state.Evt` 内存缓冲放帧），掉线缺口由 `/history` 兜底。

## 测试覆盖（`go test ./...`）

鉴权（未登录 302 且不碰上游 / 错 token 401 不发 cookie / Bearer 可用）、白名单（未知路径 403、方法不符不放行、`POST /config` 默认 403、开关打开才放行）、实例切换（点名切上游；未点名/认不出 400；单上游不需要点名）、**SSE 逐块 flush**（第一帧必须在上游第二帧之前就到达）、上游不可达 502、`/healthz` 免鉴权、token 自动生成 + 落盘 600 + 重启不换。
