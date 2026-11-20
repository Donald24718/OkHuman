# computer — 锁屏与屏保

- 本机锁屏 = **xfce4-screensaver 全屏窗口**，合成事件唤醒不了 → 用 `$XU wake <密码>`（全局 XTest 输密码）。**动手前先 `shot` 探锁**（shot 直接报"屏幕已锁"），锁了先 `wake`——锁着时所有按键都喂进密码框，会把密码拼在垃圾后面解不开。
- 自动锁的时间 = xfconf `xfce4-screensaver /saver/idle-activation/delay`，**本机现在 10 分钟**（2026-11-10 核对；别再按"2–3 分钟"防它）。长 GUI 流程要防打断只需 `xset s off`（结束 `xset s on`），**不必也不该去关 DPMS**。
- 「屏保后黑屏唤不回、只能 tty 重启桌面」那个老 bug：真凶是 **light-locker**（2026-08-27/28 已 purge，锁屏换成 xfce4-screensaver，当时另加 `~/.config/autostart/disable-dpms.desktop` 当保险）。2026-11-10 实测：`xset dpms force off` 与自动路径（空闲→Standby→Off）唤回 **8/8 全部正常**，保险已停用（改名 `disable-dpms.desktop.disabled-20261110`，要回退就改回原名）。万一再遇唤不回：先 `xrandr --output HDMI-0 --off && xrandr --output HDMI-0 --auto` 重训链路，别急着重启桌面。
