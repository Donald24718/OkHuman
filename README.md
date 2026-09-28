# OkHuman

**一个进程 = 一个 agent。** Go 单仓实现的个人 agent 运行时：每个实例是一个独立进程，
自带系统提示词、消息队列、上下文管理、工具循环和 WebUI；agent 只有一个元工具
`bash`，一切能力（浏览器、QQ、语音、定时、素材注入……）都以**解耦插件**的形式挂在旁边，
通过 HTTP 接口交互——主程序对插件零感知。

## 快速开始

```bash
# 0) 依赖：Go 1.22+；一个 OpenAI 兼容的 LLM 端点（llama.cpp / Ollama / vLLM 均可）
# 1) 编译主程序
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
- **前台超时自动转后台**：工具调用与 30s 前台超时赛跑，超时不取消、转后台继续，
  结果自动注入消息队列——模型不必傻等，结果也不丢。
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

更多设计思想见 [README-overview.md](README-overview.md)。

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

"多 agent"= 多个独立端口的进程。推荐的三角色拓扑：

- **生产**：权威树，插件/配置/提示词以它为准
- **副本**：跟随生产的只读镜像（单向 rsync --delete），日常容灾
- **救生艇**：冻结旧树的进程，平时不起、永不更新——生产与副本都死时救场

二进制交换协议：主程序新二进制**只换对方实例的运行树**，且必须先经一次性实例
端到端验证；新二进制严禁同时落两棵树（旧二进制实例就是救生艇）。换前问一句：
"此刻如果新二进制有 bug，谁还活着能救？"答不上就不换。

## 许可

MIT（见 [LICENSE](LICENSE)）。
