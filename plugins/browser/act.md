# browser — 页面上的动作、选择器与输出约定

## 动作（`browserctl <port> <verb>`）

```bash
".../browserctl" 8701 open https://example.com          # 打开页面（等加载完成）
".../browserctl" 8701 text                              # 整页正文（长页面别整页灌上下文）
".../browserctl" 8701 text "article"                    # 只取该子树
".../browserctl" 8701 text "text:权限"                  # 按可见文本定位
".../browserctl" 8701 info "a#some-link"                # 零副作用侦察：tag/class/文本/位置/可见性/控件状态
".../browserctl" 8701 click "text:机器人"               # 按 CSS selector 或 text:txt 点击（真实鼠标事件）
".../browserctl" 8701 type "#search" "hello"            # 填输入框（默认追加；--clear 先清空）
".../browserctl" 8701 press Control+a                   # 按键（Enter/Tab/Escape/方向键/组合键）
".../browserctl" 8701 eval "document.querySelector('#x').textContent"   # 任意 JS，逃生舱
".../browserctl" 8701 wait "#loaded-el" 15000           # 等元素 / 等文本 / wait 500（毫秒）
".../browserctl" 8701 scroll down                       # up/down/top/bottom 或 scroll "#footer"
".../browserctl" 8701 back ; ".../browserctl" 8701 forward
".../browserctl" 8701 select "#city" "two"              # 下拉框按 value 或可见文本
".../browserctl" 8701 hover "#menu"                     # 触发 tooltip/下拉
".../browserctl" 8701 file_upload "#upload" /tmp/a.txt
".../browserctl" 8701 type "#in" -- --weird             # 以 - 开头的参数用 -- 分隔
".../browserctl" 8701 new https://iana.org              # 开新 tab（成为活跃）/ tabs / tab 1 / close_tab 1
".../browserctl" 8701 click_at 640 310 --shot /tmp/shot.png   # 按截图像素坐标点击
".../browserctl" 8701 screenshot --out /tmp/shot.png          # --full 截整页；不带 --out 落 /tmp/browser-<port>-<ts>.png
".../browserctl" 8701 cookies bilibili                        # 含 httpOnly；--json 出完整字段
```

- **多 tab 模型**：除 `tabs/new/tab/close_tab` 外所有动作作用在**当前活跃 tab**（targetId 存状态文件；index 按 `.taborder` 计算，既有 tab 不动、新开追加末尾，不受 /json/list 枚举顺序漂移影响）。
- **click 的报告**：页面 url/标题变化；`window.open` 开的新 tab（index/url/标题，轮询最长 2s）；url/标题没变但正文长度变了会报"页面内容变化"（SPA 页内切换/手风镜展开）。
- **输出约定（人类浏览视角）**：改页面类动作（open/click/type/press/select/hover/back/forward）执行完自动附完整屏幕报告——全部 tab 列表 + 当前页 url/标题/整页正文；scroll/file_upload 附 tab 列表；READ 类动作只输出所请求的内容。选择器支持 `text:txt` 别名（按可见文本定位，叶子精确匹配优先，命中元素会随输出回显，防止点错目标）。
- **click_at 坐标**：是**原截图文件**里的像素，自动换算成 CSS 坐标（同会话实时 innerWidth 比例映射，含 dpr；截图与视口差 >5% 会警告"过时"）；**若图经 okattach 缩放过，先按 原图宽/缩略宽 放大坐标**。
- **cookies 是账号凭据**：落文件要 chmod 600、用完 rm，别进日志/仓库/skill 目录。`SESSDATA` 这类 httpOnly 只能走 CDP `Network.getAllCookies`（`document.cookie` 拿不到）。

## 副作用分级（v1 只分级 + 日志，不上审批链）

- **READ**：`text` / `info` / `status` / `screenshot` / `wait` / `tabs`
- **STATE_CHANGE**：`open` / `navigate` / `click` / `click_at` / `press` / `scroll` / `back` / `forward` / `select` / `hover` / `new` / `tab` / `close_tab`（改浏览器状态/触发导航/管 tab，仍在本实例内，不向外部发数据）
- **TRANSMIT**：`type` / `eval` / `file_upload`（往表单写数据 / 跑任意 JS / 上传文件——可能向外部提交数据）。个人单机场景可放心用；将来接入多用户/共享环境时对 TRANSMIT 加审批/handoff。

## 交回给人的时机

遇到登录/验证码/2FA：应停止自动操作、把控制权交回人（回复里说明卡在哪、已验证了什么）。插件 v1 不强制拦截——headless 下人工接管成本高，由我判断。**先按 `captcha.md` 换模式，别直接放弃**。

## 失败与限制

- **`text` 返回空 / 只有导航壳**：动态页要 `wait`（不 wait 会扑空）；正文在组件里就带选择器只取子树，或 `eval` 点站内标签再 `text`；表是图/组件时只能 `screenshot` + okattach 读（厂商规格表实测只能这么读出来）。
- **取正文的顺序**：`open` → `text`；`text` 空则 `eval` 点标签再 `text`；仍不行 → 截图读。
- **点错/点不中**：先 `info` 侦察（可见性、位置、是否被盖），再用 `text:` 别名（会回显命中元素）；坐标类点击用 `click_at` 而不是自己换算。
- **browserctl 只连自己 `browserd` 起的端口**：手工 chromium 的 `--remote-debugging-port` 不认（`实例 :9333 未起`），有头场景取正文走 computer 插件，见 `captcha.md`。
