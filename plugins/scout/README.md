# scout（Go 版）

插件/技能检索服务。**纯关键词，无向量；索引只在内存，不落盘；只支持 Linux。**
目的不变：agent 动手前先问「这事该用哪个插件/skill」。

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

## 为什么把语义向量拆了（实测，别再重新讨论）

同一语料（26 条）、34 条人工标注查询的对照（2026-11-05）：

| 配置 | R@1 | R@3 |
|---|---|---|
| 关键词 + 语义（旧，语义权重 0.2） | 24/34 | 29/34 |
| 关键词 + 语义，权重 0～1.0 全扫一遍 | 最好 22/26(直述)、21/26 且 R@3 26/26 | — |
| 纯关键词（未补 keywords） | 22/34 | 27/34 |
| **纯关键词 + `keywords` 字段（现方案）** | **31/34** | **33/34** |

留出集（12 条新写的、写 keywords 时没用过的）：旧 6/12 → 新 8/12（R@3 9/12 → 10/12）。
结论：**缺词才是根因，`keywords` 字段才是杠杆**，语义项净收益≈0（它还会把 asr/tts 从第 1 挤到第 2）。
代价对比：带向量 = reindex 16.2s、`llama-server:8591` 常驻 RSS 3.49 GB、查询 50ms（大头是给查询词做 embedding）；
拆掉 = reindex 毫秒级、查询亚毫秒、0 常驻。分词去单字也测过（R@1 22→20、R@3 27→28），不采纳。

## 为什么不落盘

真源是磁盘上的 `meta.json`；内存索引是可再生的：实测扫 25 个文件 **2.6 ms**、1000 条分词+建索引 **~165 ms**。
不持久化 = **不会拿旧快照骗人**（此前副本上挂着已删插件的索引，没人发现）。
后果要知道：进程重启或外接盘没挂载时 `docs=0`，`/health` 会照实报 0，不假装正常。

**规模阈值**（免得将来拍脑袋重构）：26 条全扫+索引 ~5 ms、查询 ~20 µs；1000 条索引 ~165 ms、线性查询 0.84 ms；
外推 1 万条线性查询 ~8 ms、冷启动 ~1.7 s —— 仍够用。**内存倒排在 ≥2000 条之前不值那 150 行**（实测只差 0.7 ms）。
真要加，先加倒排（O(命中)，照 OkEverything 的 apply/drop 增量维护），再谈持久化（必须配设备指纹 + 移除熔断 + 原子写）。

## API（:8480）

| 端点 | 说明 |
|---|---|
| `GET /search?q=&page=` | 返回 `results[]`（name/keywords/summary/path/kind/score/breakdown）+ `total`、`same_name` |
| `POST /doc` | body `{name,keywords,summary,path}` → manual 条目（内存，重启丢） |
| `DELETE /doc?name=` | 删同名条目 |
| `POST`/`GET /reindex` | 重扫；返回 `{indexed,new,changed,unchanged,removed,same_name,missing_keywords,meta_problems}` |
| `GET /doctor` | 当场重扫并与内存索引对照：`stale_in_index` / `missing_from_index` / `no_keywords` / `same_name` / `meta_problems`。**只报告，不改数据** |
| `GET /health` | `{ok,port,docs,same_name,missing_keywords,model:"off"}`（`model` 保留只为老调用方不报错） |

启动即扫树建索引（无 CLI 子命令；`./scout reindex` 已删）。

## 构建与测试

```bash
CGO_ENABLED=0 GOPROXY=off /usr/local/go/bin/go vet ./...
CGO_ENABLED=0 GOPROXY=off /usr/local/go/bin/go test -count=1 ./...   # 7 个测试：字段容错/坏meta可见/同名不吞/内联折叠/权重/kind/分词
```
纯标准库、无依赖、CGO 关。**不做交叉编译**（只 Linux）。

## 运维

- 本目录是**独立 git 仓库**（每个插件各自一个，按主人的规矩）。回滚点：`6f5afe6` = 拆向量前的原状。
- 旧二进制备份：`scout.bak-2026-11-05-pre-no-vector`；旧索引快照（含向量）：`index.json.bak-2026-11-05-pre-no-persist`。
- 起法：`(setsid /media/xhy/512nv固态/OkHuman_p/scout/scout </dev/null >>/tmp/scout-logs/scout.log 2>&1 &)`。
- embed 后端（`:8591`）已不再需要，随本次改造停止。
