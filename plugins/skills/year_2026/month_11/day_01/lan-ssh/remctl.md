## 10. 跨机状态的唯一入口：`remctl.sh`（本目录；2026-11-06 建并实测）

别再现场拼 `pgrep`/`systemctl`/`ss`/`nvidia-smi`——拼错一次付一小时。**主机只用 ssh 别名**（`local|ey|sd-box`）：写 IP 会绕开别名里的密钥，认证失败会被误当成网络故障（2026-11-06 我犯过一次）。

| 命令 | 干什么 | 实测要点 |
|---|---|---|
| `remctl.sh matrix [hosts]` | 每台：运行中的单元（标 `[user]`/`[system]`）+ 端口探活 + **每张卡显存归属哪个单元** + 我起的长期脱管进程 | 显存归属按 `/proc/<pid>/cgroup` 判：本机 8081 的 ninfer 挂在 `okhuman-8451` 名下 —— 这就是「重启 user 单元会连带杀掉 agent 起的进程」的现场证据 |
| `remctl.sh owns <端口或名字> [hosts]` | 谁提供这个端口、进程属主、归属单元、**正确停法** | 数字端口从 `ss -tlnp` 取 pid（别用裸数字 pgrep，会撞上无关进程）；远端非 root 看不到属主 → 自动走 `sudo -S -k`（口令取自 credentials §10，不回显） |
| `remctl.sh kill <host> <模式>` | pgrep → kill -9 → **再 pgrep 复核**；没杀干净返回非零 | 自动排除自己（`bg-task-watch` §1 的自匹配坑，我在这工具里又踩了一次才修） |
| `remctl.sh pre <host> [远端路径...]` | 跑远程命令前预检：免密、sudo 能力、远端文件在不在、非 ASCII 路径提醒 | 非 ASCII 路径 → 用 `tar-over-ssh`（`rsync` 的远端路径会被转义而失败，2026-11-06 实测四条全失败） |

停法的判据：`.3`（ey）**全是 user 单元、属主 ey**（11-08 起，见 §7.1）→ `systemctl --user`，**别用 sudo**；本机只有 3 个 user 单元，其余是 agent 起的**非单元**进程（只能 kill 并复核）。`remctl.sh matrix/owns` 在 `.3` 上 units 段会空、还会指错停法（它只在 `IS_LOCAL=1` 才查 `--user`，2026-11-16 实测：owns 说走 sudo，实际 `not found`）。
远端命令优先 **`ssh 'bash -s' < 本地脚本文件`**（免掉引号嵌套与非 ASCII 转义）。base64 喂 `bash -s` 也可，但 2026-11-12 两次遇到它**整段静默不执行**（无输出、无报错、退出码还 0）——所以凡改动远端状态（restart/kill/删文件）之后必须回查（`systemctl --user show -p NRestarts,ExecMainPID` 之类），别把空输出当成功。。**中间产物（patch、清单、归档）一律带日期唯一名**：2026-11-12 复用昨天的 `/tmp/*.patch` 把旧版内容提进了 git 索引，而生成新 patch 的 python 已崩（缺 import）没写出文件、旧文件静静留着被用掉，没有任何报错

## 十一、控制端可以是任一台

两台互为对等（`.3` 也能 ssh 到 `.2`）。**两台的仓库根/模型根/单元名对照只写在 `host-crash-autostart/services.md` 的「另一台机器」一节**，此处不重复。
**远端路径含非 ASCII（桌面/固态）时 rsync 会失败 → 改用 tar-over-ssh**（`tar cf - -C 根 子路径 | ssh 对端 tar xf - -C 根`）。
