# lan-bridge — OkHuman 局域网桥（手机用 WebUI）

手机在同一局域网里用 OkHuman WebUI 的**唯一对外入口**：主程序继续只听 localhost、零改动，鉴权与端点白名单都集中在这一处。**本文件不含参数与端点表。**

## 要做什么 → 去哪个文件

| 要做 | 文件 |
|---|---|
| 构建、启停、改配置（端口/token/实例列表）、systemd 单元 | `run.md` |
| 鉴权怎么走、哪些端点通、怎么切实例、Origin/Host 改写、SSE、测试覆盖 | `forward.md` |
| 手机怎么接入；能做到什么程度、做不到什么 | `phone.md` |
| 连不上、502、403、反复回登录页、流式不出；跨机入口拓扑 | `trouble.md` |
| 旧记录（**归档**，以现行文件为准） | `LESSONS.md` |

## 遇到现象 → 去哪个文件

| 现象 | 文件 |
|---|---|
| 手机打开地址超时 / 连不上 | `trouble.md`（防火墙、AP 客户端隔离、网段） |
| 502 / 403 / 反复回到登录页 | `trouble.md` |
| 手机上发消息全部失败 | `forward.md`（Origin 改写） |
| 切实例返回 400、或"回信来自另一个 agent" | `forward.md`（`okh_inst` 只认 cookie） |
| 改了 config 没生效 | `run.md`（必须重启桥；按端口取 pid） |
