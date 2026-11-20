# scout — 插件与技能索引服务（母文档只做路由）

纯关键词、无语义；索引只在内存，真源就是各条目的 `meta.json`。

## 要做什么 → 读哪份

| 要做什么 | 文件 |
|---|---|
| 什么进索引、哪些字段参与打分、字段写法边界 | `corpus-and-scoring.md` |
| 打分怎么算、为什么第一名不是自己 | `corpus-and-scoring.md` |
| 为什么不放语义向量、为什么不落盘 | `why-no-vectors-no-disk.md` |
| 端点与参数（search / reindex / doctor） | `api.md` |
| 构建、测试、起停与运维 | `build-ops.md` |

## 遇到什么现象 → 读哪份

| 现象 | 文件 |
|---|---|
| 改了 README / meta.json 搜不到 | `api.md`（reindex）、`corpus-and-scoring.md` |
| 一个词命中多个条目、分数反常、同名两份 | `corpus-and-scoring.md` |
| 查询报错、重启后什么都没有 | `build-ops.md`、`api.md` |
| 返回里出现 missing_keywords | `corpus-and-scoring.md`（必填字段）
