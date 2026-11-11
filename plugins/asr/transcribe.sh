#!/bin/bash
# 只转写一个音频文件，不拉起整套语音对话：./transcribe.sh speech.wav
# 用法: ./transcribe.sh <音频文件> [语言=Chinese]
# 音频要求：16 kHz 单声道最佳（wav/flac/mp3/ogg 均可，llama-server 内部解码）
[ -f "$1" ] || { echo "用法: $0 <音频文件>"; exit 1; }
cd "$(dirname "$0")"
PORT=$(python3 -c "import json;print(json.load(open('config.json')).get('port',8090))" 2>/dev/null || echo 8090)
NAME=$(python3 -c "import json;print(json.load(open('config.json')).get('model_name','qwen3-asr-0.6b'))" 2>/dev/null || echo qwen3-asr-0.6b)
# 语言默认 Chinese（与 voice-chat 调用一致），可传 English / auto 覆盖
LANG="${2:-Chinese}"
curl -s --max-time 180 "http://127.0.0.1:$PORT/v1/audio/transcriptions" \
    -F "file=@$1" -F "model=$NAME" -F "language=$LANG" | python3 -c '
import sys,json,re
raw=sys.stdin.read()
try: t=json.loads(raw).get("text","")
except Exception: print(raw); raise SystemExit
m=re.search(r"<asr_text>(.*)$", t, re.S)     # 模型输出带 <asr_text> 前缀，剥掉
print((m.group(1) if m else t).strip())'
