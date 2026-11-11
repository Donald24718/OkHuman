#!/bin/bash
pkill -f "llama-server.*Qwen3-ASR" && echo "[asr] 已停止" || echo "[asr] 本来就没在跑"
