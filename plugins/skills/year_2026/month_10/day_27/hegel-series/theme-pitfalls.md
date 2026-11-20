## 按主题的硬坑（第 4–8 集实测；按主题查，不按集查）

### TTS（IndexTTS2）
- 解释器必须是 index-tts 的 venv（.2:`<模型盘>/index-tts/.venv/bin/python`，本机:`<另一台盘>/mode_gguf/index-tts/.venv/bin/python`）——ComfyUI 的 venv 缺 librosa，跑 `synth_narr.py` 直接 ImportError。
- `work/narration.txt` 必须是 `L<id>|文本` 格式（不是纯文本一行句）。
- `synth_narr.py` 按「wav 已存在就 SKIP」工作。**插入/删除一行会让行号后移，旧 wav 与新文本错位**：
  改过行序后必须删掉受影响的旧 wav 再重跑那些行号，否则成片里配音与字幕对不上。

### 排片与时长
- `edl.py` 支持 `EDL_GAPS="4:1.3,7:1.0,..."`（旁白 id→该句后额外呼吸秒）。**先算 TTS 总长再决定要不要加呼吸**，
  别靠加镜头凑时长（e4 旁白 64.8s，按默认 gaps 排到 76.6s 低于 80–95s 规范 → 用 6 处加长到 82.5s）。
- **30 秒版的边界从 `edl.json.narr` 的 `t0_f/t1_f` 除以真实 fps（本系列 24）换算**，别按记忆里的秒数切
  （按旧秒数切过，把 L7 后半句切掉了，ASR 才发现）。
- **30 秒版首段必须是 `[0.0, 0.8]`**（封面卡那 0.8s）。朋友圈缩略图取视频首帧，从旁白起切就是一条带字幕的暗画面。

### assemble 前置（三处 FAIL 各踩过一次）
1. `subs.py` 必须先跑生成 `<集>/work/subs.ass`；`assemble.py` 只烧不生成，缺文件时最后一步 rc=254 才报。
2. `grade.py` 的条目数必须**等于镜头数**（脚本打印 `GRADE_FROM ... N`，N≠镜头数就是漏了；缺号那条退回 DEF，画面明显不一致）。
3. 沿用的旧配乐**要拷进本集 `<集>/music/`**（assemble 只按本集目录找，缺就报「缺配乐」退出）。
   曲库散在各集 music/ 下，`ls e*/music/Hxx.mp3` 找。
- 只改字幕时**直接重跑 assemble 就安全**（~30s）：镜头段缓存在 `<集>/work/seg/`，字幕在 final 步骤才烧；只有改画面才清 seg。

### 音频
- **音轨必须补到与画面等长**：`assemble.py` 混音用 `amix:duration=first`，输出长度＝配乐长度，
  旁白/音乐结束后那段（片尾卡 ~4s）**没有音轨**。后果不只是静音：`cut30.py` 的 `acrossfade`
  在没有音频的段上永远等不到帧而挂死——ffmpeg 写完视频数据却不写 moov，`ffprobe` 报 `moov atom not found`，
  看着像磁盘卡住其实是图没 EOF。修法：NORM 那遍 `loudnorm` 之后追加 `,apad=whole_dur=<total_f/FPS>`
  （补静音放在响度测量之后，不影响 −14 LUFS）。**判据：`ffprobe -show_entries stream=codec_type,duration` 两条流时长相等。**
- **切片的输出别落在机械盘上**：目标在 `~/hdd/...` 且内存吃紧时，`+faststart` 重写阶段会长时间不动。
  用 `CUT_SRC=/tmp/eN_v1.mp4 CUT_OUT=/tmp/eN_30s.mp4`（/tmp 是 tmpfs）先出片再 `cp` 回投递目录。
  **`CUT_SRC` 也要换**：源在机械盘上时 7 个 `-ss/-i` 随机 seek 同样慢。

