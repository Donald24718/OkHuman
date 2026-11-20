# asr — 排障

- **`start.sh` 卡在"启动中"**：模型加载中会返回 503，脚本只认 HTTP 200，会一直等到 120 秒。看 `tail -f /tmp/asr_plugin.log`。
- **`libggml*.so` 找不到**：`bin/` 的 RUNPATH 指向编译时目录，必须靠 `LD_LIBRARY_PATH=$HERE/bin`（`start.sh` 已设）。手工起 llama-server 时别忘了。
- **识别把英文词听错**（TTS 合成音尤其明显：`TTS`→`DDS`、`words`→`Voice`、`8092`→"八千零九十二"）：模型固有误差，不是服务问题。真人语音明显好于合成音。
- **转写结果是空的**：先看是不是传了相对路径（`transcribe.sh` 会静默返回空），再看服务在不在听（`./start.sh` 起，探活只认 HTTP 200）。
- **端口被占**：改 `config.json` 的 `port`，同时记得 voice-chat 那边 `ASR_URL` 也要改。
