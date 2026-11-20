## 两种协议，别搞混

- **`server.py`（HTTP，:8092）**：给外部调用方用，本插件 `say.sh` 走这条。
- **`serve.py`（stdin 一行文本 → outdir 出 wav）**：**voice-chat 仍在用这条**，
  就绪标记是日志里的 `[kokoro] Ready`。改协议要同时改 voice_loop 的 `KokoroTTSServer`。
- 两者共用 `engine.py`（模型加载 + 中英混读 G2P），只有一份实现。
