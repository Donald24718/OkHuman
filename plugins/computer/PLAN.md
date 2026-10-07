# computer use 插件规划（2026-09-12，研究 Anthropic CUA / OpenAI CUA / UI-TARS / agent-vision-toolkit 后定）

## 一句话

给 agent 装上"手"：整屏截图（ffmpeg x11grab）+ X11 鼠标键盘事件（xdotool），
视觉理解走现有 Qwen-VL（okattach 注入），**内核零改动**（只用 bash + /inject 两个已有通道）。

## 别人的方案对比（研究结论）

| 方案 | 范式 | 对我们的启示 |
|---|---|---|
| Anthropic Computer Use | 工具循环：截图→模型选动作→执行→再截图；动作集 screenshot/click/double_click/right_click/move/drag/type/key/scroll/wait/mouse_down/up/hold_key/cursor_position/bash；**坐标归一化到 1000×1000 再由 harness 缩放到真实分辨率** | 循环模式和动作集直接借鉴；坐标归一化思想解决"模型看的是缩略图"问题 |
| OpenAI CUA (Operator) | 端到端微调 VLM（CUA-8B，Llama-3.3-70B 基座），模型原生输出动作 | 需要专用微调模型，**不是我们的路**（Qwen-VL+工具循环已够用） |
| agent-vision-toolkit (1.2k★) | "视觉能力在 harness 不在模型"：CLI 工具箱（ground/detect/trace/crop/glance + 像素 diff）+ 技能文件教 agent 用 | **验证了我们的微内核+插件哲学**；像素 diff 做廉价验证是好点子 |
| UI-TARS-desktop (39k★) / bytebot | 完整产品：自带 operator 运行时（bytebot 给 agent 一个**隔离虚拟桌面**） | 隔离桌面是"安全但非本机"路线；我们走"真实桌面+窗口限定+用户授权"路线 |

## 架构（微内核，与 okattach/browser 同构）

```
agent (Qwen-VL, 8080)
  │ bash
  ├─ xusectl shot ──────── ffmpeg x11grab :0.0 ──> 1920×1080 PNG
  │    └─ okattach 注入 ──> agent 看图（默认原图直传；--scaled=1600）
  ├─ xusectl click x y ─── xdotool (XTest) ──> 目标窗口
  ├─ xusectl type "..." ── 短文本 XTest / 中文走 xclip 剪贴板+ctrl+v
  └─ xusectl diff prev.png ── ffmpeg blend=difference+signalstats ──> "变了没"
```

### xusectl 动作集（对齐 Anthropic 命名，降低 agent 学习成本）
- `shot [f] [--out f] [--settle ms]`：截图（f 可作位置参数=落盘路径；--settle 先等动画稳定）
- `move x y` / `click x y [--button] [--double]` / `right_click x y` / `double_click x y`
- `drag x1 y1 x2 y2`
- `scroll x y --dx N --dy N`（滚轮）
- `type "text" [--paste]`（--paste 强制剪贴板通道，中文必走）
- `key <combo>`（如 `ctrl+c`、`alt+tab`、`super`）
- `hold <combo> <ms>`（按住不放）
- `focus <窗口名|wmctrl-id>`（后续事件限定到该窗口）
- `open <cmd>`（setsid 起应用，如 `open gedit`）
- `diff <prev.png>`：与上张截图像素差异（YAVG + 变化区域提示）——廉价验证，模型看图确认

### 坐标空间（click_at 踩坑教训的升级版）
- 真实屏幕 1920×1080；okattach 默认原图无损直传（agent 看到 1920×1080；`--scaled`=缩长边 1600 省 token）
- **换算交给插件**：`click --view-w 1600 1320 720` → 插件乘 1920/1600 → 真实 (1584,864)
- shot 文件恒为原生分辨率；okattach 默认原图直传（坐标免换算）；仅 --scaled 时按缩略图读坐标需乘 1.2 或动作加 --view-w 1600
- 默认坐标=真实屏幕像素（agent 自己算也行，两种方式都收）

### 安全（真实桌面，舍友在睡觉）
1. **窗口限定优先**：`focus` 后事件用 `xdotool --window <wid>` 只发目标窗口；全局事件（alt+tab 等）必须显式 `--global`
2. type/key 执行前 stderr 回显动作（可审计），危险组合（ctrl+alt+del 等）直接拒
3. 起应用一律 setsid 脱离；视频/音乐页操作前先静音（browser 教训）
4. 任务级授权：agent 在系统提示词（prompts/04-computer.md）里被要求"多步桌面操作前先跟用户确认"
5. 速率护栏：同一目标 1s 内 >5 次 click 警告（防 agent 死循环狂点）

### 验证回路（双保险）
- 廉价：`diff` 像素差异（YAVG≈0 = 没变 → agent 知道点击没生效，换策略）
- 可靠：新截图 okattach 注入，模型亲眼确认（browser 测试已证明这条路）

## 分期

