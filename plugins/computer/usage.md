# computer — 前置、命令表、坐标空间、基本循环

## 两台机器上的桌面（2026-11-06 实测，两台都能做 GUI 自动化）

| | 本机（.2） | 另一台（.3） |
|---|---|---|
| `xusectl` | `plugins/computer/xusectl` | 同一棵树已同步（换根见 services.md）（3.5 MB，直接可跑） |
| 桌面与显示 | XFCE4，`DISPLAY=:0` | **XFCE4 + Xorg :1，`DISPLAY=:1`**，1920×1080（lightdm）。`/tmp/.X11-unix/X0` 存在但打不开 —— **别用 `:0`** |
| 锁屏 | 有 `xfce4-screensaver` → 截图前先 `$XU wake <密码>` | **没装 xfce4-screensaver**（不会锁屏，`wake` 用不上） |
| 依赖 | ffmpeg / xdotool / wmctrl 齐 | 同样齐；另装有 firefox、chromium、thunderbird |
| 实测 | 已用于发抖音等 | `stack` 能列出真实窗口与 EWMH 堆叠；`shot` 出 1920×1080 且远端解码正常 |

前置都一样：得有该用户的图形会话在 seat0 上 active（`loginctl list-sessions`）。`xusectl` 不在 PATH 里（正常，两台都用全路径调）。

## 用法

`XU="plugins/computer/xusectl"`

| 动作 | 说明 |
|---|---|
| `$XU shot [f] [--out f] [--settle ms]` | 整屏截图 1920×1080（f 可作位置参数=落盘路径；锁屏时会提示先 wake） |
| `$XU wake [密码]` | 解锁 xfce4-screensaver 锁屏（密码走显式参数，或 env `OKHUMAN_UNLOCK_PASSWORD`）。**本机锁屏密码备忘（用户 2026-09-30 授权记录，纯文本，插件代码/二进制未为此改动）**：`（本机 sudo 口令：见凭据保管处）` |
| `$XU move x y` / `click x y [--button left\|right\|middle] [--double]` | 鼠标（坐标=屏幕真实像素） |
| `$XU click x y --target <窗口名\|0xID> [--settle ms]` | **关键点击首选**：显式置顶目标窗→settle(默认300ms)→校验指针下顶层窗确是目标（或其子窗），不过则 exit 4 中止，绝不让点击落在盖窗上（2026-09-13 Thunar 盖窗事故后新增） |
| `$XU right_click x y` / `double_click x y` / `drag x1 y1 x2 y2` | |
| `$XU scroll x y --dx N --dy N` | 滚轮（dy>0 向下，dx>0 向右） |
| `$XU type "文本"` | 输入：ASCII 走 XTest 键入；**中文等非 ASCII 逐码点走 `xdotool key U+码点`**（免剪贴板——本机 xclip 所有权管理有坑，勿用）。**⚠ 2026-10-01：本动作在微信 4.x 里完全失效（中文、ASCII 都进不去），须绕开它自己逐 keysym 喂 xdotool，见 `typing.md`** |
| `$XU key ctrl+c [--repeat N --interval ms]` | 按键组合（危险组合 ctrl+alt+del/alt+f4 等直接拒绝；Return 有间歇丢失史，重要单键可 `--repeat 2`） |
| `$XU windowactivate <窗口名\|ID>` | 激活+置顶（**xfwm4 有 100-250ms 置顶延迟**，点击前先用 stack 核对） |
| `$XU windowraise <窗口名\|ID>` | 仅置顶（立即生效、无延迟、不动焦点） |
| `$XU windowminimize` / `windowmaximize` / `windowunmaximize` | 窗口状态控制（maximize 走 wmctrl -b） |
| `$XU stack [x y]` | 诊断：指针下（或指定坐标处）顶层窗 + active 窗 + EWMH 堆叠序（ID 归一十进制） |
| `$XU sleep <ms>` | 等待（替代 bash sleep，动作序列内可用） |
| `$XU hold ctrl 800` | 按住不放 |
| `$XU focus 窗口名` | 聚焦（windowfocus，纯键盘定向、无置顶延迟）；后续 type/key 先 focus 再全局 XTest 投递 |
| `$XU open <cmd...>` | setsid 起应用（日志 /tmp/xuse-open-*.log） |
| `$XU diff prev.png [--region x,y,w,h] [--out new.png]` | 像素差异预检（YAVG≈0=没变；小区域务必 --region，否则变化被全屏稀释到 0） |

## 坐标空间

- 屏幕 1920×1080，动作坐标默认=屏幕真实像素。
- **注入进上下文的图一律是原生分辨率**（shot 出的文件 = 屏幕像素，okattach 图片原图直传）→ 图上读到的坐标就是屏幕坐标，**不需要任何换算**。
- 2026-10-28：`okattach --scaled`（长边 1600 缩略图）已删除——缩略图会让模型按缩略图读坐标再乘 1.2，是点空的主要来源；现在传 `--scaled` 直接报错。`--view-w W` 只保留给"手上是别处来的缩放图"这种外部情况，日常用不到。

## 基本循环（agent 侧）

1. `shot` → `okattach <截图>` → 我看见屏幕
2. 决定动作（click/type/key…）
3. 执行后 `diff <上次截图> [--region …]` 廉价验证；要细看再 shot+okattach

## 失败与限制

- **`$XU sleep 300` 是 300 毫秒，bash 的 `sleep 300` 是 300 秒**。动作序列里的等待一律 `$XU sleep <ms>`；要 bash 等待就写 `sleep 1` 这种小值。睡错了想立刻往下走：`pkill -x sleep`。
- 命令被转后台后**别另起一条操作屏幕的命令**（两条会抢同一个鼠标）。
- **exec 外部命令判成功要看产物**（文件大小/内容），不能只看 exit code：曾有 shot 的 ffmpeg 缺 `-y` 而中止、exit code 仍是 0、报"已截图 1920×1080"而文件是 0 字节（已修：加 `-y` + 0 字节即 exit 3）。这版 ffmpeg：metadata 滤镜无 `:d` 选项；signalstats 输出 `YAVG=`（等号）；crop 要分别作用在每个输入上 `[0]crop[a];[1]crop[b]`。
