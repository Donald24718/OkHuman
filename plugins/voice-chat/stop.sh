#!/bin/bash
# 停止语音闭环（只停本插件进程。ASR/TTS 已拆成独立插件，
# 它们的启停请用 asr/stop.sh 与 tts/stop.sh，这里不误伤共享服务）
pkill -f "voice-chat/voice_loop.py" 2>/dev/null && echo "[stop] voice_loop 已停" || echo "[stop] voice_loop 未在运行"
pkill -f "voice-chat/webui.py" 2>/dev/null; pkill -f "python webui.py" 2>/dev/null && echo "[stop] 控制台 WebUI 已停" || echo "[stop] 控制台 WebUI 未在运行"
pkill -f "speaker/speaker_server.py" 2>/dev/null && echo "[stop] 声纹门控已停" || echo "[stop] 声纹门控未在运行"
