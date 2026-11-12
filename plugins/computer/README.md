# computer use 插件（xusectl）

给 agent 装"手"：整屏截图（ffmpeg x11grab）+ X11 鼠标键盘事件（xdotool/XTest）。
视觉走现有 okattach（截图注入 LLM 上下文），**内核零改动**（只用 bash + /inject）。
规划与选型依据见同目录 PLAN.md。

## 两台机器上的桌面（2026-11-06 实测，两台都能做 GUI 自动化）

| | 本机（生产） | 另一台（副本） |
|---|---|---|
| `xusectl` | `plugins/computer/xusectl` | 同一棵树已同步（配置里换根路径）（3.5 MB，直接可跑） |
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
| `$XU wake [密码]` | 解锁 xfce4-screensaver 锁屏（密码走显式参数，或 env `OKHUMAN_UNLOCK_PASSWORD`）。密码不入库：由 env `OKHUMAN_UNLOCK_PASSWORD` 或显式参数给（本机那份记在本地不入库的备忘里） |
| `$XU move x y` / `click x y [--button left\|right\|middle] [--double]` | 鼠标（坐标=屏幕真实像素） |
| `$XU click x y --target <窗口名\|0xID> [--settle ms]` | **关键点击首选**：显式置顶目标窗→settle(默认300ms)→校验指针下顶层窗确是目标（或其子窗），不过则 exit 4 中止，绝不让点击落在盖窗上（2026-09-13 Thunar 盖窗事故后新增） |
| `$XU right_click x y` / `double_click x y` / `drag x1 y1 x2 y2` | |
| `$XU scroll x y --dx N --dy N` | 滚轮（dy>0 向下，dx>0 向右） |
| `$XU type "文本"` | 输入：ASCII 走 XTest 键入；**中文等非 ASCII 逐码点走 `xdotool key U+码点`**（免剪贴板——本机 xclip 所有权管理有坑，勿用）。**⚠ 2026-10-01：本动作在微信 4.x 里完全失效（中文、ASCII 都进不去），须绕开它自己逐 keysym 喂 xdotool，见 LESSONS.md / 下方 2026-10-01 补录** |
| `$XU key ctrl+c [--repeat N --interval ms]` | 按键组合（危险组合 ctrl+alt+del/alt+f4 等直接拒绝；Return 有间歇丢失史，重要单键可 `--repeat 2`） |
| `$XU windowactivate <窗口名\|ID>` | 激活+置顶（**xfwm4 有 100-250ms 置顶延迟**，点击前先用 stack 核对） |
| `$XU windowraise <窗口名\|ID>` | 仅置顶（立即生效、无延迟、不动焦点） |
| `$XU windowminimize <窗口名\|ID>` / `windowmaximize` / `windowunmaximize` | 窗口状态控制（maximize 走 wmctrl -b） |
| `$XU stack [x y]` | 诊断：指针下（或指定坐标处）顶层窗 + active 窗 + EWMH 堆叠序（ID 归一十进制） |
| `$XU sleep <ms>` | 等待（替代 bash sleep，动作序列内可用） |
| `$XU hold ctrl 800` | 按住不放 |
| `$XU focus 窗口名` | 聚焦窗口（windowfocus，纯键盘定向、无置顶延迟）；**后续 type/key 先 windowfocus 该窗口再全局 XTest 投递**；要同时置顶用 windowactivate/windowraise |
| `$XU open <cmd...>` | setsid 起应用（日志 /tmp/xuse-open-*.log） |
| `$XU diff prev.png [--region x,y,w,h] [--out new.png]` | 像素差异预检（YAVG≈0=没变；小区域务必 --region，否则变化被全屏稀释到 0） |

## 坐标空间（重要）

- 屏幕 1920×1080，动作坐标默认=屏幕真实像素。
- **注入进上下文的图一律是原生分辨率**（shot 出的文件 = 屏幕像素，okattach 图片原图直传）。
  图上读到的坐标就是屏幕坐标，**不需要任何换算**。
- 2026-10-28：`okattach --scaled`（长边 1600 缩略图）已删除——缩略图会让模型按缩略图读坐标、
  再乘 1.2，是点空的主要来源；现在传 `--scaled` 直接报错。
- `--view-w W` 只保留给"手上是别处来的缩放图"这种外部情况，日常用不到。

## 基本循环（agent 侧，见 OkHuman/prompts/04-computer.md）

1. `shot` → `okattach <截图>` → LLM 看见屏幕
2. 决定动作（click/type/key...）
3. 执行后 `diff <上次截图> [--region ...]` 廉价验证；要细看再 shot+okattach

## 桌面自动化通用打法与硬坑

跟具体网站无关，任何"用 xusectl 操作桌面应用"都照这里做。平台特有的坑（入口 URL、字段上限、风控）在各平台 skill 里。
事故复盘与逐条原文在 `LESSONS.md`。

