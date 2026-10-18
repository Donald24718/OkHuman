#!/bin/bash
# 启动 TTS 常驻服务（Kokoro-82M，HTTP :8092）。模型在后台加载，就绪前 /health 返回 ok:false
set -e
cd "$(dirname "$0")"; HERE="$(pwd)"
CFG="$HERE/config.json"
g(){ python3 -c "import json;print(json.load(open('$CFG')).get('$1','$2'))" 2>/dev/null || echo "$2"; }
export TTS_PORT="$(g port 8092)" TTS_HOST="$(g host 127.0.0.1)" TTS_VOICE="$(g voice zf_xiaoyi)" TTS_OUTDIR="$(g outdir /tmp/tts_http_out)"
if curl -s --max-time 2 "http://127.0.0.1:$TTS_PORT/health" >/dev/null 2>&1; then
  echo "[tts] 已在运行 (http://127.0.0.1:$TTS_PORT)"; exit 0
fi
nohup "$HERE/venv/bin/python" "$HERE/server.py" > /tmp/tts_plugin.log 2>&1 &
echo "[tts] 启动中 :$TTS_PORT（模型加载约 5-15 秒）"
for i in $(seq 1 120); do
  curl -s --max-time 2 "http://127.0.0.1:$TTS_PORT/health" 2>/dev/null | grep -q '"ok": *true' && { echo "[tts] ✓ 就绪 pid $(pgrep -f "tts/server.py" | head -1)"; exit 0; }
  sleep 1
done
echo "[tts] ✗ 120 秒未就绪，看 /tmp/tts_plugin.log"; tail -5 /tmp/tts_plugin.log | sed 's/^/    /'; exit 1
