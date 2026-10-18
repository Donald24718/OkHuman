# okknowledge — 关系性知识库（v0）

**用途（2026-10-06 主人定）：补 LLM 缺少的专业知识。**
LLM 不知道本机实测数字、主人的裁定、项目的真实状态。这些是**只有本机才有的接地事实**，
写进上下文每轮重付；写进本库则一次写入、按需取用。

表示规范：`docs/KB-v0-表示规范.md`；种子数据格式见 `seed.example.jsonl`（示例，非真实数据）

## 三条不变式（代码强制，不是约定）

1. **ID 永不改指**：改语义走 `revise` → 新 ID + `supersedes`，旧 ID 只标记 `[已取代]`。
2. **无出处 = 不存在**：`-prov` 必填，取 `master|import|verified|proposed`。
3. **`proposed` 不得参与硬约束**：系统自己提议的东西不能当规则用。

## 启动 / 编译

```bash
cd plugins/okknowledge
go build -o okknowledge .
./okknowledge stats
```
无守护进程、无端口、无 HTTP——**独立 CLI，数据自管在 `data/`**（`audit.log` 追加式，改不丢）。

## 配置

`config.json`（插件目录内，不进 OkHuman 的 config/）：
- `data_dir`：数据目录（默认插件下 `data/`；环境变量 `OKKN_DATA` 优先）
- `budget_mib`：活跃集字节预算，`stats` 用它判定超支并列降级候选

## 用法（agent 视角：先查再答，别凭记忆报本机事实）

```bash
./okknowledge find -text "tok/s"          # 关键词（v0 只有子串匹配，无语义检索）
./okknowledge find -kind 实体.决定         # 主人的裁定都在这里
./okknowledge get -id 13 -expand 6        # 名字层默认；-expand 给展开预算（节点数）
./okknowledge neighbors -id 8 -role runs_on
./okknowledge isa -id 13                  # 可读路径 = 沿 is_a 边算出的视图，不烧进 ID
./okknowledge add -kind 实体.测量 -label "..." -prov verified -note "怎么测出来的" \
                  -constraint "hard:上下文=262144"
./okknowledge link -src 19 -dst 8 -role runs_on -prov verified
./okknowledge dedup                       # 规范形同构 → 合并提议（不自动合并）
./okknowledge stats -budget-mib 64        # 体积 / 出处分布 / 超预算降级候选
./okknowledge load -f seed.jsonl          # 批量导入（ID = 空库时的载入行序）
```

**写入纪律**：`-note` 必须写清"怎么知道的"（命令 + 日期）。没有证据的条目降级为 `proposed`，
不得挂 `hard:`。会漂移的快照（如"可用内存 108GiB"）挂 `soft:`，不挂 `hard:`。

## 当前状态（2026-10-06）

- 已实现：节点/边存储、`is_a` 视图、约束挂载与出处隔离、ID 不可变与 `supersedes`、
  规范形去重、复用计数、预算报告、审计日志。
- 已载入种子：**29 节点 / 31 边**（本机硬件、模型权重、服务端口、实测数字、主人裁定）。
- **未实现（v1）**：语义/相似度检索（要靠嵌入投影，见报告 §15）、自动合并、
  预算**强制**执行（现在只报告候选，不自动删）、自由自然语言输入。

## 排障

- `拒绝：-prov 必须是 ...` → 出处缺失或拼错，四类之外的值一律拒。
- `拒绝：proposed 出处不得参与硬约束` → 把 `-constraint hard:` 改成 `soft:`，或把出处升为 `verified`（要证据）。
- `拒绝：端点不存在（禁止编造实体 ID）` → 先 `add` 再 `link`；幻觉 ID 硬挡。
- `load` 后边比预期少 → 边里的 `src/dst` 必须是**已存在 ID**；`load` 的 ID = 空库载入行序，
  非空库会顺延，先 `stats` 看现有节点数。
- 数据坏了 → `data/audit.log` 有全部变更，`nodes.jsonl`/`edges.jsonl` 是纯文本可直接改。
- 换机器/搬目录 → 只改 `config.json` 的 `data_dir`（或设 `OKKN_DATA`），数据不用动。
