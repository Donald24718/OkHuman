
## 2026-09-19 / 09-20 token 获取全失败："响应无 access_token"（同一个 bug 栽了两次）

现象：每次取 token 都报"响应无 access_token"，手动 curl 同一 endpoint 同一凭据却正常——77 次响应全成功、0 次解析通过。
真因分两层（09-20 复发就是第二层）：① QQ 返回 `"expires_in":"7156"` 是**字符串**，TS 靠 JS 隐式转型混过，Go 的 `ExpiresIn *int` 严格解析 → **整个 struct Unmarshal 失败** → 走失败分支；改 `json.Number`（字符串/数字都收）。② 源码 09-19 已修，**二进制还是 09-14 的旧编译**——QQ 改字符串型后旧二进制必挂。
可复用：
- **TS→Go 移植里凡是靠 JS 隐式转型活着的字段必须显式适配**（TS 的 type 是文档，不是契约）。
- 排这类问题第一步**打印原始响应体**，别信错误信息（"无 access_token"实际是"expires_in 不是 int"）。
- **改完源码 = 没干活**，重编 + 真实启动验证才算完。排版本问题先对三个时间：源码 mtime、二进制 mtime、日志里 bug 出现的时间——二进制比源码旧就是没重编。

## 2026-09-26 日志迁 /tmp（tmpfs 易失）

运行日志落 `/tmp/okhuman-qq-bridge.log`（tmpfs，重启即空）；迁移前全量存档在 `archive/bridge-go-20260926.log`。
可复用：上面那套"对三个时间"**依赖 /tmp 日志还活着**——重启后近期时间线丢失，只剩 archive 旧段；排障前先确认它覆盖事件时段。

## 2026-11-08 单实例锁把桥永远挡在门外：PID 会被复用 → 换成让位（lease）

现象：开机后 qq-bridge unit 空转 75 次全退（每轮都是「已有桥在运行（PID 2491），本进程退出」），`is-active` 显示 activating；QQ 渠道断了约 1.5 小时无人发现。
真因：`bridge.lock` 是 O_EXCL 创建、内容 = 旧 PID，判活用 `os.Stat("/proc/<pid>")`。① 退出路径**没有任何删锁**（无 signal handler），而 `data/` 在插件盘不是 tmpfs → 陈旧锁跨重启仍在；② 那个 PID 已被 lightdm 会话进程复用 → 永远判成"别人在跑"。`Restart=on-failure` 每轮得到同样的判定，救不回来。
现在（已实装，旧 O_EXCL 段整块删除）：启动时把随机令牌以 tmp+rename 写进 `data/channels/qq/bridge.gen` = 接管；每 2s 自检文件里的令牌是不是自己——不是就停止取号与转发并**退 0**，读不到文件就重写自己（自愈）。
可复用：
- **拿 PID 判"那个进程还在"是启发式，PID 会复用**；真要进程身份只能比 `/proc/<pid>/exe` 绝对路径（得硬编码路径）。让位把判定改成**自证**（比对我自己写的随机令牌）：不识别对方、不需要清理钩子、不存在"陈旧状态"这种东西。
- 让位**必须退 0**：`Restart=on-failure` 不触发，unit 才不会跟手工起的桥互杀成循环。实测：让位后 `NRestarts=0`、unit 状态 inactive；反向（unit 抢回）手工桥同样自退，全程只有一个桥进程。
- 写令牌后先 `sleep leaseTick+1s` 再恢复 msg_seq / 连网关：旧桥最多再活 2s，把双写者窗口关在启动阶段而不是收发阶段——`c2cMsgSeq` 是内存计数 + 落盘文件，两个进程并发必回退，回退被 QQ 拒（见 09-24）。
- unit 侧配套 `RestartPreventExitStatus=1`（此后 exit 1 只剩 config 坏 / 无有效 bot 这类永久性错误）。一次性 test unit 实测：exit 1 → failed、`NRestarts=0`，不再空转。
