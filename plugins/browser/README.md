# browser — 浏览器插件

OkHuman 的**能力型插件**：给只有 bash 一个工具的 agent 提供真实浏览器操作能力。
遵守[插件解耦契约](../README.md)——主程序零改动，agent 经 bash 调本插件的 CLI。

## 为什么是 verb-args CLI 而不是 code-execution

OkHuman 的 agent 只有 bash，天然适合"发离散命令"（`browserctl <port> text`），
而非"写一段 Python 让沙箱执行"（QwenPaw 的 code-execution 模型，11k 行，为多
agent 企业场景服务）。本插件走轻 CLI：agent 拼离散动作，插件内部驱动本地
headless chromium（CDP），单进程短连接，零沙箱、零额外依赖（Go + gorilla/websocket + 系统 chromium）。

## 设计原则（对应需求四条）

1. **agent 自己 CLI 开服务、自己定端口**：`browserd <port>` 起一个 headless
   chromium（CDP 监听该端口）。可开多个实例，端口 agent 自定（约定 87xx 段，
   避开 845x=OkHuman 实例、8601=cron 管理页）。**默认无头**；`--headed`
   起有头实例（弹真实窗口，本机有桌面时模型可自行选择）。`--shared`
   起共享实例（多 agent 共用一份登录态，见下节）。
2. **默认 600s 空闲自动关闭**：600s 内没有任何 browserctl 动作命令 → browserd
   自动杀 chromium 退出。`--idle <sec>` 可自定义。
3. **持久需 CLI 指定**：`browserd <port> --keep` 关自动关闭（直到显式 close/kill）。
4. **截图保存地址 agent 自定，缺省 /tmp**：`browserctl <port> screenshot --out
   <path>` 存到指定地址；不指定 `--out` 时默认
   `/tmp/browser-<port>-<ts>.png`（stdout 打印实际路径，agent 可据此取图）。

## 共享实例（多 agent 共用一份登录态）

默认每个实例是"即用即焚"的：每次起都是全新 profile，实例退出登录态即灭。
若多个 agent 要共用**一份**登录状态（登录一次、全体可用），用共享实例：

- `browserd <port> --shared`——端口 agent 自定（87xx 约定）。共享实例**持久**
  （不空闲自动关闭）：登录态活在运行的浏览器进程里（内存 + 其 /tmp profile
  目录），**进程被杀才灭**；重启机器 /tmp 清空，语义相同。
- **全局至多一个**：端口写在指针文件 `/tmp/okhuman-browser-shared.port`
  （O_EXCL 互斥），再开第二个会快速失败并提示用已存在的那个。
- 共享实例上的 `browserctl <port> close` 是**软关闭**：只关 tab（保留 1 个
  页面——页面归零会触发 chromium 清 session cookie），进程继续活、登录态保留；
  要清掉全部状态：`kill <daemon_pid>`（daemon_pid 在
  `/tmp/okhuman-browser-<port>.state` 里）。
- 临时实例（不带 `--shared`）与共享实例可并存，行为不变（全新 profile、
  空闲自动关、close 即退）。

agent 经 bash 找/起共享实例：

```bash
SHARED_PORT=$(cat /tmp/okhuman-browser-shared.port 2>/dev/null)
if [ -z "$SHARED_PORT" ]; then
  # 还没有共享实例：起一个（端口自选）
  nohup plugins/browser/browserd 8731 --shared \
    </dev/null >/tmp/browser-8731.log 2>&1 &
  sleep 3   # 等 CDP 就绪
  SHARED_PORT=8731
fi
plugins/browser/browserctl "$SHARED_PORT" open https://example.com
```

**多 agent 同实例的 tab 隔离**：调 browserctl 时设环境变量
`OKH_BROWSER_AGENT=<自己的实例id>`（限字母数字 _ -），"当前活跃 tab"记录就按
agent 分文件，互不串；也建议各 agent 用 `new` 拿自己的 tab、用
`tab <index>` / `close_tab <index>` 显式操作。

## 登录态放哪儿：`--shared` vs `--data-dir`（2026-10-04 补，README 原先没写 `--data-dir`）