- **P0（✅ 2026-09-12 完成，实测全过）**
  1. 用户执行：`sudo apt install xdotool xclip`（sudo 要密码，只能用户来）
  2. 写 xusectl.ts（shot/move/click/type/key/focus/open/diff，~400 行）
  3. prompts/04-computer.md（教 agent：何时用、坐标换算、先 focus 后操作、危险动作先问用户）
  4. 测试靶：自起 `xterm`（setsid，专用窗口），窗口限定内 click+type "hello"+diff 验证
- **P1**：drag/scroll/right/double/hold；wmctrl 多窗口编排；端到端任务测试
  （"开个终端输入 echo hi 回车，截图给我看"——agent 自己驱动全循环）
- **P2（可选）**：pactl 音量/应用静音；OCR 辅助（长截图识别，avt 思路）；
  坐标 grounding 提示（模型指哪打哪的精度调优）

## 明确不做

- Wayland（本机是 X11/Xfwm4）；远程机器；模型微调（CUA 路线）；
  隔离沙箱桌面（bytebot 路线——要隔离再单独做 VM 实例）

## P0 实测结论（2026-09-12）

- shot/wake/open/focus/type/key/hold/click/drag/scroll/diff 全部实测通过
- **type 通道修正**：本机 xclip 剪贴板所有权不稳（管道挂死/owner 丢失）→ 中文改走
  `xdotool key U+码点` 逐码点发送（混合中英文单行输入验证✓，免剪贴板）
- **xdotool --window 全局选项这版不认**（man 与二进制不符）→ 定向投递 = windowfocus + 全局 XTest
- **diff 必须支持 --region**：小窗口变化在全屏 YAVG 里被稀释到 0（xeyes 瞳孔 → YAVG≈0.02）；
  且本机 ffmpeg 的 metadata 滤镜无 :d 选项、signalstats 输出 YAVG=（等号）、crop 要分作用每个输入
- 锁屏 = xfce4-screensaver 全屏窗口，合成事件唤醒不了 → wake 动作（全局 XTest 输密码）
- 测试靶教训：小窗口（xeyes）易被 xfwm4 盖回堆叠后面 → 验证点击用前台大窗口（Firefox 标签栏
  点击切页 + diff --region YAVG=186 验证✓）
- prompts/04-computer.md 已热加载进 8451（/prompts/reload，无需重启）

## 风险

| 风险 | 缓解 |
|---|---|
| 中文输入 XTest keysym 不可靠 | 默认走 xclip+ctrl+v 粘贴通道 |
| 动到用户正在用的窗口 | 窗口限定 + 任务前授权 + 动作回显 |
| 截图时页面在动 | --settle 等待参数 |
| 每轮 ~1800 图像 token | 循环任务控制步数；diff 廉价预检减少盲截 |
| xdotool 装不上（sudo 密码） | 备选：用户终端跑一条命令；或 ydotool（需 uinput 权限，更麻烦） |

## 已知坑（2026-09-12 实测）
- **XTest 滚轮事件对 Whisker 应用列表无效**：dy=12/80 在列表区内滚动均 0 位移（只有悬停高亮变化，YAVG<1）；同一 scroll 原语在 Firefox 网页正常（YAVG=20）。GTK ListBox 疑似不认 XTest 合成滚轮。
  绕行：菜单类列表用 `key Page_Down`/`Page_Up`（键盘事件可达菜单窗口）或用搜索框过滤。
- **判断滚轮是否生效看 YAVG**：真滚动 YAVG>>1；YAVG<1 = 只动了指针/高亮，列表没动（2026-09-12 翻菜单时把 8.99 的高亮变化误判成"翻动了"，白滚 4 轮）。
- **点击坐标要用全分辨率截图量**：从 okattach 缩略图（1600 宽）目测 y 会偏一行（11 个分类点错 5 个）。okattach 现已默认原图直传（2026-09-13 晚翻默认），此坑已彻底规避。
- **scroll 的 dy 单位 = 滚轮档数，不是像素**（2026-09-12 实测）：--dy 300 在 Firefox 网页上直接冲到底（200 行×48px 的页面，scrollY 0→8924px）；每档≈100px 且带惯性。精确滚动用小 dy（5–20）；粗滚动 dy 会被页面边界钳制（触底/触顶后不再动，属正常）。
- YAVG 阈值在均匀内容上会偏小（200 行白灰条纹全换了，YAVG 才 3.7）——YAVG<1 仍可靠判"无变化"，但 YAVG 小≠没大动，关键帧还是要看图。

## 2026-09-12 focus/type 绑定坑（computer use 实测）
- xusectl 的 type/key 发到 FOCUS_FILE 记录的窗口（上次 focus 的），**click 不会更新 FOCUS_FILE**：点了别的窗口后必须重新 `focus <wid>`，否则键鼠事件投错窗口（实测把 URL 打进了终端）。
- `focus` 按名字匹配要求**精确标题**（wmctrl -l 为准；标题里冒号后有空格这种细节会致"找不到窗口"）。
- **最稳：`focus 0x<窗口ID>`**（wmctrl -l 拿 ID），直接传窗口 ID 跳过名字搜索。
- 同名多窗口（多个 Terminal）时按名字 focus 必歧义，用 ID。
- Firefox 地址栏：focus 后 ctrl+l → type URL → Return，7s 后再补一次 Return（自动补全竞态）实测一次过。
