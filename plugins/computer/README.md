# computer use 插件（xusectl）

给 agent 装"手"：整屏截图（ffmpeg x11grab）+ X11 鼠标键盘事件（xdotool/XTest）。
视觉走现有 okattach（截图注入 LLM 上下文），**内核零改动**（只用 bash + /inject）。
规划与选型依据见同目录 PLAN.md。

## 用法

`XU="plugins/computer/xusectl"`

| 动作 | 说明 |
|---|---|
| `$XU shot [f] [--out f] [--settle ms]` | 整屏截图 1920×1080（f 可作位置参数=落盘路径；锁屏时会提示先 wake） |
| `$XU wake [密码]` | 解锁 xfce4-screensaver 锁屏（密码也可走 env OKHUMAN_UNLOCK_PASSWORD） |
| `$XU move x y` / `click x y [--button left\|right\|middle] [--double]` | 鼠标（坐标=屏幕真实像素） |
| `$XU click x y --target <窗口名\|0xID> [--settle ms]` | **关键点击首选**：显式置顶目标窗→settle(默认300ms)→校验指针下顶层窗确是目标（或其子窗），不过则 exit 4 中止，绝不让点击落在盖窗上（2026-09-13 Thunar 盖窗事故后新增） |
| `$XU right_click x y` / `double_click x y` / `drag x1 y1 x2 y2` | |
| `$XU scroll x y --dx N --dy N` | 滚轮（dy>0 向下，dx>0 向右） |
| `$XU type "文本"` | 输入：ASCII 走 XTest 键入；**中文等非 ASCII 逐码点走 `xdotool key U+码点`**（实测可靠，免剪贴板——本机 xclip 所有权管理有坑，勿用） |
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
- agent 用 okattach 看的是缩略图（长边 1600）→ 图里估出的坐标乘 **1920/1600 = 1.2** 才是屏幕坐标；
  或动作直接加 `--view-w 1600`，插件自动换算（`click 800 436 --view-w 1600` → 屏幕 (960,523)）。
- shot 每次都会在输出里提示换算因子。
- 默认**原图直传**（精度优先，图内坐标=屏幕坐标，免换算）；要省 token 可 `okattach xxx.png --scaled`
  （缩长边 1600，此时按注入图读坐标需乘 1.2 或加 --view-w 1600）。

## 基本循环（agent 侧，见 OkHuman/prompts/04-computer.md）

1. `shot` → `okattach <截图>` → LLM 看见屏幕
2. 决定动作（click/type/key...）
3. 执行后 `diff <上次截图> [--region ...]` 廉价验证；要细看再 shot+okattach

## 已知坑（2026-09-12 实测）

- **xdotool 3.20160805.1 不认 `--window` 全局选项**（man 说有，二进制报 unrecognized）→ 定向投递用 `windowfocus` + 全局 XTest（且比 XSendEvent 可靠，应用无法忽略合成事件）
- **本机锁屏 = xfce4-screensaver 全屏窗口**，合成事件唤醒不了 → `wake`（全局 XTest 输密码，锁屏对话框默认聚焦密码框）
- **xclip 剪贴板所有权在本机不稳**（管道挂死/owner 丢失）→ 中文输入走 U+码点 keysym，不走粘贴
- **小窗口变化在全屏 YAVG 里被稀释**（150×100 的 xeyes 瞳孔移动 → 全屏 YAVG≈0.02 < 阈值 0.5）→ diff 加 `--region`
- ffmpeg 这版：metadata 滤镜无 `:d` 选项；signalstats 输出 `YAVG=`（等号）；crop 要分别作用在每个输入上 `[0]crop[a];[1]crop[b]`
- **xfwm4 堆叠竞争**：windowraise 小窗口后可能被大窗口（Firefox 最大化）很快盖回 → 测试靶选前台大窗口（如浏览器标签栏），或 raise 后立刻动作
- xeyes 是理想 click 靶（眼球随鼠标转、点击眨眼），但窗口小（150×100）且易被盖住

## 已知坑 / 经验

- **全屏窗盖点击点 + windowactivate 置顶延迟**（2026-09-13 事故实测根因，NoMachine 已洗清）：有别的最大化窗口（如 Thunar）叠在目标窗上时，点击落在它上面、被 xfwm4 raise_on_click 抬到前台，目标窗收不到点击；且 `windowactivate` 有 100-250ms「先焦点后置顶」延迟，延迟内点击同样打偏 → 关键点击用 `click --target <窗口名|0xID>`（显式置顶+settle+XQueryPointer 校验，不过则 exit 4 中止）；或纯键盘（windowfocus+XTest key）。
- **shot 曾静默产出 0 字节"成功截图"**（2026-09-20 修复）：shot 先 0600 预创建输出文件，ffmpeg 命令行漏了 `-y` → 对已存在文件弹 "File exists. Overwrite?"，Go exec 的 stdin 是 /dev/null → EOF 当 N → ffmpeg 中止但 exit code 仍 0 → 旧代码 `err==nil && exists(out)` 判成功，报"已截图 1920×1080"文件却是空的（diff 命令带 -y 所以没踩到）。已修：shot 加 `-y` + 输出 0 字节即 exit 3。教训：exec 外部命令判成功要看**产物**（文件大小/内容），不能只看 exit code。
- Return 键、type 输入有间歇丢失/重复 → 单键操作发后验证，输入后先截图核对再提交；切标签用 ctrl+Tab 系（本机 ctrl+1 无效）。
- 详见 `LESSONS.md`（2026-09-13 Firefox 关标签事故复盘）。

## 已知坑 / 经验（2026-09-20 全量测试补录）

- **单实例应用不是 type 的干净靶**：`open xed`（不带文件参数）走 D-Bus 单实例——新文档挂到用户已有 xed 窗口的新标签里，测试文本可能落到用户编辑器（今日实测：16 字符字符串里"世"进了用户 xed 标签）。测 type 要带文件参数（`open xed /tmp/t.txt`）或选多实例应用。
- **type 的焦点路由非原子**：同一字符串的字符可散落到不同窗口或丢失（今日实测 16 字符 → 1 进 xed、7 进 terminal、8 丢失）。重要输入后必须截图/OCR 逐字核对再提交。
- **xfce4-terminal 右键菜单会悬挂**：right_click 打开的上下文菜单不自动关，悬挂期间吞掉后续按键（今日吞掉了 ctrl+c 致清行失败）。right_click 测试后补发 Escape。
- **动态 UI 的旧标定会失效**：关掉一个标签，标签行所有 x 坐标左移 ~90px。对动态布局做像素操作，点击前从最新截图重新 OCR（tesseract TSV）标定，别复用旧坐标。
- **目标应用快捷键可能被用户重映射**：本机 xed 切标签被改成 Alt+1..9（ctrl+PageUp 失效）。键盘操作前先查 `~/.config/<app>/accels`。
- **okattach --scaled 的 1600 缩略图与 native shot 不同坐标系**：shot 文件是原生 1920，`--scaled` 注入的图只有 1600 宽，图上读的坐标要 ×1.2 才是屏幕像素（或给 xusectl 传 `--view-w 1600`）。需要按像素操作的截图一律 native 直传（不带 --scaled），坐标直接读、零换算。
