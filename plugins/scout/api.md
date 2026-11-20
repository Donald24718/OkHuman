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