| 需求 | 用什么 | 登录态活多久 |
|---|---|---|
| 多个 agent 共用一份、当次开机内复用 | `--shared` | 活在**运行的进程**里；杀进程 / 重启机器即灭 |
| 扫码登录一次，**跨重启 / 重开机**长期复用 | `--data-dir <dir>` | 画像落盘到该目录；cleanup 只删临时目录，**持久目录保留** |
| 一次性抓取，不想留痕 | 都不带 | 每次起独立 /tmp 临时画像，退出即删 |

两者可叠加（`--shared --data-dir <dir>`）。`--data-dir` 的目录里就是账号凭据：
**放在插件目录之外**（否则会被插件索引/镜像一起带走），
权限 600，不进 git。

扫码登录一次、之后长期用：

```bash
PROF=$HOME/browser-profiles/example-site; mkdir -p "$PROF"; chmod 700 "$PROF"
(setsid plugins/browser/browserd 8751 --data-dir "$PROF" --idle 3600 </dev/null >/tmp/browser-8751.log 2>&1 &)
sleep 3
plugins/browser/browserctl 8751 open https://example.com/login
plugins/browser/browserctl 8751 screenshot --out /tmp/qr.png   # 把截图交给用户扫码
plugins/browser/browserctl 8751 cookies example.com                   # 扫完导出含会话 cookie 的 Cookie 头
```

⚠ 路径含空格（`Frequently Used Plugins`）——**示例里一律带引号**。不加引号的 `cd %s && setsid %s`
这类拼接脚本会在含空格目录上短路（okmon 起服务 2026-09-21 起就一直失效即此因，已修）。

## 用法（agent 经 bash）

