# H3 出视频：锚定、档位、动作、硬坑

## 先按官方字段写，别自由发挥（2026-11-06 实测，两部短片）

不用 `base-en.txt` 的代价：物件/材质被换成常见物、关键帧废率高（30 张里 9 张）——它的字段规范里「写死物件 + 多处重复」正是解决这个的。写法见 `MiniMax-H3-Skills/h3-prompt-writing/`。

## 六、视频：首尾帧优先

`MiniMaxH3ImageToVideo` 有 `first_frame` **和** `last_frame` → 双锚定最可控。
`length` 是**帧数**（24 fps：124≈5.2 s，277≈11.5 s）。分辨率按官方档位（16:9）：
608×352 / 736×416 / 864×480 / 960×544 / 1056×608 / 1152×640 / 1216×672 / 1344×768(默认) / 1664×928。
steps=20、sampler `res_multistep`、scheduler `normal`（角色 sheet 用 `beta`）、seed 固定可复现。

提示词结构（角色一致性靠它）：`subject_definitions`（外观逐条，每段原样重复）→ `summary` →
`retention_analysis` → `detailed_description` → `overall_soundscape` → `non_diegetic_music: none`。
台词写进 prompt（H3 音画同出）：`says in Chinese: "原话"`。
**双锚定不会自动出动作**：只写端点状态 → 画面像 PPT。要写不间断的渐进动作过程。
验收看**逐帧运动能量曲线**，抽几帧看图会漏掉 1 s 瞬移。细节见 references。

## 十三、H3 三条硬坑（2026-11-03 实测，两次共赔约 25 分钟 GPU）

1. **分辨率必须是 32 的倍数**。§六 那张档位表全满足；用档外尺寸（如 720×960）**在 `ReferenceToVideo` 会静默通过**（我照样出过片），
   但同尺寸给 `ImageToVideo` 就在采样器处炸：`shape '[1,24,1,1,30,2,22,2]' is invalid for input of size 64800`（期望 30×2 × 22×2 = 960×704，我给了 720）。
   竖版 3:4 用 672×896、768×1024（都 /32 且比例正好）。`length` 也要 4k±1（124、145 可用）。
2. **`ImageToVideo` 的 `first_frame` 只配大 `length`**：`length=22` 配 `first_frame` 直接报上面的 reshape 错。
   → 想要"首帧锚定"就必须跑真视频（≥121 帧）；**没有"首帧 + 单帧出图"这条路**。
3. **身份从哪来，决定 prompt 怎么写**：
   - `ref_images`（R2V）路线：身份只靠 `subject_definitions` 复述外观。**prompt 里不描述 = 模型自由发挥**（我按"什么都不描述"跑一次，出来完全是另一个人，连发色服装都换了）。要一致就必须复述外观。
   - `first_frame`（I2V）路线：身份由首帧像素锁，**prompt 里一个字都不描述也能保住同一个人**（同一张照片实测：R2V 版全错，I2V 版人脸一致）。
   - 两者不能同时要：一张图里要三视图（重新构图）就不能以照片为首帧（首帧锁死就是照片的构图）。要"三视图 + 同一人"：走 I2V 让它转体，再**自己抽帧拼接**。
   - 补一句：首帧锁身份 ≠ 锁体型。I2V 生成的全身常把体型画胖、头身比失调——那是照片里看不见的部分被模型补的，得照实向主人说明。


## 十、竖版成片与人物/手部（2026-10-27 实测，第 1 集试镜）

- **竖版耗时**：544×960 原生竖版 16 步，SolAttnV100 生效时 **≈46 s/步**；105 帧 385 s、145 帧 615 s
  → 单条 6–10 分钟，17 条一批 ≈ 2.5 小时。竖版 latent token（29972）比 16:9 960×544×124（24801）还多 → **不能拿 16:9 的 26.9 s/it 外推竖版**。
- **两只手 + 物体必崩**：`两只手从两侧伸向一块石头` → 手与石头糊成一团、手指数量崩、手长在物体上。
  必须写 `exactly two human hands with ten fingers total, hands clearly separated from every object,
  no hand merging into or growing out of any object`；更稳的做法是**改用人影/影子/手套/器物**，不出现手。
- **不写调性就给大晴天暖光**：`hard low side light` 不够。要在 `detailed_description` 末尾显式写
  `low-key cinematic lighting, deep shadows, single hard light source, desaturated cool grey-blue palette,
  no bright daylight, no warm sunny ambience, no golden hour`（暖调段换成 `warm earthy palette`）。
  全片统一调性靠这句，不靠后期。
- **日志里的步数可能是历史任务**：判断当前任务步数看 `GET /queue` 里 `BasicScheduler.inputs.steps`，
  别看 `tail comfyui.log`（进度条会混进旧任务）。
- **QC 脚本要 numpy**：系统 `python3` 没有 numpy，跑逐帧差 QC 用
  `<本地盘>/ComfyUI-0.37.0/venv/bin/python`。

## 十五、竖屏短片（AI 短片）

**完整流水线参数与脚本入口在 `~/hdd/okhuman_horror/PIPELINE.md`**（按它执行，不在本文重复）；
好不好、算不算交付 → `video-aesthetic-acceptance.md`。
