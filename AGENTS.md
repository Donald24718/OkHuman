# OkHuman — Agent 运行时项目指令

> 所有权边界 / 重要默认值 / 本地安全工作流 / 验收标准。
> 任务特定流程按需链接，不在此重复。

---

## 协作规范

包含 Git 规范、文档维护、PR/Issue 流程三个子章节。

## Git 规范

- **更改确认**：任何 Git 更改 → 暂存 → 提交操作，必须得到用户的允许或提示
- **分支命名**：
  - 功能：`feature/<功能名>`
  - 修复：`fix/<问题描述>`
  - 文档：`docs/<文档类型>`
- **提交信息格式**：
  ```
  <类型>(<范围>): <简短描述>

  [可选的详细说明]

  [可选的关联 Issue：Closes #123]
  ```
  - 类型：`feat` / `fix` / `docs` / `style` / `refactor` / `perf` / `test` / `chore`
  - 示例：`feat(server): 添加 WebUI 配置页保存功能`
- **命令示例**：
  ```bash
  # 创建功能分支
  git checkout -b feature/协作规范完善

  # 暂存并提交更改
  git add AGENTS.md
  git commit -m "docs: 完善协作规范章节"

  # 推送并创建 PR
  git push -u origin feature/协作规范完善
  ```

## 文档维护

- **文档层级**：
  - `README.md` — 项目介绍、快速开始、核心概念
  - `CHANGELOG.md` — 版本变更记录（按时间倒序，如有需多版本管理）
  - `CONTRIBUTING.md` — 贡献指南（可选，有多人协作时添加）
  - `docs/` — 详细技术文档（如需）
- **更新时机**：
  | 变更类型 | 必须更新 | 位置 |
  |---|---|---|
  | 新增 / 移除配置项 | ✅ | `config/default.json` + `config/user.json.example` + `AGENTS.md` 关键默认值表格（`internal/config/config_test.go` 会拦截漏项） |
  | 新增 / 移除 API 端点 | ✅ | `AGENTS.md` 相关章节 |
  | 新增 / 移除插件能力 | ✅ | 插件 `README.md` + scout 索引 |
  | 新增 / 移除目录结构 | ✅ | `AGENTS.md` 目录布局 |
  | 性能或安全相关变更 | ✅ | `CHANGELOG.md` |
- **格式要求**：
  - Markdown 优先，代码块标注语言
  - 配置项变更需同步更新 `config/default.json`
  - **`config/user.json.example` 必须是严格合法 JSON**（不支持 `//` 注释行）：README
    让用户直接 `cp` 它成 `user.json`，而 loader 用 `encoding/json`——带注释的模板会让
    照文档操作的用户**启动即失败**。说明文字写进 `_comment` 字符串字段。模板里不要写
    `data.dir`（会被合并生效，写错即数据目录错乱）。
  - 敏感信息（如密钥、端点）使用占位符或环境变量引用

## PR / Issue 流程

- **Issue 模板**（建议在仓库根目录创建 `.github/ISSUE_TEMPLATE/` 目录，参考模板如下）：
  - `bug_report.md` — 问题描述、复现步骤、环境信息
  - `feature_request.md` — 需求背景、期望行为、建议方案
  - `question.md` — 问题描述、已尝试的方向
- **PR 流程**：
  1. 从 `main` 创建功能分支
  2. 开发并添加测试（Go 测试文件命名：`*_test.go`）
  3. 提交时填写 PR 模板（标题、描述、关联 Issue）
  4. 至少一次 review 后合并
  5. 合并后删除分支
- **审核要点**：
  - 代码逻辑正确性
  - 是否破坏现有测试
  - 文档是否同步更新
  - 是否引入不安全模式（如 `panic` / 敏感信息泄漏）

## 项目概述

**Go 单仓**：一个进程 = 一个完整 agent（系统提示词、消息队列、上下文管理、工具循环、WebUI）。
多 agent = 多个独立端口的进程。插件与技能**设计为独立仓库**（`plugins/RULE.md` 为唯一规范），当前 `plugins/` 下的各插件是主仓子目录（各自独立的 git 仓是规范目标）。

**单一元工具**：`bash`——一切外部操作（命令执行、文件读写、搜索、抓截断日志）都靠它。

---

## 目录布局（权威）

```
OkHuman/                     # 主程序仓（唯一 git 仓）
├── main.go                  # 入口
├── internal/                # agent 核心（agent / background / config / context / llm / persist / prompt / server / tools / types）
├── webui/                   # 前端资产
├── prompts/                 # 系统提示词（分层 .md，热加载）
├── config/
│   ├── default.json         # 出厂默认
│   └── user.json            # 用户覆盖（最小补丁，勿手动删键）
├── plugins/                 # 插件目录（扁平，非 git 仓）；具体有哪些插件以 `ls plugins/` 或 scout 索引为准
│   └── skills/              # 技能库（非 git 仓）；year_<年>/month_<月>/day_<日>/<skill>/SKILL.md
```

