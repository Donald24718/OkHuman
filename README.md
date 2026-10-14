# OkHuman

**一个进程 = 一个 agent。** Go 单仓实现的个人 agent 运行时：每个实例是一个独立进程，
自带系统提示词、消息队列、上下文管理、工具循环和 WebUI；agent 只有一个元工具
`bash`，一切能力（浏览器、QQ、语音、定时、素材注入……）都以**解耦插件**的形式挂在旁边，
通过 HTTP 接口交互——主程序对插件零感知。

## 快速开始

```bash
# 0) 依赖：一个 OpenAI 兼容的 LLM 端点（llama.cpp / Ollama / vLLM 均可）
#    不想编译的话：Releases 里有 Actions 自动构建的包（linux/amd64、linux/arm64、darwin/arm64），解压即用
# 1) 编译主程序（Go 1.22+）
go build -o okhuman .

# 2) 配置（默认值已可跑；按需改 config/user.json）
cp config/user.json.example config/user.json
#    把 llm.base_url 指到你的 LLM 端点，例如 http://127.0.0.1:8080/v1

# 3) 启动（数据目录默认 $HOME/.okhuman，提示词从 prompts/ 热加载）
(setsid ./okhuman < /dev/null > /tmp/okhuman.log 2>&1 &)

# 4) 打开 WebUI
open http://127.0.0.1:8451/
```

插件同样 `cd plugins/<名字> && go build`（见各插件 README）。需要语义检索能力时
再起 `plugins/scout`。

## 架构亮点

- **上下文滚动压缩**：总 token 超预算时，旧消息批次交给 LLM 压成总结滚动进
  summary；迭代压缩最多 3 轮取最短保底，压缩过程流式可见。
- **前台超时自动转后台**：工具调用与 30s 前台超时赛跑（`tools.fg_timeout_ms`）。
  超时**不取消执行**，只回填一条"已转后台"的 tool 消息（保持 tool_calls 与 tool 严格配对），
  并把结果通道交给后台编排器登记；命令总时长另有 `tools.timeout_ms` 硬上限（到点杀进程组）。
  结果回来后走两条路之一：本轮未结束 → 在下次 LLM 调用前**搭车注入**（纯追加，不加调用、
  不加 KV 代价）；本轮已收笔 → 留在队列，下次运行作为新消息触发。模型不必傻等，结果也不丢。
- **轮末校验（end_check）+ 后台任务闸门**：模型输出结束且无 tool_calls（=停止信号）时，
  注入一条校验提示再问一次——仍无工具调用才正式收笔（回复取校验前正文），有则继续干活；
  每次停止至多校验一次，`agent.end_check=false` 可全关。校验提示的**措辞就是判据**：
  问"此刻有没有必须你现在就做的动作"，而不是"有没有还未完成的事情"（后者会被读成
  "任务没全完=有"，导致等待期间反复被追问）。更进一步，收笔前先查编排器里有没有未 settle
  的后台任务，有则连问都不问、直接收笔（`end_check_skipped`）——实测一次大模型下载期间
  被追问十余次，每次都是一次完整推理，而下载一次都不需要模型。
- **死循环检测（doom）**：连续 3 次完全相同的工具调用 → 注入警告；仍重复 → 强停本轮。
- **生命自感知**：系统提示词首 prepend 一块"[你的生命]"（本实例端口/PID/源码位置 +
  所接入 LLM 服务的端口/PID，每次调用现算）——明确告诉模型：动这些 PID 和端口
  就是伤它自己。
- **提示词分层 + 热加载**：`prompts/*.md` 拼接为系统提示词，`POST /prompts/reload`
  热加载；部署/插件规范等专用层不进系统提示词，agent 按需读。
- **彻底解耦契约**：插件不 import 主程序、不共享进程/内存，只走公开 HTTP 接口；
  配置自管、数据自管、独立可编译。插件挂了不影响主程序，主程序重启不影响插件。
- **署名先于记忆**：agent 的 git 提交带 `Provenance: agent(<名字> @<端口>)` trailer，
  账本先承载"谁写的"，才谈得上承载过去。

更多设计思想见 [README-overview.md](README-overview.md)；
运行时机制（转后台 / 结果回投 / 轮末校验 / 闸门 / 配置与事件）逐条对应代码，
见 [docs/runtime-mechanics.md](docs/runtime-mechanics.md)。

## 配置

一切可调参数集中在 `config/`，优先级：内置默认 < `config/default.json` <
`config/user.json` < 环境变量（`OKHUMAN_PORT` / `OKHUMAN_LLM_BASE_URL` /
`OKHUMAN_LLM_MODEL` / `OKHUMAN_DATA_DIR`）。所有路径相对**可执行文件所在目录**解析，
仓库 clone 到哪都能跑，无需改任何绝对路径。

## 插件（plugins/）

| 插件 | 干什么 |
|---|---|
| attach | 素材注入上下文：图片原图/压缩，视频按策略直通 input_video（自动压 360p/分段） |
| browser | browserctl 轻 CLI 驱动本地 Firefox：导航、定位、填表、点击、取文本 |
| computer | 给 agent 装"手"：整屏截图注入 + X11 鼠标键盘事件 |
| cron | 定时任务（:8601），到点把 prompt 作为 user 消息 POST 到对应实例 /chat |
| okmon | 进程监控 + 启停管理（webui :8496） |
| qq-channel | QQ 官方渠道桥：QQ 消息 ↔ agent 双向转发（独立常驻进程，无监听端口） |
| scout | 插件/技能语义索引（:8480）：关键词 + Qwen3-Embedding 语义向量打分 |
| voice-chat | 语音全闭环：麦克风 → VAD → 声纹门控 → ASR → 唤醒词路由 → Kokoro TTS 朗读 |

布局与管理规范见 [plugins/RULE.md](plugins/RULE.md)。每个插件都是独立 Go 模块
（独立 git 仓、自带 config.json 与 data/），改完源码必须重编 + 真实启动验证。

**技能库**（`plugins/skills/`）：skill = 本机实测验证过的操作经验（SKILL.md），
日期层级存放，同样进 scout 索引。本仓不预置技能——技能是环境相关的，
请在你自己的机器上实测后积累。

## 多实例部署

"多 agent"= 多个独立端口的进程。推荐至少跑两个实例互为备份：

- **实例之间平等（自举）**：源码保持一致——改动先落一处，再同步到另一处；
  **配置与提示词各自独立**，每个实例可以有自己的身份提示词和部署参数，互不覆盖。
- **升级二进制时留一个旧版活着**：别两棵树同时换新。旧二进制实例就是救生艇，
  新二进制出问题时它还能起来救场。
- **换二进制由人指示，agent 不主动换**：改完源码不擅自往任何运行树落二进制，
  等人说"换"再换。

## 许可

MIT（见 [LICENSE](LICENSE)）。
