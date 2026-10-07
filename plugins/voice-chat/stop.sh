#!/bin/bash
# 停止语音闭环（本插件内所有进程；模式均限定本插件路径/独有特征，不误伤其他进程）
pkill -f "voice-chat/voice_loop.py" 2>/dev/null && echo "[stop] voice_loop 已停" || echo "[stop] voice_loop 未在运行"
pkill -f "voice-chat/webui.py" 2>/dev/null; pkill -f "python webui.py" 2>/dev/null && echo "[stop] 控制台 WebUI 已停" || echo "[stop] 控制台 WebUI 未在运行"
pkill -f "voice-chat/kokoro_serve.py" 2>/dev/null && echo "[stop] Kokoro serve 已停" || true
pkill -f "speaker/speaker_server.py" 2>/dev/null && echo "[stop] 声纹门控已停" || echo "[stop] 声纹门控未在运行"
pkill -f "llama-server.*Qwen3-ASR" 2>/dev/null && echo "[stop] ASR 服务器已停" || echo "[stop] ASR 服务器未在运行"
