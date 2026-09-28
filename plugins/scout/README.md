# scout（Go 版）

插件目录语义检索服务。移植自 TS 版（机械硬盘 `OkHuman_ts_2026_9_20/OkHuman_p/scout/`），行为 1:1 对齐：
语料 = 统一根目录（`plugin_root`，技能与插件同目录）每个顶层子目录，
条目三件套（名字/简介/路径）全部读自**模型自写的 `meta.json`**（`summary` 必填，`name` 可选、缺省用目录名）；
无 meta.json 或 summary 空则跳过——简介一律 agent 自写，scout 不抓任何文件内容。
另有 manual 条目（API 写入，reindex 不清除）。

- 纯 Go 标准库，无第三方依赖；`go 1.24.4` 构建。
- 索引落盘 `index.json`（原子写：tmp+rename），重启直接加载，不重新 embedding。

## 打分

`score = 0.40*nameKw + 0.25*summaryKw + 0.20*semantic + 0.15*kwAll`；
模型缺席时按 `base/0.8` 归一。分词：中文 unigram+bigram、英文/数字整词、中英双向词桥（XLING）。
语义向量：Qwen3-Embedding-0.6B（1024 维，f16），纯文本直喂（Qwen3 系不用 e5 前缀）。

## API（:8480）

| 端点 | 说明 |
|---|---|
| `GET /search?q=&page=` | 每页 5 条、总上限 50、score>0.02；返回含 breakdown（nameKw/sumKw/sem/kwAll） |
| `POST /doc` | body `{"name","summary","path"}`，写 manual 条目（带 embedding） |
| `DELETE /doc?name=` | 删 manual/任意同名条目（query 参数，与 TS 版一致） |
| `POST`/`GET /reindex` | 重建索引（manual 条目保留）；返回 `{indexed,removed,total,model}` |
| `GET /health` | `{"ok","port","docs","model"}` |

首次启动（docs=0）自动 reindex。CLI：`./scout reindex`（重建一次后退出）。

## embed 后端（常驻服务语义）

`Qwen3-Embedding-0.6B-f16.gguf` 由 `llama-server --embedding` 提供，**常驻不重载**：

1. scout 启动时先健康探测 `:8591` —— 已就绪则直接复用（日志"已就绪，复用"）。
2. 未就绪则 setsid 拉起 llama-server（完全脱离，scout 死了它独立存活，日志 `/tmp/scout-embed.log`），等待最长 3 分钟。
3. `./scout reindex` CLI 即使自己拉起也不关闭 —— 谁先拉起谁负责，之后所有实例复用。
4. 模型加载失败/超时时语义降级为 off（纯关键词打分），服务不挂。

手动停 embed 后端：`kill $(ss -ltnp | grep 8591 | grep -oP 'pid=\K[0-9]+')`（下次 scout 会重新拉起并加载模型，约 3s）。

## 启动 / 停止

```bash
# 启动（常驻）
(setsid plugins/scout/scout < /dev/null > /tmp/scout.log 2>&1 &)
# 探活
curl -s http://127.0.0.1:8480/health
# 停止（embed 后端保持存活，下次复用）
pkill -f 'OkHuman_p/scout/scout'
```

## 配置（config.json）

`port` 8480 / `plugin_root` 插件目录 / `skill_root`（空）/ `model_path` gguf 路径 /
`server_bin` llama-server 路径 / `embed_port` 8591 / `embed_ctx` 1024。

## 排障

- 日志：`/tmp/scout.log`（scout 本体）、`/tmp/scout-embed.log`（embed 后端）。
- `model:"off"` → embed 后端没起来或加载超时；看 embed 日志，手动起一次再 `/reindex`。
- 索引坏了 → `rm index.json && POST /reindex`。
- 新增/修改插件（meta.json/README）后记得 `POST /reindex`。