```bash
# 起一个实例（端口 8701），后台跑，日志落 /tmp
nohup plugins/browser/browserd 8701 \
  </dev/null >/tmp/browser-8701.log 2>&1 &

# 起一个有头实例（弹真实窗口，便于人眼观察；缺省无头）
nohup plugins/browser/browserd 8702 --headed \
  </dev/null >/tmp/browser-8702.log 2>&1 &

# 打开页面（等加载完成）
plugins/browser/browserctl 8701 open https://example.com

# 读正文（不带选择器 = 整页；带选择器只取该子树正文，长页面别整页灌上下文）
plugins/browser/browserctl 8701 text
plugins/browser/browserctl 8701 text "article"
plugins/browser/browserctl 8701 text "text:权限"

# 检查元素（零副作用：tag/class/文本/位置/可见性/控件状态，点击前先侦察）
plugins/browser/browserctl 8701 info "a#some-link"
plugins/browser/browserctl 8701 info "text:登录"

# 按 CSS selector 或可见文本（text:txt）点击元素（真实鼠标事件）。
# 报告：页面 url/标题变化；window.open 开的新 tab（index/url/标题，轮询最长 2s）；
# url/标题没变但正文长度变了（"页面内容变化"，SPA 页内切换/手风琴展开）
plugins/browser/browserctl 8701 click "a#some-link"
plugins/browser/browserctl 8701 click "text:机器人"

# 往输入框填文本（默认追加；--clear 先清空再输）
plugins/browser/browserctl 8701 type "#search" "hello"
plugins/browser/browserctl 8701 type "#search" "world" --clear

# 按键（Enter/Tab/Escape/方向键/字母，组合键如 Control+a）
plugins/browser/browserctl 8701 press Enter
plugins/browser/browserctl 8701 press Control+a

# 页面里跑任意 JS，打印返回值（万能逃生舱：任何没专门做的动作都能用它）
plugins/browser/browserctl 8701 eval "document.querySelector('#x').textContent"

# 等元素出现 / 等文本出现 / 等毫秒（动态页面/SPA 点击后内容异步加载，不 wait 会扑空）
plugins/browser/browserctl 8701 wait "#loaded-el" 15000
plugins/browser/browserctl 8701 wait "text:加载中" 5000
plugins/browser/browserctl 8701 wait 500

# 滚动页面（up/down/top/bottom）或滚到某元素
plugins/browser/browserctl 8701 scroll down
plugins/browser/browserctl 8701 scroll "#footer"

# 浏览器后退 / 前进
plugins/browser/browserctl 8701 back
plugins/browser/browserctl 8701 forward

# 下拉框选值（按 value 或可见文本匹配）
plugins/browser/browserctl 8701 select "#city" "two"

# 悬停元素（触发 tooltip / 下拉菜单）
plugins/browser/browserctl 8701 hover "#menu"

# 文件上传（input[type=file]）
plugins/browser/browserctl 8701 file_upload "#upload" /tmp/a.txt

# 以 - 开头的参数用 -- 分隔
plugins/browser/browserctl 8701 type "#in" -- --weird

# 多标签页：开新 tab / 列 tab / 切换 / 关 tab（其余动作默认作用在当前活跃 tab）
plugins/browser/browserctl 8701 new https://iana.org   # 开新 tab（成为活跃）
plugins/browser/browserctl 8701 tabs                   # 列出所有 tab（标活跃）
plugins/browser/browserctl 8701 tab 1                  # 切到第 1 个 tab
plugins/browser/browserctl 8701 close_tab 1            # 关第 1 个 tab
plugins/browser/browserctl 8701 text                   # 读当前活跃 tab 正文
plugins/browser/browserctl 8701 info "text:登录"       # 无副作用检查元素

# 按视口截图的像素坐标点击（2026-09-12）：图内像素 → CSS 坐标自动换算
# （同会话实时 innerWidth 比例映射，含 dpr；截图与视口差 >5% 会警告"过时"）
# 坐标是**原截图文件**里的像素——若图经 okattach 缩放过，先按 原图宽/缩略宽 放大
plugins/browser/browserctl 8701 click_at 640 310 --shot /tmp/shot.png

# 截图存到 agent 指定的地址；--full 截整页（突破视口，懒加载页只含已加载部分）
plugins/browser/browserctl 8701 screenshot --out /tmp/shot.png
plugins/browser/browserctl 8701 screenshot --full --out /tmp/full.png
# 或不指定 --out，默认存 /tmp/browser-8701-<ts>.png（stdout 打印实际路径）
plugins/browser/browserctl 8701 screenshot

# 导出浏览器 cookie（含 httpOnly，如 B 站 SESSDATA——document.cookie 拿不到，只能走 CDP
# Network.getAllCookies）。默认输出一行 Cookie 头格式 name=value; ...；--json 出完整字段
plugins/browser/browserctl 8701 cookies example.com
plugins/browser/browserctl 8701 cookies example.com --json
# ⚠ 这些值是账号凭据：落文件要 chmod 600、用完 rm，别进日志/仓库/skill 目录

# 查状态
plugins/browser/browserctl 8701 status

# 用完关闭（browserd 5s 内退出并清理）
plugins/browser/browserctl 8701 close

# 持久实例（不自动关）
nohup plugins/browser/browserd 8702 --keep </dev/null >/tmp/browser-8702.log 2>&1 &
```

## 架构

```
agent ──bash──> browserctl ──WS/CDP──> chromium(headless, :port)
                      │ 刷心跳
                      ▼
                 browserd（守护：空闲 600s 杀 chromium 退出 / --keep 持久）
```

- **browserd**：起 chromium（CDP 监听指定端口）、写心跳文件、空闲超时看门狗。
  单实例单进程，不常驻全局——agent 要几个实例就起几个，各自独立。
- **browserctl**：动作 CLI，每次短连接 CDP 执行一个动作后退出。每个动作刷
  心跳（让 browserd 知道实例还在被用）。
- **多 tab 模型**：除 tabs/new/tab/close_tab 外，所有动作默认作用在当前活跃 tab
  （targetId 存状态文件；index 按持久化顺序 `.taborder` 计算，既有 tab 不动、
  新开追加末尾，不受 /json/list 枚举顺序漂移影响）。
- **输出约定（人类浏览视角）**：改页面类动作（open/click/type/press/select/hover/
  back/forward）执行完自动附完整屏幕报告——全部 tab 列表 + 当前页 url/标题/整页
  正文（像人动作后看到整屏）；scroll/file_upload 附 tab 列表；READ 类动作只输出
  所请求的内容。选择器支持 `text:txt` 别名（按可见文本定位，叶子精确匹配优先，
  命中元素会随输出回显，防止点错目标）。
