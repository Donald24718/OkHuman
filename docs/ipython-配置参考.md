# ipython 元工具 · 配置参考

> 面向使用者与维护者。所有结论均对照代码与本机实测，不是推测。
> 设计依据（为什么这么选型）见同目录另三篇，本文只讲「怎么配、配了发生什么」。

---

## 一、一句话

**只有一個开关：`tools.ipython_python`。** 它是一个字符串，三种取值决定一切：

| 值 | 行为 |
|---|---|
| `""`（出厂默认） | **不注册**该工具，工具表里仍然只有 `bash` |
| `"auto"` | 启动时按 `python3` → `python` 探测第一个**能 `import IPython`** 的解释器 |
| 绝对路径 | 直接用它（正斜杠 `/`、反斜杠 `\` 都行） |

---

## 二、配置面总表：哪些路真能走

| 途径 | 生效 | 持久 | 说明 |
|---|---|---|---|
| `config/user.json` 写 `tools.ipython_python` | ✅ | ✅ | **推荐**。重启后仍在 |
| `POST /config`（body 套 `"patch"`） | ✅ | ✅ | 热更新，不用重启；同样落盘 `user.json` |
| WebUI 配置页（「工具」段） | ✅ | ✅ | 2026-10-05 起有 `ipython_python` 输入框；保存即时生效，成功/失败都在该页下方列出 |
| `OKHUMAN_IPYTHON_PYTHON` 环境变量 | ❌ | — | **运行时不认**，只在测试里读（见 §六） |

**优先级链**（`internal/config/config.go`）：

```
内置默认值  <  config/default.json  <  config/user.json  <  环境变量
```

⚠️ **环境变量那一级对 ipython 不存在**：`internal/config` 只认 5 个环境变量
（`OKHUMAN_PORT` / `OKHUMAN_LLM_BASE_URL` / `OKHUMAN_LLM_MODEL` /
`OKHUMAN_DATA_DIR` / `OKHUMAN_FAKE`），`ipython_python` 不在其中。

---

## 三、三份文件各自的角色

| 文件 | `ipython_python` | 角色 | 该不该改 |
|---|---|---|---|
| `config/default.json` | `""` | 出厂默认 | ❌ 别改（改了所有新克隆默认启用） |
| `config/user.json.example` | `""` | 新用户模板，供 `cp` | ❌ 模板，不生效 |
| `config/user.json` | 你填的值 | **实际生效的覆盖层** | ✅ 改这里 |

模板里另有 `_comment` 字段写用法说明——它是字符串字段，走结构体 Unmarshal 时
被忽略，安全。**模板必须是严格合法 JSON**（不支持 `//` 注释行），否则照文档
`cp` 的新用户启动即失败。

---

## 四、`"auto"` 的真实行为（`detect.go`）

```
python3 → python  （只这两个候选，没有第三个）
```

每步做三件事：

1. `exec.LookPath` 找路径；
2. **跳过 Microsoft Store 存根**：路径含 `microsoft/windowsapps/` 的直接跳过——
   那是 App Execution Alias，运行它会弹商店且不退出；
3. 跑 `python -c "import IPython"` 验证，单次**兜底超时 15 秒**。

**探测失败不阻断启动**，只在 stderr 留一行 `⚠️ ipython 工具未启用：…`，工具表
保持原样。

⚠️ **本机实测的坑**：PATH 上第一个 `python` 是 WorkBuddy 托管的 3.13.12，**没装
IPython**；装了 IPython 9.17.1 的是系统 Python311。所以 `auto` 在这台机器上
必然静默不启用——**必须写绝对路径**。

---

## 五、热更新：`POST /config`

```bash
curl -X POST http://127.0.0.1:8451/config -H 'Content-Type: application/json' \
  -d '{"patch":{"tools":{"ipython_python":"C:/Users/me/.../python.exe"}}}'
```

三个硬性约束（`server.go:1445` 起）：

1. **body 必须套一层 `"patch"`** —— 直接发 `{"tools":{...}}` 返回 400。
2. **只允许已知段**：`llm` / `context` / `tools` / `doom` / `server` / `data` /
   `system_prompt`，其它段返回 400。
3. 段值必须是 JSON 对象。

生效路径：`has("tools")` → `tools.ConfigureIPython(...)` → **重建工具表**，
不需要重启。

### 一个反直觉的细节：设成 `""` 关闭时，那一行会消失

落盘前会跑 `DiffAgainstBase`（`config.go:196`），**把「与出厂默认相同」的叶子键
剔除**，以保持 `user.json` 最小。设 `""` 等于默认 → 被剔除 → `user.json` 里
`ipython_python` 这行消失。

**这是正常的**，效果仍是禁用（default 就是 `""`）。但别因此以为"没生效"。
本机实测（2026-10-05）：

```
POST /config  {"patch":{"tools":{"ipython_python":""}}}
  → user.json 的 "tools" 段整段消失（段里只剩一个被剔除的键）
  → 生效值 ''
```

注意是**整段消失**，不只是那一行——如果 `tools` 段里你还有别的覆盖（如
`timeout_ms`），段不会消失，只是那一行被摘掉。

> 另：热更新返回的 `applied` 文案是
> `tools（前台超时/命令总时长上限/结果上限，下条命令起生效）`——**里面没提
> ipython**（文案硬编码，未随新字段更新）。看到这个提示不代表改错了。