### 坐标：只从当次截图读

- **禁用任何"扫截图吐坐标"的脚本**（原 `locate.py` 已删，用户 2026-10-26 明令）。按钮/输入框/链接的坐标一律自己看当次截图读。
- `shot` 出的文件是**原生分辨率**（1920×1080），okattach 只有原图直传这一条路（`--scaled` 已删）→ **图内坐标 = 屏幕坐标，零换算**。历史上"1600 缩略图 + 1920 屏幕"两套坐标系混用是点空的第一大原因。
- **别目测估 y**：注入图在我这边渲染会缩小，y 误差能到 150px 以上（把 native 930 看成 775），x 也偏（发送按钮目测 1222、实际 1828）。点空前先核对**目标所在行的上下文**（上下行是什么）——多数"点空"是把相邻行看串，不是缩放。
- 小目标（文字链接）扫它**旁边的图标**按行统计：例如评论动作行的气泡图标（x 364–392 的实心灰块），一次能算出所有动作行的 y。
- 需要放大才看清时才 crop，且**读数换算**：`native = crop起点 + 显示坐标/倍率`。能读 1:1 原图就别 crop。
- 读不准就**分两步点**：点目标中部 → `shot` 看落点 → 再微调。点完必须 `shot` 核对，"点了没反应"不是结论。
- 红色拼写检查下划线和红色按钮同色：找红色按钮要看**成块实心色块**（有边框有高度），不是零散红线。

### 每次动作前重新 shot

- 输入框随文字增高、发送按钮跟着下移（抖音实测 x 固定 1803–1854，y 从 827 跑到 1025）。
- 浏览器自己的提示条会让整页平移（Firefox「此网页拖累了您的 Firefox」消失后整页上移 ~46px）。
- 动态 UI 的旧标定必失效：关掉一个标签，标签行所有 x 坐标左移 ~90px。
- **定位和点击之间不能隔任何可能改变布局的事件**（提示条、懒加载、滚动、刷新后跳标签页）。列表定位完立刻点。

### 窗口与标签

- 关键点击用 `click --target <窗口名|0xID>`（显式置顶 + settle + XTest 校验，不过则 exit 4 中止）。`--target` 要 `wmctrl -l` 里的**完整标题或 0xID**（标题带"— Mozilla Firefox"之类后缀，只给前缀解析失败）。
- **`--target` 只防盖窗，不防换标签页**：同一浏览器窗口切标签时窗口 ID 不变，坐标没错、`--target` 也过了，点击却落在已切到前台的另一个页面上。**动作前必须 shot 确认画面就是我要操作的那个页面。**
- `windowactivate` 有 100–250ms「先焦点后置顶」延迟；xfwm4 的 raise_on_click 会把盖着的最大化窗口抬到前台 → 点击落在它上面。xdotool 3.20160805.1 **不认 `--window` 全局选项**（man 说有），定向投递用 `windowfocus` + 全局 XTest。
- GTK 文件选择器每次重开窗口 ID 都变，`--target` 前先 `wmctrl -l`；Ctrl+L 后的自动补全弹层会吃 Enter；中文系统的图片目录是 `~/图片` 不是 `~/Pictures`。
- **shot 里发现标签数/活动页/任务栏和上一轮不一致 → 立刻停手回报用户**，不在别人正在用的桌面上试坐标。

### 输入与粘贴

- 长文本走剪贴板：`(setsid xclip -selection clipboard -i 文件 &)` 然后 `key ctrl+v`。**每次粘贴前重新装填**（xclip 服务过一次可能已退出，前台 xclip 一退内容就没了）。
- `xusectl type` 会**吞连字符**（`creator-micro` → `creatormicro`）；混排 ASCII 会被中文输入法当拼音吃、标点丢 → 带符号的串一律走粘贴。纯中文可走 `key U+码点`。
- `xdotool key` 拒 `.` 和 `-`（当 key-sequence 分隔符）：含 `llama.cpp`、`-np` 的文本会丢字符 → 用 keysym 名 `period` `minus` `slash` `parenleft/parenright` `braceleft/braceright` `comma`。
- **type 的焦点路由非原子**：同一字符串的字符可散落到不同窗口或丢失（实测 16 字符 → 1 进 xed、7 进 terminal、8 丢失），还会顺序错位（开头三个字跑到末尾）。**重要输入后必须截图逐字核对再提交。**
- **微信 Linux 4.x 的输入框里 `type` 完全失效**（中文 ASCII 都进不去，stderr 报 `Serial number of failed request`），但直接喂 `key a` / `key U4F60` 正常 → 自己逐 keysym 喂（配方见 `LESSONS.md` 2026-10-01）。
- **起 xclip（以及一切不退出才干活的进程）也必须脱离三件套**：`printf … | setsid xclip -selection clipboard -d :0 >/dev/null 2>&1 &（**管道喂输入时不要加 `< /dev/null`**，它会把管道顶掉、粘贴内容为空）`。缺重定向时 xclip 持有 selection 不退出、还占着输出管道写端 → 命令被挂住直到下一个 xclip 顶替它，实测挂等 167–225 秒（对照实验与定位方法见 `LESSONS.md` 2026-10-31）。
- 单实例应用不是 type 的干净靶（`open xed` 不带参数会把文档挂进用户已有窗口的标签）→ 测输入要带文件参数或选多实例应用。
- 应用快捷键可能被用户重映射（本机 xed 切标签是 Alt+1..9）→ 键盘操作前先查 `~/.config/<app>/accels`。
- `tesseract`（装有 chi_sim）对微信这类小字输出乱码，**OCR 标定这条路在本机不可用**。

