# 一集从选题到成片（工作树与切集方式）

工作树 `~/hdd/okhuman_hegel/`（第 1 集在根，第 2 集在 `e2/`）。所有脚本用 `HEGEL_W=<该集目录>` 切集（`gen_shots / assemble / cover / subs / edl / synth_narr / cut30 / gen_music` 都已参数化）。解释器用 `<本地盘>/ComfyUI-0.37.0/venv/bin/python`；**`synth_narr.py` 必须换成 `<模型盘>/index-tts/.venv/bin/python`**（ComfyUI 的 venv 缺 librosa，会 ImportError）。

## 顺序（照做，别跳步）

1. `narration.txt`（每行一句旁白）→ `synth_narr.py`（IndexTTS2，`lang="zh"`，参考音色在脚本里）→ `narration/final/Lxx.wav` + `work/narr_durations.json`
2. `edl.py` → `work/edl.json`（镜头表：`id/src/clip_f/need_f/speed`、旁白 `t0_f/t1_f`、`cover_f/end_f/tail_f/total_f`，
   以及本集专属字段 `slug / title{series,ep,sub} / music[cue…] / cards{term,person,end}`）
3. `gen_shots.py [镜头号,逗号分隔]` → `clips/shotNN.mp4` + 运动 QC + 抽帧 `frames/shotNN_f{0..3}.jpg`
   （seed=100+n*7 固定；重跑会把旧片备份成 `clips/prev_shotNN_*.mp4`）
4. **画面验收（必做，QC 过不等于能用）**：把每条的 4 帧拼成 contact sheet 用 okattach 看。
   QC 只测运动量，测不出：构图被转 90°、人形崩坏、跨镜材质不一致、可读文字。第 2 集 16 条里 3 条栽在这里。
   重跑前先改 `work/shots.py`（提示词硬坑见 `comfyui/MiniMax-H3-Skills/h3-prompt-writing`）。
5. 配乐：cue 定义在 `work/submit.py` 的 `MUSIC`（bpm/dur/caption），`gen_music.py H1 H2 …` 只生成缺的。
   **首尾 cue 沿用上集（系列听觉身份），中段按本集情绪新写两条**；`edl.json.music` 决定顺序，`assemble.py` 等分时间轴 + 1s 交叉淡接。
6. 封面：`cover.py <某条clip> <取帧秒>` → `work/cover.png`（文案从 `edl.json.title` 读）。**首帧即封面**，成片开头不加淡入，这张同时是各平台缩略图。
7. 合成：`mkdir -p <集>/out <集>/deliver`（**不建 out/ 会在最后一步 FAIL final**）→ `assemble.py`
   （封面卡→逐镜硬切+调色→五床音频侧链→两遍 loudnorm −14→烧字幕 → `out/<slug>_v1.mp4`）
8. 30 秒版：`work/cut30.json` 写 `{"pad":0.0,"segs":[[a,b],…]}` → `cut30.py`（段前后各留 0.3s 静音，让 `acrossfade` 吃空白不吃词）
9. 投递：**B 站 + 抖音 + 朋友圈，不发小红书**（主人 2026-10-27 明令）。
   B 站走 `deliver/bili_up.py`（`/tmp/biliup` venv，cookie 在 `/tmp/bili-cookie.txt`，从 Firefox 画像 `cookies.sqlite` 取，tid=65）；
   抖音见 `douyin` skill 的 `video.md`（发视频）与 `reply.md`（回评论）；朋友圈见 `wechat-moments`（发视频）。上传件压到 ~27MB：`-crf 23 -maxrate 3500k -bufsize 7000k`。

## 第3集起生效的做法（2026-10-27 实测）

- **每集独立调色表**：`eN/work/grade.py`（逐条 shot 参数），`assemble.py` 用 `GRADE_FROM=<该文件>` 加载。
  俯拍/揭示类镜头统一 `blk` 负值压暗降饱和，避免与叙事镜头同亮度时"发飘"。
- **验收脚本 `work/verify.py` 可复用**：ASR 逐句比对 + `ebur128` 响度 + 15 点抽帧拼图。
  ASR 差异若只是**同音字/标点/英文词转写**（它→他、摘要→摘药、agent→A镇台）＝非缺陷，
  逐字比对必须按此过滤，否则每集都误报 5 处。响度目标 **−14 LUFS**（实测 −14.0）。
- **片尾卡小字会被裁**：一行控制在 ~15 字内（本次「黑格尔《精神现象学》序言 · 1807」通过；
  加长版被切掉右端）。烧完必须抽帧看边界。
- **对外文案里的时长数字用实测值**（成片 85.5s、30s 版 27.7s），不写排片预估（83s/26s 都写错过）。
- **上传件一份通吃 B站+抖音**：`-crf 23 -maxrate 3500k -bufsize 7000k -pix_fmt yuv420p -movflags +faststart`
  → 85s 1080p 约 30MB；同时拷一份到 `~/视频/hegel_eN_full.mp4` 供抖音 GUI 选文件。
- **镜头淘汰重跑判据**：材质被换（浅灰木→写死 `NOT pale grey wood`）、地面变木地板
  （写 `bare stone, never floorboards`）、俯拍环被平铺（见 h3-prompt-writing §4）。
  视觉线要跨镜连续时，把关键元素写进相邻两镜（本次"环中心那根短条"）。
