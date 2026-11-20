# 用 ssh 操控局域网另外两台机器（lan-ssh）— 母文档只做路由

适用：在那两台机器上执行命令、装包、改网络、看它们上的 OkHuman 实例。密码只存条目 `credentials`，本篇不重复。

## 要做什么 → 读哪份

| 要做什么 | 文件 |
|---|---|
| 用前先复核的几条入口（谁可达、用什么身份） | `entry.md` |
| 远程执行的硬规矩；隐私边界 | `remote-rules.md` |
| 改它网络之前、NetworkManager / netplan 的实测结论、当前生效配置、装包 | `network-sdbox.md` |
| 对面那台上的实例怎么起、健康检查、缺什么工具、换推理后端 | `ey-instances.md` |
| 把推理后端复制到第二台 V100 机器时必须补什么 | `backend-copy.md` |
| 跨机状态的唯一入口脚本 | `remctl.md` |

## 遇到什么现象 → 读哪份

| 现象 | 文件 |
|---|---|
| 连不上、被要密码、命令在远端挂住 | `entry.md`、`remote-rules.md` |
| 远端的用户级服务起不来、报 XDG_RUNTIME_DIR not found | `remote-rules.md`、`ey-instances.md` |
| 改过网络后失联、配置不生效 | `network-sdbox.md` |
| 对面实例在跑但探活不通、换完版本起不来 | `ey-instances.md` |
| 复制过去的后端跑不起来、缺依赖 | `backend-copy.md` |