### 锁屏

- 本机锁屏 = **xfce4-screensaver 全屏窗口**，合成事件唤醒不了 → 用 `wake`（全局 XTest 输密码，密码见本 README 的授权备忘）。**动手前先 `shot` 探锁**（shot 直接报"屏幕已锁"），锁了先 `wake`——锁着时所有按键都喂进密码框，会把密码拼在垃圾后面解不开。
- 自动锁的时间 = xfconf `xfce4-screensaver /saver/idle-activation/delay`，**本机现在 10 分钟**（2026-11-10 核对；别再按"2–3 分钟"防它）。长 GUI 流程要防打断只需 `xset s off`（结束 `xset s on`），**不必也不该去关 DPMS**——见下条。
- 「屏保后黑屏唤不回、只能 tty 重启桌面」那个老 bug：真凶是 **light-locker**（2026-08-27/28 已 purge，并把锁屏换成 xfce4-screensaver，当时另加 `~/.config/autostart/disable-dpms.desktop` 当保险）。2026-11-10 实测：`xset dpms force off` 与自动路径（空闲→Standby→Off）唤回 **8/8 全部正常**，保险已停用（改名 `disable-dpms.desktop.disabled-20261110`，要回退就改回原名）。万一再遇唤不回：先 `xrandr --output HDMI-0 --off && xrandr --output HDMI-0 --auto` 重训链路，别急着重启桌面。

### 等待与并发

- **`$XU sleep 300` 是 300 毫秒，bash 的 `sleep 300` 是 300 秒**。动作序列里的等待一律 `$XU sleep <ms>`；要 bash 等待就写 `sleep 1` 这种小值。
- 睡错了想立刻往下走：`pkill -x sleep`。
- 命令被转后台后**别另起一条操作屏幕的命令**（两条会抢同一个鼠标）。

### 收尾

- 误开的标签页用**中键**关闭（关闭按钮太小易点空）。**绝不用 `key ctrl+w`**——它关的是当前活动标签：曾误关正在上传的抖音标签，311MB 上传全丢（2026-10-26）。
- 点错用户名/封面常会新开公开页或主页标签，发现就关掉，别攒一屏。

### 验证与判据

- 小窗口的变化会被全屏 YAVG 稀释（150×100 的 xeyes 瞳孔移动 → 全屏 YAVG≈0.02 < 阈值 0.5）→ `diff --region`。
- **exec 外部命令判成功要看产物**（文件大小/内容），不能只看 exit code：曾有 shot 的 ffmpeg 缺 `-y` 而中止、exit code 仍是 0，报"已截图 1920×1080"而文件是 0 字节（已修：加 `-y` + 0 字节即 exit 3）。
- 这版 ffmpeg：metadata 滤镜无 `:d` 选项；signalstats 输出 `YAVG=`（等号）；crop 要分别作用在每个输入上 `[0]crop[a];[1]crop[b]`。

### 残留的菜单窗会 grab 整个 X

症状：桌面/面板右键**弹不出菜单**、"键盘一直在输入"、对某些窗口的点击**没反应也不报错**
（xusectl 警告"指针下顶层窗口与激活窗口不一致"）。杀 xclip、重启 xfdesktop、补 mouseup 都没用。

真凶是一个**已 map 的 XFCE 应用菜单**（菜单对指针和键盘做 grab，挂着不 map-out，全桌面的右键和按键都被它吃掉）：

```bash
xwininfo -root -tree | grep -i whisker                    # 看到 0x… "Whisker Menu" ("wrapper-2.0")
xdotool search --name "Whisker Menu" | xargs -r xdotool windowunmap
```

**判据**：右键无菜单 + 键盘有"输入"却进不了应用 → 先 `xwininfo -root -tree` 找 map 着的菜单/弹出层
（`Whisker Menu`、`wrapper-2.0`、各种 `menu`），unmap 它，别急着重启桌面组件或注销。

### 调试时选靶

xeyes 是理想 click 靶（眼球随鼠标转、点击眨眼），但窗口小（150×100）易被盖住；
测点击优先选前台大窗口（如浏览器标签栏），或 raise 后立刻动作（xfwm4 堆叠竞争）。
