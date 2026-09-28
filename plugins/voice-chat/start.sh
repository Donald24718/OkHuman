#!/bin/bash
# 启动语音闭环（OkHuman 插件 voice-chat，全自包含，无外部项目依赖）：
#   声纹门控(8091) + ASR llama-server(8090) + voice_loop（含 TTS serve）
set -e
cd "$(dirname "$0")"
HERE="$(pwd)"

PY="$HERE/venv/bin/python"

# 资产检查（开源版：先跑 setup.sh）
if [ ! -x "$PY" ] || [ ! -f "$HERE/asr/bin/llama-server" ] || [ ! -f "$HERE/asr/models/Qwen3-ASR-0.6B-bf16.gguf" ]; then
  echo "[start] ⚠ 环境资产不全（venv / asr/bin/llama-server / asr/models/*.gguf 之一缺失）"
  echo "[start]   先运行: ./setup.sh"
  exit 1
fi

# ── OkHuman 实例（agent 腿，解耦契约：只走 HTTP）──
OKHUMAN_URL="$( "$PY" -c 'import json;print(json.load(open("config.json")).get("okhuman_url","http://127.0.0.1:8451"))' 2>/dev/null || echo http://127.0.0.1:8451 )"
if curl -s --max-time 2 "$OKHUMAN_URL/health" >/dev/null 2>&1; then
  echo "[start] OkHuman 可达: $OKHUMAN_URL"
else
  echo "[start] ⚠ OkHuman 不可达 ($OKHUMAN_URL)——语音轮次会逐次报错重试，先把它拉起来"
fi

# ── 声纹门控服务 (CAM++) ──
GATE_PORT=8091
export VOICE_GATE_THRESHOLD="$( "$PY" -c 'import json;print(json.load(open("config.json")).get("gate_threshold",0.60))' 2>/dev/null || echo 0.60 )"
if ! curl -s --max-time 2 http://127.0.0.1:$GATE_PORT/status >/dev/null 2>&1; then
  echo "[start] 启动声纹门控 (CAM++ 8091, 阈值 $VOICE_GATE_THRESHOLD) ..."
  nohup "$PY" "$HERE/speaker/speaker_server.py" > /tmp/speaker_gate.log 2>&1 &
  for i in $(seq 1 90); do
    curl -s --max-time 2 http://127.0.0.1:$GATE_PORT/status >/dev/null 2>&1 && break
    sleep 1
  done
  echo "[start] 声纹门控就绪 (pid $(pgrep -f "voice-chat/speaker/speaker_server.py" | head -1))"
else
  echo "[start] 声纹门控已在运行"
fi

# ── ASR 服务器 (Qwen3-ASR-0.6B, CPU) ──
PORT=8090
BIN="$HERE/asr/bin"
MODEL_DIR="$HERE/asr/models"
if ! curl -s --max-time 2 http://127.0.0.1:$PORT/v1/models >/dev/null 2>&1; then
  echo "[start] 启动 ASR 服务器 (Qwen3-ASR-0.6B, CPU) ..."
  # RUNPATH 指向旧编译目录, 用 LD_LIBRARY_PATH 指向本插件 asr/bin
  nohup env LD_LIBRARY_PATH="$BIN" "$BIN/llama-server" \
    -m "$MODEL_DIR/Qwen3-ASR-0.6B-bf16.gguf" \
    --mmproj "$MODEL_DIR/mmproj-Qwen3-ASR-0.6B-bf16.gguf" \
    --host 127.0.0.1 --port $PORT -t 16 -c 8192 \
    > /tmp/voice_loop_asr.log 2>&1 &
  for i in $(seq 1 90); do
    curl -s --max-time 2 http://127.0.0.1:$PORT/v1/models >/dev/null 2>&1 && break
    sleep 1
  done
  echo "[start] ASR 服务器就绪 (pid $(pgrep -f "llama-server.*Qwen3-ASR" | head -1))"
else
  echo "[start] ASR 服务器已在运行"
fi


# ── 控制台 WebUI (:8495) ──
if curl -s --max-time 2 http://127.0.0.1:8495/ >/dev/null 2>&1; then
  echo "[start] 控制台 WebUI 已在运行"
else
  nohup "$PY" "$HERE/webui.py" > /tmp/voice_chat_webui.log 2>&1 &
  sleep 1
  echo "[start] 控制台 WebUI 已启动 (http://127.0.0.1:8495/)"
fi

echo "[start] 启动 voice loop（首次 TTS 模型加载约 10s）"
exec "$PY" "$HERE/voice_loop.py"
