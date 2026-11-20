# okattach — 用法、目标实例、依赖与限制

把**图片**注入 OkHuman 上下文：默认原图 native（不缩放不压质）；或从视频抽 t 秒单帧压宽后注入为图片。内核按 part 类型透传（`types.go` 注释：主程序对 part 类型零感知），**能不能被后端透出由后端决定**——当前 :8080 只支持 image_url，`input_video` 被忽略。

## 0. 什么时候该用它（先想这个，不要先想机制）

任何素材**需要我知道里面有什么**，就先把它注入我自己的上下文：

- 收到图片（QQ/微信附件、截图、下载的文件、我生成的图）想知道内容、人物、构图、颜色、图里的文字水印 → 走 `okattach`
- **附件不会自己进上下文**：QQ 入站只投来一行「已保存到 <路径>」的文字通知（带图 part 的注入只有我自己跑 `okattach` 才有）。**落盘路径 ≠ 我看得见**，只回显文字而我看不到画面就是漏了这步。
- 收到**视频**想知道里面有什么 → **不要 `okattach video`（当前后端不可用）**；用 `okattach frame <视频> <秒>` 抽帧注入（要多个时刻就多次调用）
- 做完图/片要**自己亲眼验收** → 先注入再看，不要只凭参数判断

反例（2026-11-03 我犯的）：主人发来照片，我手搓 base64 调 `/v1/chat/completions` 问图里有什么，凭记忆转述位置会出错（水印 B612 实际在左下，曾被说成右下）。正规路径就是注入自己上下文，一次到位。

## 用法

可执行文件 `okattach` 就在本插件目录（同 `meta.json` 一层），**不在 PATH**：先 `cd` 到这里跑 `./okattach`，或用绝对路径。

```
okattach <file>                      图片：原图注入（native，不缩放不压质，唯一路径）
okattach <file> --png                图片：转 PNG 无损（原分辨率，截图类素材）
okattach video <file> [--seg N]      【当前不可用】input_video 被后端忽略，别用
okattach video <file> --dry-run      视频：只打印计划（分辨率/分段/token 估算），不编码不 POST
okattach frame <video> <t> [--w W]   视频抽 t 秒单帧，压宽 W（默认 1280）注入为图片
加 --note "说明"：它会作为 label **进我的上下文**（`[okattach] <note>`，见 server.go:1562-1575），不是只显示在 /attachments
```

## 注入到哪台实例

优先级 = `--okhuman-port` 显式 > **本实例声明** `OKHUMAN_SELF_URL` 里的端口（主程序起工具子进程时注入，见 `internal/tools.SetSelfID`；与 agent-wire 同一个身份来源，2026-11-11）> 兜底才沿**进程父链**找 okhuman 主进程（argv0 基名 `okhuman`/`okhuman-*`）取其 LISTEN 端口最低一个。声明排在父链前：父链要按二进制名猜（.3 的插件代码记着 2026-11-07 主实例二进制当时叫 `okhuman-local`、判不出；2026-11-11 复核已改名 `okhuman`），且多个 LISTEN 只能取最低；声明是跑我的实例直接说的，跨平台、不受进程树变形影响。**认不出就报错退出（退出码 1），不猜端口**；在会话外手工跑要显式加 `--okhuman-port <端口>`。

## 依赖与限制

- ffmpeg/ffprobe（本机 6.1.1）；ffprobe 失败（无视频流/非视频）直接退出。
- 文件 ≤ 64MB（与 /inject 侧车上限一致）；注入走 base64，侧车体积 ≈ 文件 ×4/3。
- 视频段编码：libx264 crf23 fast、yuv420p、去音轨（`-ss` 在 `-i` 前 = 快 seek + 帧精确 + 输出时间戳归零）。
- `--note` 会进我的上下文（note 就是回显 label，正文里 `[okattach] <note>`）。要给我时间轴/主题说明就写在 note 里，一行足够。

## 图里的文字读不出时（OCR 环境，2026-11-06）

- **本机（.2）**：`tesseract 5.3.4`，语言 `chi_sim eng osd`。
- **另一台（.3）**：**已装 `tesseract 5.3.4` + `chi_sim eng osd`**（`sudo apt-get install tesseract-ocr tesseract-ocr-chi-sim tesseract-ocr-osd`，不到 20 秒——原先担心它外网 apt 很慢，是错的）。两台**同版本**：同一张截图分别 OCR，输出**逐字节一致**（md5 相同）；`--tsv` 词框、以及「黑底白字先用 `ffmpeg -vf negate` 反色再读」在 `.3` 上同样有效。两台都有 XFCE4 桌面，GUI 自动化两台都能跑（`.3` 用 `DISPLAY=:1`，对照表见 `computer` 插件）。
