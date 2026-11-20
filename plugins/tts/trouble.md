## 排障

- **`'http' is not a package`**：本目录下的文件不能叫 `http.py`——脚本目录在 `sys.path`
  最前面会遮蔽标准库 `http` 包（这个坑踩过，故改名 `server.py`）。
- **英文念不出来 / 静音**：Kokoro v1.0 中文管线走 legacy_call，`en_callable` 不被调用，
  英文会原样透传导致静音。`engine.py` 的 `ZhG2PWithEnglish` 用 espeak IPA 补拉丁段，
  别绕过它直接用 `KPipeline`。
- **首次合成慢**：模型加载 5–15 秒 + jieba 建缓存；常驻服务复用进程即可，
  别每次新起进程（`say.sh` 已自动复用）。
- 模型权重从 `hf-mirror.com` 拉（`HF_ENDPOINT` 已在 engine.py 里设默认）。

## 实测（本机 2026-10-16）

- 一句 30 字中英混读 → **7.42 秒**音频（24 kHz / 单声道 / 16 bit），CPU 合成 RTF≈0.35