- **状态文件**（/tmp，进程生命周期同义，重启清空）：
  - `okhuman-browser-<port>.hb` 心跳（mtime 即最后活动时间）
  - `okhuman-browser-<port>.state` `{ daemon_pid, chrome_pid, keep, started_at }`
  - `okhuman-browser-<port>.active[-<agent>]` 当前活跃 tab 的 targetId（多 agent 按环境变量 OKH_BROWSER_AGENT 分文件）
  - `okhuman-browser-<port>.taborder` tab index 持久化顺序（targetId 数组）
  - `okhuman-browser-<port>.stop` close 信号

## 治理（轻量版，借鉴 QwenPaw 副作用分级）

v1 只做**副作用分级 + 日志**，不上审批链（个人单机场景）：
- **READ**：`text` / `info` / `status` / `screenshot` / `wait` / `tabs`（只读/等待/列 tab，无副作用）
- **STATE_CHANGE**：`open` / `navigate` / `click` / `click_at` / `press` / `scroll` / `back` /
  `forward` / `select` / `hover` / `new` / `tab` / `close_tab`（改浏览器状态/触发导航/
  管理 tab，但仍在本实例内，不向外部发数据）
- **TRANSMIT**：`type` / `eval` / `file_upload`（往表单写数据 / 跑任意 JS / 上传文件——
  可能向外部提交数据）。个人单机场景 agent 可放心用；将来若接入多用户/共享环境，
  对 TRANSMIT 动作按 QwenPaw 思路加审批/handoff。

## handoff（借鉴 QwenPaw）

遇到登录/验证码/2FA，agent 应停止自动操作、把控制权交回人（在回复里说明卡在哪、
已验证了什么）。本插件 v1 不强制拦截——headless 下人工接管成本高，agent 自行判断。
将来可加 `browserctl <port> handoff` 返回明确信号。

## 依赖

- Go（`cmd/browserctl`、`cmd/browserd` 两个独立二进制；依赖仅 gorilla/websocket）
- 系统 chromium（默认 `/usr/bin/chromium`，`OKH_BROWSER_CHROME` 环境变量可覆盖）

## 网页测试（2026-10-07 加）

给"我自己写完前端，自己开浏览器验"这条链用。三条命令：

```bash
browserctl <port> count  "#stage .l"        # 数元素个数
browserctl <port> errors [--clear] [--all]  # 页面 JS 异常 / console 报错
browserctl <port> eval   "<js>"             # 任意求值（可模拟输入并提交）
```

`errors` 由 **daemon 侧**持续记录（`/tmp/okhuman-browser-<port>.errors`，JSONL）：
browserctl 是短连接，命令之间没人接事件，异常只能由常驻的 browserd 收。
**有报错 → 退出码 1**，测试脚本可以直接 `&&`/`if` 判，不用肉眼比对输出。

一条完整的自检（实测跑通）：

```bash
browserctl 8732 open http://127.0.0.1:8460/
browserctl 8732 eval '(()=>{const i=document.getElementById("in");
  i.value="来杯麦酒";document.getElementById("f").requestSubmit();})()'
sleep 6
browserctl 8732 count "#stage .l"   # 行数变多 = 玩家那句话真落地了
browserctl 8732 errors              # 干净页退出码 0；有 JS 异常退出码 1
```

实测：干净页 `errors` 退出码 0；故意放一个 `undefined_fn()` 的页面退出码 1，
并把 `Uncaught ReferenceError: undefined_fn is not defined @file:1:39` 连行号打出来。

去噪两条（都是实测踩出来的）：
- 同一个异常 chromium 会重放（重新 enable、切 tab 都会再来一遍）→ **写入侧按 kind+text 去重**；
- 资源 404（favicon 之类）默认不计入，否则真异常被埋 → 要看全部用 `--all`。

已知边界（不装全）：`console.error` 只有在 daemon 挂上之后**新产生**的才收得到——
页面加载瞬间打的那条可能漏（异常不会漏，Runtime.enable 会补发）。