**布局规范以 `plugins/RULE.md` 为唯一权威**，本文件不重复细节。

---

## 关键默认值（验证过）

| 参数 | 默认值 | 位置 |
|---|---|---|
| HTTP 端口 | 8451（冲突时自动 +1） | config/default.json, main.go |
| LLM 端点 | http://127.0.0.1:8080/v1 | config/default.json |
| 数据目录 | `$HOME/.okhuman-<port>` | main.go（自动追加端口后缀） |
| 提示词目录 | `prompts/`（相对可执行文件目录） | config/default.json |
| 前台工具超时 | 30 s | config/default.json |
| 工具总超时 | 600 s | config/default.json |
| 工具轮次上限 | 200（0/负 → 200） | config/default.json（`tools.max_tool_rounds`） |
| doom 检测 | 连续 3 次相同调用 | config/default.json |
| Result limit | Default()=10000 / default.json=50000 / user.json=50000 | config/default.json, config/user.json |

配置优先级：**内置默认值 < config/default.json < config/user.json < 环境变量**（`OKHUMAN_PORT` / `OKHUMAN_LLM_BASE_URL` / `OKHUMAN_LLM_MODEL` / `OKHUMAN_DATA_DIR` / `OKHUMAN_FAKE`）。

路径均相对**可执行文件所在目录**解析；克隆到任意位置均可运行。

---

## 验证过的本地命令

```bash
# 构建主程序（Go ≥1.25）
go build -o okhuman .

# 启动
# Linux
(setsid ./okhuman < /dev/null > /tmp/okhuman.log 2>&1 &)
# Windows (PowerShell)
Start-Process -FilePath .\okhuman -RedirectStandardOutput "$env:TEMP\okhuman.log"

# WebUI
open http://127.0.0.1:8451/

# 测试
go test ./...

# FakeLLM 模式（不烧 token，测试用）
OKHUMAN_FAKE=1 ./okhuman
```

---

## 插件与技能发现流程（必须遵守）

当需要主进程之外的能力时：

1. **搜索 scout 索引**：`curl "http://127.0.0.1:8480/search?q=关键词"`
2. 命中 → 读对应 `README.md` / `SKILL.md` 再用
3. scout 挂了或无命中 → 才退回 `ls plugins/` 或通用工具

**凭记忆/常识跳过搜索 = 违规**（历史教训：读图需求没搜引走了通用 OCR 弯路，素材注入插件两次都是搜索第一命中）。

改完插件/skill 的 README 或 meta.json → 刷新索引：`curl -X POST http://127.0.0.1:8480/reindex`

---

## 插件开发铁律（摘要）

> 完整规范及全部细节见 `plugins/RULE.md`（唯一权威）。以下为最常用规则的快速索引：

- 改完源码 = 没干活；**重编 + 真实启动验证 = 才干完**
- 不 import OkHuman 任何模块，只走 HTTP 接口
- 配置自管（插件目录内 `config.json`）、数据自管（`data/`）
- 每个插件 = 独立 Go 模块 + 独立 git 仓

---

## WebUI 配置页保存行为

WebUI 配置页的保存会触发 `POST /config`，patch 深合并进 `config/user.json`，并自动剔除"等于出厂默认"的叶子键以保持文件最小。

**手动删除 user.json 的键是危险的**：如果 default.json 也设了该键，删除 user.json 中的值意味着让 default.json 的值生效——可能改变实例身份（如 port 被偷），导致端口冲突。修改配置请用 WebUI 或 `PATCH /config` API。

---

## 多实例部署

- **生产**：权威树，服务 `:8451`
- **副本**：另一进程树（如 `:8452`），插件仓单向镜像跟随
- **救生艇**：冻结旧树，永不更新，唯一用途是双死时救场

二进制交换前必须先在一次性实例端到端验证。新二进制严禁同时落两棵树——旧二进制实例就是救生艇。

---

## 验收标准

> **平台支持**：自 2026-10-04 起，主程序**三平台可构建**（Linux / macOS / Windows）。
> 原先 `bash` 工具内的 `Setpgid`/`syscall.Kill` 等 Unix 专属调用已按 build tag 拆入
> `internal/tools/bash_unix.go` / `bash_windows.go`，平台无关层留在无 tag 文件。
>
> 验收**推荐在 Linux 环境**进行（行为基准），但 Windows 上 `go build/vet/test ./...`
> 也应全绿 —— 二者都跑通才算完整验收。Windows 端到端需机器上有可用 bash
> （Git for Windows 或 WSL）。详见 `internal/tools/README.md` §6。

- `go build ./...` 成功，无 vet 错误（三平台）
- `go test ./...` 全部通过（Linux 为准；Windows 亦应全绿）
- 主程序启动后在 `http://127.0.0.1:<port>/` 看到 WebUI
- scout 索引服务（`:8480`）可响应 `/search` 和 `/reindex`
- 插件/skill 的 `meta.json` 含非空 `summary` 字段
