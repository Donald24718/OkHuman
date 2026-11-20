## 语料与字段

语料 = `plugin_root` 下任意深度、含 `meta.json` 且 `summary` 非空的目录（2026-09-21 起两棵树合并，
2026-11-05 起 `OkHuman_skill` 与 `Frequently Used Plugins` 都在同一根下）。条目四件套：

| 字段 | 来源 |
|---|---|
| `name` | `meta.json` 的 `name`，缺省用目录名 |
| `keywords` | `meta.json` 的 `keywords`（**字符串或字符串数组都接受**）；若 summary 里内联写了「关键词：…」，那截会被折进 keywords 并从 summary 摘掉（同一信号不算两遍） |
| `summary` | `meta.json` 的 `summary`（必填，空则跳过） |
| `path` | 目录路径 |
| `kind` | 在 `OkHuman_skill/…` 下 = `skill`，否则 `plugin`（2026-11-05 前 `source` 一律误写成 plugin） |

另有 manual 条目（`POST /doc` 写入，`reindex` 不清除；**只在内存，重启会丢**）。
运行时资产目录（`venv`/`data`/`speaker`…）剪枝，但**目录自己带 meta.json 就不剪**（2026-10-16 `asr` 撞名教训）。

**同名不自动取舍**（2026-11-05，主人定）：两处同名就两条都返回 + `same_name` 计数，由 agent 看见后自己删。
旧代码用 `seen[name]` 去重，谁被看见取决于 WalkDir 顺序 —— 那是真 bug，已修。

## 打分

`score = 0.30*nameKw + 0.35*kwKw + 0.25*sumKw + 0.10*pathKw`
分词不变：中文 unigram+bigram、英文/数字整词、中英双向词桥（XLING）。
阈值 `score>0.02`，**每页 10 条（2026-11-05 主人定：一次给够，不翻页）**，上限 50；同分按 path 稳定排序。`page=` 仍可用（老调用方不破）。
