# 开机自启：两实例 + 两后端 + scout + qq 桥 + lan-bridge

## 前置：数据盘挂载（2026-10-29 起已配，开机自启的第一道依赖）

运行树在 `<数据盘>/512nv固态`（nvme1n1p1），模型在 `<数据盘>/mode`（sdc1，P5-1TB NVMe）。
这两块盘原本不在 fstab，由 udisks 在桌面会话挂载 → 实测挂载发生在**开机后约 4.8 分钟**（07:33:13 开机 / 07:38 挂上）。
三个 unit 在开机后 13 秒就全部失败：`203/EXEC`（strata、okhuman——ExecStart 路径不存在）、`200/CHDIR`（qq 桥——WorkingDirectory 不存在），
靠 `Restart=on-failure` 空转约 5 分钟（okhuman 计数 56、strata 20）才撞上挂载。**症状不是"起不来"，是"空转几分钟后自己好了"** —— 只看 `is-active` 会漏掉。

已配两层（缺一不成立）：

1. **fstab 静态挂载**（备份 `/etc/fstab.bak-20261029`）。**五块盘全部开机挂载**（2026-10-29 主人要求）：
   | 挂载点 | UUID | 介质 | device-timeout |
   |---|---|---|---|
   | `<数据盘>/512nv固态`（运行树+插件） | `ee45de3e-1e25-4db1-ac12-3642e2ab3dc9` | NVMe | 30s |
   | `<数据盘>/mode`（模型权重） | `dc8eb29f-d2a1-4a39-8fa2-bd31941c5fe9` | NVMe(P5-1TB) | 60s |
   | `~/hdd`（含 okhuman-8453 实例） | `66ee1582-11d5-4b32-a33b-cae32eca087a` | 旋转盘 ROTA=1 | 60s |
   | `<数据盘>/lil_app`（8452 副本树） | `4756cb5a-a066-4939-a0d9-19d433f34895` | SSD | 30s |
   | `<数据盘>/机械硬盘500g`（归档 tar/7z） | `1d07d146-ace9-47f1-a5d2-c1aae243ab5e` | 旋转盘（老盘，读写慢） | 60s |
   统一 `defaults,nosuid,nodev,nofail`。**必须显式写 `nosuid,nodev`**：udisks 现在的挂载带这两项，只写 `defaults` 会在下次开机把它们去掉、静默改变挂载语义（`systemctl show <mount> -p Options --value` 里能看到当前还带 `uhelper=udisks2`，那是运行时旧挂载，fstab 选项下次开机才生效）。`nofail`：`nofail`=缺盘/坏盘不卡开机（unit 只进 local-fs 的 Wants 不是 Requires）；`passno=0`=开机不 fsck（要 fsck 手动来）。
   挂载点含中文**直接写 UTF-8**：fstab 转义只对空格/`\`/`#` 有效且是八进制（`\040`），`\xNN` 会解析出错（实测 `findmnt --verify` 报「第 14 行解析出错」）。
   挂载是并行的，开机多花的时间≈最慢那块盘的 spin-up（几秒），不是各盘累加。
   改后校验三连：`findmnt --verify` → `sudo systemctl daemon-reload` → `systemctl list-units --type=mount | grep media` 应五个都 active、
   且 `systemctl show <unit> -p FragmentPath --value` 指向 `/run/systemd/generator/…`（= 由 fstab 接管，不再是 udisks 的 transient 单元）。
   **daemon-reload 不会卸载已挂载的盘**（adopt 而非重挂），所以能在运行中的会话里安全改 fstab。
2. **`ExecStartPre=~/bin/wait-for-paths.sh -t 120 <关键文件绝对路径…>`**（三个 unit 都有）。
   **user unit 用不了 `RequiresMountsFor=`**：`/media/…` 的 `.mount` unit 属 system manager，user manager 看不见（跨 manager 无依赖）。只能自己等。
   脚本轮询 `-e` 直到全部存在或超时，超时退 1 → unit 失败 → 按 Restart 重试，并在 journal 指名缺哪个路径。
   坑：`ExecStartPre` 计入 start timeout，默认 90s 会掐掉长等待 → 同 unit 里显式 `TimeoutStartSec=`（strata 600、其余 180）。含空格路径在 unit 里用引号包（实测 systemd 解析正确）。

## 实现

`~/.config/systemd/user/{strata-8080,ninfer-8081,okhuman-8451,okhuman-8452,okhuman-scout,qq-bridge,lan-bridge-8602}.service`，均 enabled（共 7 个）。`Linger=yes` → 无需登录。

- 排序：`strata-8080` → `okhuman-8451`（After=）→ `qq-bridge`（After=okhuman-8451）→ `lan-bridge-8602`（After=两实例）；都 `Wants=network-online.target`。`After=` 只保证顺序不保证就绪，8080 的模型加载要 3 分钟，okhuman 先起属正常。
- strata 走 `run-strata.sh strata-262k.json`（从 config 读 port/gpu，设 `STRATA_GGUF_PY`、`LD_LIBRARY_PATH`）；`Type=simple`，模型加载 3 分钟不会被判超时（`TimeoutStartSec=600`）
- 日志 append 到 `~/logs/<unit>.log`（strata、okhuman、lan-bridge-8602）；qq-bridge 走 journal：`journalctl --user -u qq-bridge`
- `Restart=on-failure`（okhuman 5s / strata 15s / qq 10s / lan-bridge 5s + `StartLimitBurst=10`）；`Restart=on-failure` 不救干净退出（人手动 stop 不会被抢回来）

## 坑：目录名含空格 → `status=203/EXEC`

