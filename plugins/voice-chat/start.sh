#!/bin/bash
# 启动语音闭环（OkHuman 插件 voice-chat，全自包含，无外部项目依赖）：
#   声纹门控(8091) + ASR llama-server(8090) + voice_loop（含 TTS serve）
set -e
cd "$(dirname "$0")"
HERE="$(pwd)"

PY="$HERE/venv/bin/python"

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

# ── ASR：已拆成独立插件 asr/，这里只做探活 + 复用它的 start.sh ──
PORT=8090
# 本插件可能被挪进日期层级目录，逐级向上找 Frequently Used Plugins/asr
ASR_DIR=""
_d="$HERE"
for _i in 1 2 3 4 5 6 7 8; do
  _c="$_d/Frequently Used Plugins/asr"
  [ -d "$_c" ] && { ASR_DIR="$_c"; break; }
  _d="$(dirname "$_d")"
done
[ -n "$ASR_DIR" ] || ASR_DIR="$HERE/../asr"
if curl -s --max-time 2 http://127.0.0.1:$PORT/v1/models >/dev/null 2>&1; then
  echo "[start] ASR 已在运行 (:$PORT，由 asr 插件提供)"
elif [ -x "$ASR_DIR/start.sh" ]; then
  echo "[start] ASR 未运行，调用 asr 插件启动：$ASR_DIR/start.sh"
  "$ASR_DIR/start.sh" || echo "[start] ⚠ ASR 启动失败——语音识别会逐次报错重试"
else
  echo "[start] ⚠ 找不到 asr 插件（$ASR_DIR），且 :$PORT 无服务——ASR 不可用"
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
