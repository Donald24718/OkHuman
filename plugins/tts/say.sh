#!/bin/bash
# 只合成一句话，不拉起语音对话：./say.sh "要念的文本" [输出wav路径]
# 服务没起时会自动拉起（首次加载模型约 5-15 秒）
[ -n "$1" ] || { echo "用法: $0 <文本> [输出.wav]"; exit 1; }
cd "$(dirname "$0")"
PORT=$(python3 -c "import json;print(json.load(open('config.json')).get('port',8092))" 2>/dev/null || echo 8092)
OUT="${2:-/tmp/tts_say_$(date +%s).wav}"
curl -s --max-time 2 "http://127.0.0.1:$PORT/health" >/dev/null 2>&1 || ./start.sh >/dev/null 2>&1
code=$(curl -s -o "$OUT" -w '%{http_code}' --max-time 300 \
  -X POST "http://127.0.0.1:$PORT/tts" -H 'Content-Type: application/json' \
  -d "{\"text\":$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$1"),\"format\":\"wav\"}")
[ "$code" = "200" ] && echo "$OUT" || { echo "✗ HTTP $code"; head -c 200 "$OUT"; exit 1; }
