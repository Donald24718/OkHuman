# 崩溃与内存报错：留证、读法、判读

## panic 时什么会丢

- 已写入 journal 的不丢（持久化）。**panic 一发生所有用户态冻结，journald 来不及写最后几条。**
- 触发 panic 的那条 MCE 只存在于 CPU MCA bank 的 MSR 里，**复位/断电即永久消失**；本机无 BMC、无 ERST，没有旁路。
- kdump 未生效期间真出 UE：唯一取证办法是**拍屏幕**，窗口 60 秒。

## 四条措施（2026-10-29 已配）

1. **死机先别断电，拍照**。
2. **内存哨兵**：cron `30 3 * * * ~/bin/mce_watch.sh`（**v3，2026-10-29 08:20 修**；旧版 `mce_watch.sh.bak-v2-20261029-0818`、v1 `mce_watch.sh.bak-20261029`）
   - 日志 `~/logs/mce_watch.log`（磁盘），基线 `~/state/mce_ce.state`（`boot=` + `ce=` + `ue=`）
   - 采三样：journal 24h 的 MCE/CE 事件行数（`journalctl -k`）、EDAC 逐 DIMM 的 CE/UE、与上次快照的 CE 增量
   - 日志字段：`NCE` journal 事件数 / `TOTAL_CE` `TOTAL_UE` 本次开机累计 / `DCE_24H` 自上次采样增量 / `BOOT_RESET` / `NEG` / 逐 DIMM 明细；**全零时写 `EDAC=ALL_ZERO`，别把"没有明细"读成"没问题"**
   - 播报条件（任一）：journal 有新事件 / **同一 boot 内** CE 增量 > 0 / UE > 0 → `POST 127.0.0.1:8451/enqueue`
   - 首次无基线不播报（否则把开机累计当增量误报，旧版 07:13 那条 `DCE_24H=294372007` 就是这么来的）
   - **v3 修的三件事**（都是真踩过的坑，改代码前先照做）：
     a. state 带 `boot=`。EDAC 计数换开机归零，旧版拿上个 boot 的累计值相减 → 巨大负数 → 条件不成立，**真实 CE 增长被吞掉一个 24h 窗口**。现在换 boot 置 `BOOT_RESET=1`、基线归零重算，不播报。
     b. 同 boot 内增量为负（有人手工 `reset_counters`）→ `NEG=1` 并在消息里点明增量不可信，不当成正常的 0。
     c. journal 匹配串必须 `journalctl -k`（只内核）+ 宽正例 + 显式排噪。**不带 `-k` 实测 24h 内误报 6 条**：内核每次开机都打 `RAS: Correctable Errors collector initialized`、smartd 的硬盘 `Offline_Uncorrectable`、还有哨兵自己写进 journal 的命令文字被自己采到（自反污染）。反过来旧版那两个精确串常年 N=0——一个 CE 累计 2.9 亿的会话在 journal 里一条都没采到，那条信号形同虚设。
   - 自测（用隔离副本，别污染真 log/state）：`sed` 换 LOG/STATE 路径 + 把 EDAC 路径指向自造的假 sysfs + 把 curl 换成 echo。覆盖：无基线 / 旧版无 boot= / 同 boot 基线高于当前(NEG) / 全零 / CE 增长(应播报) / UE>0(应播报) / 换 boot。匹配串单独测：真日志应为 0、构造的正例应全中、构造的噪音应全被排。
3. **kdump**：`kdump-tools` enabled+active；GRUB 里 crashkernel 按内存分级（`128G-:4096M`，本机保留 4 GB）；dump 落 `/var/crash/`；分析用 `crash` 包；capture kernel 在 `/var/lib/kdump/`。
   - 生效与否只看一条：`grep crashkernel /proc/cmdline`。为空时 `kdump-config show` 打印 `no crashkernel= parameter in the kernel cmdline` 是**正常**的（没重启而已），GRUB 里已有 `crashkernel=`。
4. **panic 后自动重启**：`/etc/sysctl.d/99-panic-reboot.conf` → `kernel.panic = 60`。60 秒是留给人看屏幕的时间；要更快恢复就调小。

## 读法

