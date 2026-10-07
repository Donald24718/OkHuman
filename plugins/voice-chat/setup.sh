#!/bin/bash
# voice-chat 一次性环境安装（开源版）：
#   1) Python 环境（主 venv + Kokoro TTS venv）
#   2) ASR 模型（Qwen3-ASR-0.6B gguf + mmproj，需自备或设下载 URL）
#   3) llama-server 二进制（CPU build，需 mtmd/mmproj 支持）
# 已存在的步骤自动跳过。模型/音色（Kokoro）首次运行时自动从 hf-mirror 下载。
set -e
cd "$(dirname "$0")"
HERE="$(pwd)"

# ── 1) 主 venv（VAD/声纹/ASR 客户端）──
if [ ! -x venv/bin/python ]; then
  echo "[setup] 创建 venv ..."
  python3 -m venv venv
  venv/bin/pip install --upgrade pip
  venv/bin/pip install torch --index-url https://download.pytorch.org/whl/cpu
  venv/bin/pip install numpy soundfile requests jieba cn2an
fi

# ── 2) Kokoro TTS venv ──
if [ ! -x kokoro_venv/bin/python ]; then
  echo "[setup] 创建 kokoro_venv ..."
  python3 -m venv kokoro_venv
  kokoro_venv/bin/pip install --upgrade pip
  kokoro_venv/bin/pip install torch --index-url https://download.pytorch.org/whl/cpu
  kokoro_venv/bin/pip install kokoro misaki phonemizer
  # phonemizer 的 espeak 后端需要系统 espeak-ng：
  #   Debian/Ubuntu: sudo apt install espeak-ng
fi

# ── 3) ASR 模型 ──
mkdir -p asr/models
if [ ! -f asr/models/Qwen3-ASR-0.6B-bf16.gguf ]; then
  # Qwen3-ASR-0.6B 的 bf16 gguf + mmproj。官方权重可用 llama.cpp 自行转换；
  # 已有文件直接拷到 asr/models/，或设这两个变量给下载 URL：
  ASR_URL=${ASR_URL:-}
  ASR_MMPROJ_URL=${ASR_MMPROJ_URL:-}
  if [ -n "$ASR_URL" ]; then
    curl -L -o asr/models/Qwen3-ASR-0.6B-bf16.gguf "$ASR_URL"
    curl -L -o asr/models/mmproj-Qwen3-ASR-0.6B-bf16.gguf "$ASR_MMPROJ_URL"
  else
    echo "[setup] ⚠ ASR 模型缺失：把 Qwen3-ASR-0.6B-bf16.gguf 和"
    echo "        mmproj-Qwen3-ASR-0.6B-bf16.gguf 放到 asr/models/，"
    echo "        或用 ASR_URL=... ASR_MMPROJ_URL=... 重跑本脚本。"
  fi
fi

# ── 4) llama-server（CPU build，需支持 mmproj/mtmd）──
if [ ! -x asr/bin/llama-server ]; then
  mkdir -p asr/bin
  if command -v llama-server >/dev/null 2>&1; then
    cp "$(command -v llama-server)" asr/bin/llama-server
    echo "[setup] llama-server 已从 PATH 复制到 asr/bin/"
  else
    echo "[setup] ⚠ asr/bin/llama-server 缺失：编译/安装支持 mtmd 的 llama.cpp，"
    echo "        把 llama-server（及其依赖 .so）放到 asr/bin/，或加入 PATH。"
  fi
fi

echo "[setup] 完成。首次 ./start.sh 后：对着麦克风说几句，"
echo "        curl \"http://127.0.0.1:8091/enroll?wav=/path/你的录音.wav\" 注册声纹。"
