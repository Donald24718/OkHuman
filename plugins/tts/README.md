# tts — 独立的文字→语音服务（本地模型，中英混读）— 母文档只做路由

只想要「文本→声音」时不必拉起整套语音对话（整套见条目 `voice-chat`）。要用某个人的声音（零样本音色克隆）才起另一套，见 `references/index-tts.md`。

## 要做什么 → 读哪份

| 要做什么 | 文件 |
|---|---|
| 起服务、停服务、一句话合成 | `run.md` |
| 接口与参数（返回 wav 路径 / 直接取音频） | `api.md` |
| 配置项 | `config.md` |
| 两套协议的区别（别搞混） | `protocols.md` |
| 报错与实测耗时/显存 | `trouble.md` |
| 零样本音色克隆的跑法、耗时显存、男女声判定 | `references/index-tts.md` |

## 遇到什么现象 → 读哪份

| 现象 | 文件 |
|---|---|
| 服务起不来、端口被占 | `run.md`、`trouble.md` |
| 返回格式不对、拿不到音频字节 | `protocols.md`、`api.md` |
| 克隆出来的音色是乱语 | `references/index-tts.md` |
| 合成慢、显存挤不下 | `trouble.md`（让卡见 `gpu-window`） |