### 字幕
- `subs.py` 样式是 `WrapStyle: 2`（禁自动换行），长句居中会两头被切。`wrap()` 按标点切块后贪心装到**每行 ≤15 字**
  （N 样式 58px，全角≈58px，安全区 1080−70−70=940px）。**改字号要同步改这个上限。**
- 片尾卡小字一行控制在 ~15 字内，烧完必须抽帧看边界（加长版被切掉过右端）。
- **片尾卡墨色跟着结尾画面走**：结尾是白光吃满画面时，在 `edl.json.cards.end` 写 `"ink":"dark"`
  → 片尾卡 T/S 两条样式改黑字白描边（N/K 不动）；结尾是暗画面就不写该字段（行为与以前一致）。
- **试字幕样式时 `-ss` 必须放在 `-i` 之后**：`-ss X -i 片` 会把时间戳归零，ASS 事件全对不上，
  抽出的帧上"字不见了"是测试方法错，不是字幕错。正确：`-i 片 -ss X -frames:v 1 -vf subtitles=…`。

### 画面一致性（最容易漂移的三样）
- **地面**：在 `shots.py` 写 `FLOOR` 常量并拼进每条 prompt（`large worn stone slabs, NOT small square tiles, NOT grout grid`）。
  近景/宏镜与俯拍镜（地面占满画面的镜号）要把 FLOOR **同时写进 `detailed_description`**——只写 retention 段压不住。
  地面是这类暗场戏最容易漂移的元素，**写死比事后挑便宜**（e4 两条自己长出方砖勾缝、e6 俯拍长成方砖网格，都是重跑一次即合格）。
- **同一间房的连续性**：提示词里写「同一间暗石房、夜里、只有一束冷硬光、NOT 室外、NOT a wooden chair」，
  并把上一镜已成立的物件（石椅/立玻璃）**按名字**写进 prompt，否则模型每次换家具（e8 17 镜里 3 条跑成室外大太阳）。
- **两物体对峙压不住「间距走合」**（推近、改侧移、写死 never touch 都会合拢，三次重跑无效）→
  取前 N 帧补帧慢放填满窗：`trim=end_frame=N,setpts=PTS*k,minterpolate=fps=24:mi_mode=mci:mc_mode=aobmc:me_mode=bidir`，
  再把 `edl.json` 该镜的 `clip_f`/`speed` 改成实际值。慢放一段本来就慢的平移看不出来。
- 镜头淘汰重跑的其他判据：材质被换（写死 `NOT pale grey wood`）、地面变木地板（`bare stone, never floorboards`）、
  俯拍环被平铺（见 h3-prompt-writing §4）。视觉线要跨镜连续时，把关键元素写进相邻两镜。

### 封面
- 首帧即封面（成片开头不加淡入）。关键镜质量最好时直接用它做封面，取帧点选**主体被照亮、织纹看得清**的那帧
  （e6 的 S14 用 3.4s 而不是 2.6s）。
- **对外文案里的时长数字用实测值**（成片 85.5s、30s 版 27.7s 这类），不写排片预估（83s/26s 都写错过）。

### verify.py
- **要等 assemble 打印 FINAL**：最后一步带 `+faststart`，FINAL 前文件还在重写，早跑报 `moov atom not found`。
- **`-n` 会被当成视频路径**（它取第一个不以 `-` 开头的参数当 SRC）。要改抽帧数就显式带上视频：
  `verify.py out/<slug>_v1.mp4 -n 18`；不带参数跑默认 15 点最省事。
- **30s 版「句子是否完整」不能用 ASR 判**：ASR 对 5 秒短窗口会漏听、还会把内容串到别的窗口（三次复听互相矛盾）。
  可靠做法是**逐秒包络相关**求 cut→src 映射：8kHz 单声道 → 10ms RMS 包络 → 每个 1s 窗口在全源里找最大相关（corr≥0.99 即对齐）。
  整片 ASR 只用来核对「有没有截词」，不用来证明「哪句在/不在」。
- 投递回查（含 B站标签 `get_tags()`）见 bilibili / douyin / wechat-moments 三篇。
