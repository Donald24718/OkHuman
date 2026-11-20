# MiniMax H3 出片（母文档 = 只做路由）

H3 跑在 ComfyUI 里。**服务侧（起停、节点接线、模型名与路径、端口、让卡、API 提交取件）在条目 `comfyui/`**；本条目只管生成什么、怎么写提示词、生成上的坑。
提示词的**权威写法**是随包的 vendored 文件：`MiniMax-H3-Skills/h3-prompt-writing/` —— 单帧起手/纯文本用它，多参考用它，字段名与顺序照抄，别自己编。

## 要做什么 → 读哪份

| 要做什么 | 文件 |
|---|---|
| 出视频：首尾帧双锚、分辨率档位与倍数、时长怎么换算帧、动作怎么写、三条硬坑、竖版耗时与成本、手部与调性怎么写才不崩 | `h3-video.md` |
| 出单帧、给现成的图上色/改材质、把片里的人物换掉 | `h3-image.md` |
| 配乐、BGM 垫台词、成片音轨怎么选、响度怎么做到目标 | `h3-audio.md` |
| 多参考（图 + 场景 + 参考音频）的标签写法；怎么证明某个参考真生效 | `h3-multi-ref.md` |
| 官方提示词的字段结构与顺序 | `MiniMax-H3-Skills/h3-prompt-writing/` |
| 好不好、算不算交付 | `comfyui/video-aesthetic-acceptance.md` |

## 遇到什么现象 → 读哪份

| 现象 | 文件 |
|---|---|
| 出来的换了个人，发色服装全变 | `h3-video.md`（身份从哪来决定怎么写） |
| 场景图与参考音频等于没接上 | `h3-multi-ref.md` |
| 画面像 PPT：只有端点状态、中间不动 | `h3-video.md` |
| 手和物体糊成一团、手指数量崩 | `h3-video.md` |
| 给图上色后完全没上色、只在两端插值 | `h3-image.md` |
| 采样器处 reshape 报错、首帧配短视频长直接报错、档外尺寸静默通过 | `h3-video.md` |
| 成片比做好的混音轻、片内环境音没进去、响度达不到目标 | `h3-audio.md` |
| 参考给得多之后动作幅度被压平 | `h3-multi-ref.md` |

## 成片风格配方（vendored，随上游包；每个还带 `.cn` 中文版，不改写）

**这些配方依赖 MiniMax Hub 的 `hub_*` 工具与 canvas，本机没有** → 只当分镜结构与镜头清单参考，别当可执行流程。

| 要什么 | 读哪个 |
|---|---|
| H3 提示词怎么写（必读） | `MiniMax-H3-Skills/h3-prompt-writing/SKILL.md` → `base-en.txt` / `ref-en.txt` |
| 纸艺定格动画科普（纸片/剪影/微缩布景/定格木偶动作） | `MiniMax-H3-Skills/papercraft-stop-motion-explainer/SKILL.md` |
| 纸拼贴风格动画：旁白/知识点/抽象概念视觉化 | `MiniMax-H3-Skills/paper-collage-explainer-generator/SKILL.md` |
| 极简高质感产品广告短片（电商推广、上新） | `MiniMax-H3-Skills/minimalist-product-ad-generator/SKILL.md` |
| 风格化 3D 动画短片：故事优先，先问画幅与总时长 | `MiniMax-H3-Skills/3d-animation-short-generator/SKILL.md` |
| 手绘发光动画 + 实拍空间融合的短片 | `MiniMax-H3-Skills/handdrawn-live-video-generator/SKILL.md` |
| 品牌/产品/网站/App/店铺宣传片 | `MiniMax-H3-Skills/brand-promo-video-generator/SKILL.md` |
| MV / 情绪短片 + 屏上文字排版 | `MiniMax-H3-Skills/music-video-subtitle-generator/SKILL.md` |
| 双人合作游戏主菜单 / 开场动画（先一张图定画风） | `MiniMax-H3-Skills/co-op-game-intro-generator/SKILL.md` |