---

## 六、环境变量只对测试有效

`OKHUMAN_IPYTHON_PYTHON` 只在两处被读取，都在测试里：

- `internal/tools/ipython/kernel_integration_test.go:20`
- `internal/tools/ipython/ipython_spec_test.go:88`

```bash
OKHUMAN_IPYTHON_PYTHON=$(command -v python3) go test ./internal/tools/... -count=1
```

**它不是运行时开关**。拿它启动实例会静默不生效——环境里设了但工具没出现，
先怀疑这个。

---

## 七、不存在的配置项（别照旧文档找）

| 你可能会找 | 实际 |
|---|---|
| `ipython.total_timeout_ms` | ❌ 不存在。上限复用 `tools.timeout_ms`（默认 600 s） |
| `ipython.enabled: true` | ❌ 不存在。空串即禁用，无需布尔开关 |
| 独立的「单次执行超时」配置项 | ❌ 常量 `DefaultIPythonMS = 60000`（60 s），代码内写死 |

> 这三样出现在 `docs/ipython-最优解-级联中断实证.md` 的**提案**形态里，
> 落地时收敛成了一个字符串键。照旧示例改会白改。

---

## 八、派生路径（不是配置项）

图片 / PDF / SVG 落盘到：

```
<数据目录>/ipython-output/
```

数据目录由 `OKHUMAN_DATA_DIR` 或默认 `$HOME/.okhuman-<port>` 决定，**不能单独
改 ipython 的输出目录**。放在数据目录而非临时目录是有意的：临时目录会被系统
清理，而模型可能隔几轮才来读这张图。

---

## 九、多实例：两种情形别混为一谈

| 情形 | 配置 | 工作区 |
|---|---|---|
| **同一棵代码树起多个进程** | **共享同一份 `config/user.json`**，配一次全部生效 | 数据目录按端口区分：`$HOME/.okhuman-<port>`；端口冲突自动 +1（`main.go:96`） |
| **多棵代码树**（生产 / 副本 / 救生艇） | 各有自己的 `config/user.json`，**要逐个开启** | 各自独立 |

判据：配置路径由 `Config.Load(root)` 按**可执行文件所在目录**解析
（`config.go:93`）——同一个 exe 目录就是同一份配置，与起了几个进程、占哪个端口无关。

⚠️ 一个副作用：多实例共享配置时，`server.port` 也在同一份文件里。后起的实例因
端口冲突自动 +1，**若它把新端口写回 `user.json`，会"偷走"先起实例的端口身份**
（AGENTS.md 已就此警告：不要手动删 `user.json` 的 port 键）。

---

## 十、排错清单

| 现象 | 先看这里 |
|---|---|
| 模型说"没有 ipython 工具" | `GET /config` 的生效值是不是 `""`；启动日志有没有 `ipython 工具已启用（…）` |
| 启动日志里没有启用行 | 注意那行走 **stdout**（`main.go:78`），**不在 `okhuman.log`** 里——只在启动重定向的那个文件 |
| 配了但没生效 | 是不是改错文件（`default.json` / `example` 不生效）；生效值查 `GET /config` 的 `effective` 段 |
| `auto` 没启用 | PATH 上第一个解释器没装 IPython（本机就是），改绝对路径 |
| 热更新返回 400 | body 少套 `patch`，或用了未知段 |
| 改完想确认 | `curl http://127.0.0.1:<port>/config` → `effective.tools.ipython_python` |

**关于 KV 前缀（2026-10-05 修正）**：启用会让工具表多一项 → 发给 LLM 的 tools
JSON 变化。但实测表明「首轮 miss、次轮命中」是这个后端的**常态**——连续三轮
`cached = 0 → 3131 → 3131`，且**未改任何配置**时隔一段时间的首轮同样 miss。
因此「启用导致 KV 前缀失效一次」这一因果**待 A/B 证实**，不该当作默认关闭的
强理由。

**默认关闭的真正理由**：不改动既有部署的工具契约——未配置时工具表与现状逐字
一致，由 `TestIPythonAbsentByDefault` 守护。

---

## 十一、已评估但暂不实现的途径（2026-10-05 决定）

| 途径 | 状态 | 理由 |
|---|---|---|
| 运行时环境变量 `OKHUMAN_IPYTHON_PYTHON` | **暂不做** | 同树多实例**共享配置**，「逐个改文件」的痛点不成立；仅剩「临时换解释器」收益，不强 |
| ~~WebUI 配置页加字段~~ | **已实现**（2026-10-05） | 成本仅 `CFG_SCHEMA` 里一行——表单是数据驱动的，后端零改动 |
| 独立超时 `tools.ipython_timeout_ms` | 暂不做 | 新增配置项需三处同步（`default.json` + `user.json.example` + AGENTS.md 表格）；60 s 目前够用 |
| 内核参数配置化（闲置 10 min / 上限 8） | 暂不做 | 同上，且当前值够用 |

> ⚠️ **别混淆**：`OKHUMAN_IPYTHON_PYTHON` 环境变量**在测试里照旧有效**
> （`kernel_integration_test.go:20`、`ipython_spec_test.go:88`）——它只是**运行时
> 不认**。跑集成测试的命令不受此决定影响。
