#!/usr/bin/env python3
"""stdin 一行文本 -> outdir 出 wav（协议与拆分前完全一致，voice-chat 仍用它）。

环境变量：KOKORO_OUTDIR（输出目录）、KOKORO_VOICE（音色，默认 zf_xiaobei）
就绪标记：[kokoro] Ready   —— voice_loop 靠轮询日志这一行判断可用
"""
import os, sys, time
import soundfile as sf
from engine import get_pipeline, synth

OUTDIR = os.environ.get("KOKORO_OUTDIR", "/tmp/kokoro_serve_out")
VOICE = os.environ.get("KOKORO_VOICE", "zf_xiaobei")
os.makedirs(OUTDIR, exist_ok=True)

print(f"[kokoro] loading pipeline (voice={VOICE}) ...", flush=True)
pipeline = get_pipeline()
print("[kokoro] Ready (legacy zh + espeak IPA for English)", flush=True)

n = 0
for line in sys.stdin:
    text = line.strip().replace("\n", " ")
    if not text:
        continue
    n += 1
    t0 = time.time()
    wav = synth(pipeline, text, VOICE)
    path = os.path.join(OUTDIR, f"out_{n:04d}.wav")
    sf.write(path, wav, 24000)
    dur = len(wav) / 24000
    el = time.time() - t0
    print(f"[kokoro] wrote {path} ({dur:.1f}s 音频, 合成用时 {el:.1f}s, RTF={el/max(dur,0.1):.2f})", flush=True)
