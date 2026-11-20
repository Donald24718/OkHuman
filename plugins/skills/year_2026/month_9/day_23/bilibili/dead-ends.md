## 4. 已验证死路（别再踩）

| 尝试 | 结果 |
|---|---|
| `POST /x/v2/reply/modify/reply` | 已下线，网关 404 HTML（2026-08-21 确认） |
| 页内 fetch 带 `x-csrf-token` 自定义头 POST api.bilibili.com | CORS 预检被拒，"Failed to fetch" |
| 页内"简单请求"（只带 content-type）POST | 同样 404 网关页 |
| 页内改 POST www.bilibili.com 同域 | 404 |
| **服务器端 Python requests + 全套 cookie** | ✅ 唯一走通的路 |
| 由 rpid 反查 aid：`reply/info?rpid=`、`reply/oid`、`reply/parent`、`thread/reply/info`、`reply/detail` | 全不可用。`reply/info` 是 `code:0` 但 `data` 空壳（别被 code:0 骗），其余 404 / -400（2026-10-28） |
| 无登录态列自己全部视频 | `arc/search` 带 WBI 无 cookie → 412；不带 WBI → 返 HTML；`space.bilibili.com/<mid>/video` 的 HTML 无 aid（2026-10-28）。**mid 不用登录**：任意一条我自己的回复里 `member.mid` |
| `xusectl type` 输入含英文的正文 | 中文走 `key U+码点` 能进，但 ASCII 被中文输入法当拼音吃、标点丢 → 正文只能 `setsid xclip` + `ctrl+v`（2026-10-28；xusectl usage 里的 `--paste` 未实现） |

## 5. WBI 签名（只有 arc/search 需要）

**2026-10-28 复测：`reply/add` 不带 WBI 签名也 `code:0`**（旧结论「2026-10-05 起必须带签名，缺了 -101」已不成立，别再为发回复实现签名）。
`reply/del` 同样不带签名可用。
**需要签名的只有 `x/space/wbi/arc/search`**（列视频，§2.5/§2.6）：算法与 MIXIN 表见 `references/wbi.md`，
带 cookie + 签名实测 `code:0`（2026-10-28，返回 24 条视频）。
