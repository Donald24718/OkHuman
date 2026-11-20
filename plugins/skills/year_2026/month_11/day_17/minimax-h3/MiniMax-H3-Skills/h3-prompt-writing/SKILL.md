---
name: h3-prompt-writing
description: Write MiniMax H3 video generation prompts for T2VA, I2VA, FL2VA, L2VA, and Ref2VA. Use when rewriting multimodal requests into H3 prompt structures, composing integrated_multimodal_description, overall_soundscape, and non_diegetic_music, aligning keyframes, or defining reference labels for images, videos, and audio.
compatibility: Portable to any agent that can read local files — no external API calls, MiniMax Hub tools, or proprietary runtime required. The agents/openai.yaml file only adds optional ChatGPT/Codex UI metadata; it does not restrict the skill to OpenAI agents.
---

# H3 Prompt Writing

## Workflow

1. Identify the input mode: T2VA, I2VA, FL2VA, L2VA, or full-reference Ref2VA.
2. For base text/keyframe modes, read `references/base-en.txt` and follow its final prompt structure.
3. For full-reference mode, read `references/ref-en.txt` and follow its six-section rewrite format.
4. Preserve the exact field names, section order, labels, and timing notation from the selected guide.

## Base Modes

- T2VA: build the full audiovisual timeline from text.
- I2VA: start from the first frame and develop forward from it.
- FL2VA: describe the continuous path between the first and last frames.
- L2VA: infer a plausible opening and converge to the supplied last frame.

Use `integrated_multimodal_description`, `overall_soundscape`, and `non_diegetic_music` in the order shown in `references/base-en.txt`.

## Full-Reference Mode

Ref2VA rewrites use `subject_definitions`, `summary`, `retention_analysis`, `detailed_description`, `overall_soundscape`, and `non_diegetic_music` in that order. Reference labels stay consistent across all sections.

Read `references/ref-en.txt` for label rules, retention analysis, and complete examples.

## Output Rules

- Write rewrite sections in English; preserve dialogue, lyrics, and visible scene text in their original language.
- Describe each shot by composition, subjects, environment, actions, camera, sound, and the exact point where referenced content appears.
- Avoid plot summaries, unresolved reference labels, and timing that does not match the requested duration.

## 本机实测补充（2026-10-27，黑格尔人话版第2集，544×960 / 16 steps）

1. **跨镜同一物体必须写死材质**：只写"同一堆木块"不够，模型会换材质（深棕旧木 → 浅色新木）。
   要在 `subject_definitions` 里显式写 `the very same dark weathered wooden blocks from the previous shot … NOT new pale wood, NOT light beige timber`，
   并在 `detailed_description` 里再重复一次。
2. **天然横向的场景会被整幅转 90°**：`vast flat plain + huge sky` 这类，即使提示词已写
   `Vertical 9:16 portrait … horizon low in the frame`，两次生成都把地平线画成竖线（地面在左、天空在右）。
   **解法是去掉地平线**：改成垂直俯拍（`camera pointing straight down at the ground, the ground fills the entire frame, no sky and no horizon visible`）——
   没有天空就没有可转的方向，一次通过。
3. **要"空镜无人"时必须逐条否定**：写 `the dark imprint of the step` 模型画出了人形剪影＋爪状脚。
   改成 `a worn dark rectangular scuff mark shaped exactly like that step; no person, no human figure, no silhouette, no body, no feet anywhere in the frame`，
   并在 `retention_analysis` 与 `detailed_description` 里各重复一次。
4. **俯拍"一个环"压不住平铺倾向**：`exactly one ring` 连写两次（含 `retention_analysis` 重复）仍被画成
   多个环平铺，两次重跑都失败。**解法是只让一段大弧线进画、其余出框**：
   `only a large arc of the ring enters the frame, the rest of the ring outside the frame, no other rings`——
   弧线无法被平铺复制，第三次一次通过。要"完整一个环"时改用近景/斜俯拍而非正俯拍。
