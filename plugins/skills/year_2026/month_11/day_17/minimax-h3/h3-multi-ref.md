# 多参考（图+场景+音频）的标签体系与归因

## 十二、多参考（图 + 场景图 + 参考音频）必须用标签体系（2026-11-03 实测）

`MiniMaxH3ReferenceToVideo` 能同时接多类参考：`ref_images={ref_image_0,ref_image_1,...}`、
`ref_audios={ref_audio_0}`（音频用 `LoadAudio`，并**必须**给 `audio_vae`=`minimax_h3_audio_vae_fp32`），
`ref_image_size="max"` 时按各自原分辨率编码。

**关键：prompt 里必须用官方标签指代每个参考，不能用自然语言**（"the reference image"、"the recording" 这类指代无效）。
权威写法在本技能随包的 `MiniMax-H3-Skills/h3-prompt-writing/references/`：**多参考走 `ref-en.txt`（六段式 + 标签）**，
**单帧起手/纯文本走 `base-en.txt`（两段式 + 三字段，含台词与说话人写法）**。要点：

- 标签：`<Picture N>` = 第 N 张 ref_image（**1 基**）、`<Audio N>` = 第 N 条 ref_audio、`<Subject N>` = 要跟踪的内容单元（人也包括场景/服装/风格）。标签一旦定义，在 `subject_definitions / summary / retention_analysis / detailed_description` 中含义不变。
- **只用于定义角色/场景/服装/风格的图，不要单列 `<Picture N>` 条目**，要写进对应的 subject 定义里：
  `<Subject 1> is the young woman shown in <Picture 1>, with ...`
- 同一主体来自多个资产时，写清**每个资产提供什么**：
  `<Subject 1> is the woman whose appearance comes from <Picture 1> and whose walking motion comes from <Video 1>.`
- 场景/室内当**空间参考**：`<Subject 2> is the room whose spatial layout — … — comes from <Picture 2>, restyled as …`（再点名要保留的结构：窗位、门位、家具位）。
- 参考音频固定句式 + 说话人 ID：`<Audio 1> is the voice-timbre and delivery reference for <Subject 1> (S1).`
- 说话人用 `(S1)`，**台词写在 `<d>` 里并标语言**：`The young woman with a clear gentle voice (S1) says: <d>[Chinese] 开张第一天，往后，还请多多关照。</d>`

实测（三参考：三视图 + 实景室内 + Kokoro TTS 语音，960×544/124 帧/20 步）：
- 不用标签时：只有角色图起作用（长相稳住了），**场景图与参考音频等于没接**——因为我的文字已经描述了同样的空间与同一句台词，无法归因。
- 用了标签时：场景的空间结构（左百叶窗/后门/右柜/桌位）与音色才可能跟上来；**台词的文字由 `<d>` 控制，音色才由 `<Audio N>` 控制**（用本机 ASR 转写比对是验这个的唯一硬办法）。
- **归因铁律**：只看"成片符合"不能证明某个参考生效，必须做对照（去掉那个参考再跑一次）。
- 参考给得越多，**动作幅度越容易被压平**（同一角色三次实测逐帧运动能量 13.3 → 6.5 → 3.7）。要多参又要动，就必须把动作写成带时间刻度的大动作（迈步、转身、甩袖、深鞠躬），并在验收时看能量曲线。
