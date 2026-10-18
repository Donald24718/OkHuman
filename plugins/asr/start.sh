#!/bin/bash
# 启动 ASR 服务（Qwen3-ASR-0.6B，llama.cpp CPU build，OpenAI 兼容接口）
set -e
cd "$(dirname "$0")"; HERE="$(pwd)"
CFG="$HERE/config.json"
g(){ python3 -c "import json,sys;print(json.load(open('$CFG')).get('$1',$2))" 2>/dev/null || echo "$2"; }
PORT="$(g port 8090)"; HOST="$(g host 127.0.0.1)"
MODEL="$HERE/$(g model models/Qwen3-ASR-0.6B-bf16.gguf)"
MMPROJ="$HERE/$(g mmproj models/mmproj-Qwen3-ASR-0.6B-bf16.gguf)"
THREADS="$(g threads 16)"; CTX="$(g ctx 8192)"
if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://$HOST:$PORT/v1/models" 2>/dev/null)" = "200" ]; then
  echo "[asr] 已在运行 (http://$HOST:$PORT)"; exit 0
fi
[ -f "$MODEL" ] || { echo "[asr] ✗ 缺模型: $MODEL"; exit 1; }
echo "[asr] 启动 llama-server :$PORT (threads=$THREADS ctx=$CTX)"
# bin 的 RUNPATH 指向编译时目录，必须用 LD_LIBRARY_PATH 指回本插件 bin
nohup env LD_LIBRARY_PATH="$HERE/bin" "$HERE/bin/llama-server" \
  -m "$MODEL" --mmproj "$MMPROJ" --host "$HOST" --port "$PORT" \
  -t "$THREADS" -c "$CTX" > /tmp/asr_plugin.log 2>&1 &
for i in $(seq 1 120); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://$HOST:$PORT/v1/models" 2>/dev/null)" = "200" ] && { echo "[asr] ✓ 就绪 pid $(pgrep -f "llama-server.*Qwen3-ASR" | head -1)"; exit 0; }
  sleep 1
done
echo "[asr] ✗ 120 秒未就绪，看 /tmp/asr_plugin.log"; tail -5 /tmp/asr_plugin.log | sed 's/^/    /'; exit 1
