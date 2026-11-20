# qq-channel/ — QQ 官方渠道桥（okhuman-qq-bridge）

把 OkHuman 实例接入 QQ 的独立常驻进程：QQ 消息进 agent、agent 回复回 QQ；无监听端口，只有出站连接。**本文件不含链路细节、参数与指令表。**

## 要做什么 → 去哪个文件

| 要做 | 文件 |
|---|---|
| 起停、重编、改 config、看运行时数据（token/msg_seq/让位令牌） | `run.md` |
| 上/下行链路、气泡拆分与长度、多 bot、重连、解耦契约 | `link.md` |
| QQ 里发 `/stop` `/restart` 这类控制指令；要不要开白名单 | `commands.md` |
| 给 QQ 发文件（attach）、收 QQ 发来的文件落在哪 | `media.md` |
| 排障 | `trouble.md` |
| 踩坑档案（**归档**） | `LESSONS.md` |

## 遇到现象 → 去哪个文件

| 现象 | 文件 |
|---|---|
| 改了源码行为没变 / "响应无 access_token" | `trouble.md`（先比二进制与源码 mtime） |
| QQ 发了没反应；双份转发、重复入队 | `trouble.md`（订阅、让位令牌） |
| 消息被 QQ 拒 | `trouble.md`（msg_seq 回退） |
| 数据写到旧目录 | `run.md`（data_dir 启动时算定） |
| 想发到 QQ 却没发出去 / 发错了会话 | `media.md` |
| `/stop` 没中断、`/restart` 卡很久 | `commands.md` |
