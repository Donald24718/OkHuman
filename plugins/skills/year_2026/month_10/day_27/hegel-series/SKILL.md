# 黑格尔人话版：一集短片怎么做完（母文档只做路由）

本条目 = 这条流水线的真源（片源与脚本入口见 `pipeline.md`）。模型侧：出片参数在条目 `minimax-h3/`、服务在 `comfyui/`、配音在条目 `tts`。

## 要做什么 → 读哪份

| 要做什么 | 文件 |
|---|---|
| 一集从选题到成片的完整顺序（含第 3 集起生效的做法） | `pipeline.md` |
| 算不算做完（验收判据，缺一不算完） | `acceptance.md` |
| 改流水线脚本时的硬坑 | `script-pitfalls.md` |
| 按主题查坑：配音、排片与时长、合成前置、音频、字幕、画面一致性、封面、校验脚本 | `theme-pitfalls.md` |
| 已投到哪些渠道、各集状态 | `published.md` |
| 另一台机器上的差异 | `peer.md` |

## 遇到什么现象 → 读哪份

| 现象 | 文件 |
|---|---|
| 合成前置三处 FAIL、assemble 过不去 | `theme-pitfalls.md` |
| 字幕对不上、时间漂移、字幕被重烧丢格式 | `theme-pitfalls.md` |
| 人物/服装/场景前后不一致 | `theme-pitfalls.md` |
| 响度不对、爆音、音画不同步 | `theme-pitfalls.md` |
| 时长不对、排片被卡 | `theme-pitfalls.md` |
| 校验脚本 FAIL 但看不出哪步错 | `acceptance.md`、`theme-pitfalls.md` |