- `journalctl -k` 必须窄范围（`--since`/`--until` 到小时级）。跨周全量 + grep 会超 30 秒被转后台。
- 按天分布：`journalctl -k --since … | grep -iE 'cmci|machine check|\[hardware error\]|mce: |edac mc[0-9]+: [0-9]+ (ce|ue)' | grep -viE 'ras: correctable errors collector|seeking for|giving out device|mce: (cpu supports|using |physical address)' | awk '{print $1,$2}' | uniq -c`（与脚本同一套串；不带 `-k` 会把 smartd 和自己塞进来）
- 哨兵日志里的 `NCE` 统计的是**过去 24 小时** → 事件实际日期通常是报告时间的**前一天**，别把统计窗口当事件日期。
- CE=可纠正（ECC 已把数据纠正回来，数据是对的）；UE=纠不出来（要么当场崩，要么拿到错数据）。

## 哨兵自己有没有真在跑（2026-10-29 实测）

- **执行记录只在 cron unit 里**：`journalctl -u cron --since …| grep '(<用户>) CMD'`。别对整个 journal grep 脚本名——我自己的话会被 qq-bridge 写进 journal，实测 37 次命中里只有 1 次是真执行。
- **判断"什么时候建的"看 Birth，不是 mtime**：`stat --format='%n Birth=%w  Mtime=%y' 文件`。mtime 只反映最后一次改。实例：`mce_watch.sh` 的 Birth=2026-09-22，而 mtime 显示 10-29——只看 mtime 会把「什么时候开始跑」判错 37 天。
- **cron 的失败是无声的**：本机没有 MTA（`/var/mail/<用户>` 不存在），脚本里的报错没有任何去处；目录不存在时 `>>` 重定向失败也不报错（无 `set -e`）。所以脚本必须自己 `mkdir -p`（v3 已加）。10-29 03:30 那次就是"执行成功、零痕迹"——当时 `~/logs`、`~/state` 都还没建。
- **日志绝不写 `/tmp`**：v0 写 `/tmp/mce_watch.log`（tmpfs），一次重启历史全没，两个月的执行痕迹无从追。一律 `~/logs`。
- **"配了"不等于"有效"**：v0（09-22 ~ 10-29）每个开着的凌晨都在跑，但它只 grep `Machine check events logged|CMCI storm` 两个常年为 0 的串，也不读 EDAC——那条 DIMM 上累计 2.9 亿次 CE，它一次都没报过。结论：**任何监控都要能证明两件事——它跑过、它能采到东西**。后者靠自测：构造一条真事件行喂进匹配串，看它是否命中（正例应全中、构造的噪音应全被排）。
- **03:30 若机器没开，当天样本永久错过**：anacron 只补 `/etc/cron.{daily,weekly,monthly}`，**不补用户 crontab**。但**本机常开**（2026-10-29 主人确认）→ 这条不发生，**不必加 `@reboot` 补采，别再提这个建议**。

## 判读标准

- 只有一个 `dimm_label` 有 CE、其余槽全 0 → **单条 DIMM**，不是内存控制器/通道。
- 只有 CE（UE=0）不会崩，但累计值高说明那条在退化。
- **若损伤是持续性的，UE 会在小时级出现**（高 CE 密度下同一字错 2 bit 几乎必然）。此后几天 CE 归零 → 判为条件性事件（开机后重载、温度、接触），不是持续磨损。
- 立刻安排停机的条件（任一）：`dimm_ce_count` 重新增长 / 再现 `CMCI storm` / `dimm_ue_count` > 0 / 无故重启。
- 处置顺序：先在 BIOS 核对 slot 与 CE 计数 → 重插拔 + 换槽交叉验证（错误跟着条走=条坏，跟着槽走=槽/CPU 触点）→ 换同型号条。这平台 DDR3 RECC 二手便宜。**别先跑 memtest86+**：要停机且对间歇性 CE 常不复现，留作换条后的验证。
- 注意 `dmidecode -t 17` 可能报不全（本机 251 GB 只报出 4 根 32 GB）→ 物理插槽对应要在 BIOS 里核对，EDAC 的 `Ha#0_Chan#0_DIMM#1` 不能直接当丝印槽位。
