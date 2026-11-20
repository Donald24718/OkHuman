## 环境事实（所有子模块共用，先核对这些，别假设）


| 项 | 值 |
|---|---|
| 账号 | OkHuman, mid `544275079`（账号由 OkHuman agent 托管） |
| 登录态 | 两条来源：① browser 插件的持久画像（`--data-dir`，扫码一次跨重启存活）② **主人桌面 Firefox 画像的 sqlite**（§1 路线 B，2026-10-04 实测）。本机当前**没有**①的画像，实际走的是② |
| 发帖方式 | **服务器端 Python requests 直连**（见下方"已验证死路"，页内 POST 走不通） |
| 发回复端点 | `POST https://api.bilibili.com/x/v2/reply/add`（**不需要 WBI 签名**，2026-10-28 实测 code:0；旧结论"必须签名"作废。签名只有 `arc/search` 需要，见 §5） |
| 删除端点 | `POST https://api.bilibili.com/x/v2/reply/del`（实测 code:0） |
| 读评论端点 | `GET https://api.bilibili.com/x/v2/reply?type=1&oid=<AID>&pn=1&ps=20&sort=1` |
| oid 是什么 | 视频的 **aid**（数字），不是 bvid 也不是 cid |

## 1. 拿登录态（每次操作前做）

**先探现状再选路线**（2026-10-04 实测：本机已无持久 B 站画像，旧 `docker` 树整个没了）：

```bash
find <数据盘> ~ -maxdepth 6 -name "cookies.sqlite" 2>/dev/null   # 有哪些画像
ls -d <数据盘>/*/plugins/browser/profiles/* 2>/dev/null                # 有没有 --data-dir 持久画像
```

- **路线 A（推荐，一次配好长期省事）**：起持久画像扫码一次，登录态跨重启存活：
  ```bash
  PROF=~/browser-profiles/bili; mkdir -p "$PROF"; chmod 700 "$PROF"   # 画像=凭据，放插件目录外
  (setsid "plugins/browser/browserd" 8751 \
    --data-dir "$PROF" --idle 3600 </dev/null >/tmp/browser-8751.log 2>&1 &)
  ```
  → `browserctl 8751 open https://passport.bilibili.com/login` → `browserctl 8751 screenshot --out /tmp/bili-qr.png`
  → okattach 发给主人扫 → `browserctl 8751 cookies bilibili` 导出。
- **路线 B（无画像时的兜底，2026-10-04 实测 code:0）**：从**主人桌面 Firefox 的画像**只读取出 cookie。
  Firefox 已登录 B 站（§9 环境事实），cookie 在 sqlite 里明文存（含 httpOnly 的 SESSDATA）：
  ```bash
  P=~/.config/mozilla/firefox/zv0bthyg.default-release   # 画像名会变，先 find
  cp $P/cookies.sqlite $P/cookies.sqlite-wal /tmp/               # -wal 必须一起拷，最新评论期 cookie 在 WAL 里
  ```
  ⚠️ 表名必须是 **`moz_cookies`**（查 `cookies` 表报 `no such table`，会被误判成"取不到/被加密"），
  且 **`-wal` 必须和 sqlite 一起拷**（只拷主库会缺表/缺最新 cookie）。这两条错一条就白判一次"此路不通"。
  ```python
  # 列名是 expiry（不是 expiration），host 取 '.bilibili.com'
  rows=c.execute("select name,value from moz_cookies where host='.bilibili.com' and name in "
                 "('SESSDATA','bili_jct','DedeUserID','sid','buvid3','buvid4','bili_ticket')").fetchall()
  open('/tmp/bili-cookie.txt','w').write('; '.join(f"{n}={v}" for n,v in rows))  # 随后 chmod 600
  ```
  验证 `GET /x/web-interface/nav` → `isLogin:true`（实测 mid 544275079 / uname OkHuman / Lv5）。
  **用完必须删**：cookie 文件 **和** sqlite 副本（`/tmp/cookies.sqlite*` 改名过的也要删），
  `rm -f /tmp/bili-cookie.txt /tmp/ff-c.sqlite*`。别把 SESSDATA 打进日志/仓库/skill。

`document.cookie` 里**没有 SESSDATA**（httpOnly），走浏览器时必须用 CDP `Network.getAllCookies`——browser 工具已内置命令：

```bash
BP="plugins/browser"
"$BP/browserctl" <port> cookies bilibili        # 加 --json 出完整字段（domain/httpOnly/path）
```

返回 bilibili 域全部 cookie（含 SESSDATA 等 httpOnly）的 Cookie 头格式（`name=value; ...` 一行）；加 `--json` 出完整字段（domain/httpOnly/path）。

**完整登录流（还没有 SESSDATA 时；命令与 §1 路线 A 同一套 Go 二进制）**：

1. 起**持久画像**实例（登录态跨重启存活；默认临时画像一关就没）：
   ```bash
   BP="plugins/browser"
   PROF=~/browser-profiles/bili; mkdir -p "$PROF"; chmod 700 "$PROF"
   (setsid "$BP/browserd" 8751 --data-dir "$PROF" --idle 3600 </dev/null >/tmp/browser-8751.log 2>&1 &)
   ```
2. `"$BP/browserctl" 8751 open https://passport.bilibili.com/login`
3. 截图二维码发给主人扫：`"$BP/browserctl" 8751 screenshot --out /tmp/bili-qr.png`，
   再 `"plugins/view_image/okattach" /tmp/bili-qr.png "B站登录二维码"` 注入会话
4. 扫完验证 `GET /x/web-interface/nav` 为 `code:0 且 data.isLogin:true`，然后导出：
   `"$BP/browserctl" 8751 cookies bilibili`
5. 把含 `SESSDATA` + `bili_jct` 的 cookie 写进文件（`k1=v1; k2=v2` 格式，bash 直写纯 UTF-8），`chmod 600`。**SESSDATA 是账号令牌，别写进 skill 目录/仓库，放 /tmp 用完即弃。**

验证登录（每次操作前都做）：`GET https://api.bilibili.com/x/web-interface/nav`（带 cookie），`code:0 且 data.isLogin:true` 才继续。

> browser 插件的 `cookies`（CDP `Network.getAllCookies`）与 `browserd --data-dir <画像目录>` **在插件 README 里没有写**，
> 但都在代码里、可用（2026-10-28 实测）。别只读 README 就断定没有。