`Frequently Used Plugins` 下的 `ExecStart` 会被 systemd 按空格切断，`verify` 报 `Command /media/…/Frequently is not executable`。
解法两选一：`ExecStart` 整条路径各加双引号（2026-11-19 `lan-bridge-8602` 实测 `verify` 通过并实跑，不必包装脚本），或指向无空格的包装脚本 `~/bin/start-qq-bridge.sh`（脚本内带引号 `cd` 再 `exec`）。okmon 的 `workdir` 是同一个坑（它已用 shellQuote 修）。
注意与 203/EXEC 的另一种成因区分：**路径不存在（盘没挂）也报 203/EXEC**——看 `journalctl -b | grep 203/EXEC` 时先查挂载。

## 管理

- `systemctl --user {start,stop,restart,status} strata-8080|okhuman-8451|qq-bridge|lan-bridge-8602`
- 校验：`systemd-analyze --user verify ~/.config/systemd/user/<u>.service`（无输出=通过）。`systemctl verify` 没有这个动词。
- 重启推理后端用 `systemctl --user restart strata-8080`。**不要用 `restart-strata.sh`**：它 `pgrep -f "strata-volta-0.1.44.1"` 会匹配到脚本自己、把自己 kill 掉，结果只完成"停"完不成"起"。
- 手工起的只剩 cron、okmon（setsid）；8452 / 8081 / scout 自 2026-11-08、lan-bridge 8602 自 2026-11-19 起是**持久 unit（enabled）**。接管前先 `systemctl --user list-units --all '<前缀>-*'` + `pgrep` 确认没有旧进程，否则双起（qq 桥双起会抢登录）。
- **不在自启内**：8453（`机械1tb`）、8454（小G，`mode`）、okmon（:8496）、cron（:8601）。盘已开机挂载，起不起是另一层。
  **2026-11-05 起 scout 没有 embed 后端了**：它的语义向量与常驻 `:8591` llama-server（RSS 3.49 GB）已拆掉，起一个 scout 进程就是全部；
  它的**索引不落盘**（真源是 meta.json，扫一遍毫秒级），所以开机后没起 scout 就是 `docs=0`——`/health` 会照实报 0，不会拿旧快照假装正常。
- 开机后确认：`systemctl --user is-active strata-8080 okhuman-8451 qq-bridge`、`ss -ltn | grep -E '8451|8080'`、`journalctl --user -b -u <u> -n 20`；
  查是否又踩挂载竞态：`journalctl -b --user | grep -E '203/EXEC|200/CHDIR'`（应为空）、`systemctl status 'media-<用户>-*'` 的 `Active since` 应贴近开机时刻。

## 实测边界

- 三个 unit 已实跑接管：2026-10-29 开机自启，挂载后被 Restart 救活，8451/8080/qq 桥都在 unit 名下（CGroup 可查）。
- 就绪门已实测（一次性 test unit，用完即删）：路径存在→放行；缺路径→超时拦停、`ExecStart` 不执行、日志指名缺哪个路径。含空格路径引号写法已验证；transient 单元更不用管（systemd-run 自己转义，11-08 实测 `Frequently Used Plugins/…` 正常起），「不能含空格」只针对写死的 unit 文件。
- **fstab 挂载本身未实跑开机验证**（2026-10-29 已在运行会话里 daemon-reload 验证到「五个 mount unit 由 generator 接管且保持 active」，但没重启过）（改完不能重启自证——会杀掉自己）。下次开机按上面「开机后确认」两条 grep 判定。

## 坑：重启一个 user 单元会连带杀掉同一 cgroup 里所有"非单元"进程（2026-11-05 实测）

- 机理：`setsid` 只脱离**进程组**，**不脱离 systemd 的 cgroup**。凡是 agent 从 8451 里用 bash `setsid` 起的常驻服务，仍挂在 `okhuman-8451.service` 的 cgroup 里；`systemctl --user restart okhuman-8451` 会把整组收走。
- 实测被杀清单（2026-11-05；此后 8452/8081/8480/8602 各自成 unit，都不再被连坐）：**8452 小B、它的后端 8081 ninfer、8480 scout、8601 cron、8602 lan-bridge、8496 okmon**。`qq-bridge` 一直活着，因为它是独立单元。反向的一半：agent 进程**异常崩溃**（不是单元被 stop）时 cgroup 还在，setsid 服务变孤儿**带旧配置继续跑** →「配置改了行为没变」先按端口取 pid 看进程多老。
- **enabled 的 user 单元 7 个**：`okhuman-8451`、`strata-8080`、`qq-bridge`，加 2026-11-08 补的 `okhuman-8452`、`ninfer-8081`、`okhuman-scout`（排序 `ninfer-8081` → `okhuman-8452`，都带 `ExecStartPre=wait-for-paths.sh`），加 2026-11-19 补的 `lan-bridge-8602`（After=两实例，无 `ExecStartPre` 之外的特殊项）。restart 8451 谁都不被连坐（11-08 实测）。
- 所以：重启 8451 前先想清楚会连带死一片；死后按各自 README/脚本重新起——
  ⚠️ 同名 transient 会挡住持久 unit：`enable` 报 `is transient or generated`、`start` 报 `Unit not found`。顺序必须是 `stop <名>`（`--collect` 回收 transient）→ `daemon-reload` → `enable` → `start`（11-08 踩过）。临时实例才用 `systemd-run --user --unit=<名> --collect …`，写法见 `services.md`。
- unit 用 `systemctl --user {stop,restart}`；transient 用 `stop`；setsid 起的只能 `kill`。
