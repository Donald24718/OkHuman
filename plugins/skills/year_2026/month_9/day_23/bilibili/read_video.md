# read_video — 读一个 B 站视频讲了什么（2026-11-17 实测，BV18gWm65Ewk，478s）

给 BV 号或 `b23.tv` 短链，要拿到它讲的内容。**画面怎么抽帧、拼几张、注入有什么限制 → 见 `video-frames` skill**；本文只写 B 站特有的部分。回评论见 `reply.md`，投稿见 `upload.md`。

- 短链换 BV：`curl -o /dev/null -w '%{redirect_url}' https://b23.tv/xxxx`，从返回里取 `BV…`。
- `GET /x/web-interface/view?bvid=` → `aid`/`cid`/`desc`/`duration`。不需 WBI 签名，但**必须带浏览器 UA**，否则 -352。
- **字幕接口不带 cookie 时 `data.subtitle.subtitles` 是空数组；同一请求带 cookie 返回 8 条。空数组 ≠ 这个视频没有字幕**——先带 cookie 再判（取法见 `SKILL.md` §1）。
- 取哪条：`lan=ai-zh`、`ai_type=0` 是服务端原始语音转写；`ai-en`/`ai-ja`/`ai-es`…（`ai_type=1`）是它的机器翻译，不要用；UP 自己上传的人工字幕是 `lan` **不带** `ai-` 前缀的那些（本片没有）。
- 字幕正文：`subtitle_url` 以 `//` 开头要补 `https:`；内容在 `body[]`，字段 `from`/`to`（秒）+ `content`。478s 实测 251 段、正文 4991 字符，一次能读完。
- **转写：内容可信，专名不可信**（实测把 Claude Opus 4.5 转成「OPPOS5.5」「office5.5」「app s5.5」）。人名、模型名、数字别照抄——改用画面核对，见 `video-frames`。
- 要画面就自己取流：`GET /x/player/playurl?bvid=&cid=&qn=32&fnval=1` → `data.durl[0].url`，不带 cookie 也有（360P）。带 cookie 最高 **720P＝qn 64**：请求 `qn=80` 会被静默降到 64，`support_formats` 里写明上限。
- 自己下音频转写走 `asr` 插件（`POST :8090/v1/audio/transcriptions`，见其 README；**未实测**）。简介（`view` 的 `desc`）与 UP 自己的评论（`GET /x/v2/reply?type=1&oid=<aid>` 里 `mid`＝UP）只作补充。
