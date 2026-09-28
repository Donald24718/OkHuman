#!/usr/bin/env python3
"""Kokoro-82M TTS serve：stdin 一行文本 -> outdir 出 wav（与 Qwen3-TTS serve 同协议）。"""
import os, sys, time
os.environ.setdefault("HF_ENDPOINT", "https://hf-mirror.com")
import numpy as np
import soundfile as sf
from kokoro import KPipeline

OUTDIR = os.environ.get("KOKORO_OUTDIR", "/tmp/kokoro_serve_out")
VOICE = os.environ.get("KOKORO_VOICE", "zf_xiaobei")
os.makedirs(OUTDIR, exist_ok=True)
# 英文段 callable：中文管线默认会把英文删掉（misaki ZHG2P），这里用 espeak IPA 补上
from phonemizer.backend import EspeakBackend
from phonemizer.separator import Separator
_en_backend = EspeakBackend("en-us")
_en_sep = Separator(word=" ")
def _en_callable(en_text: str) -> str:
    try:
        return _en_backend.phonemize([en_text], separator=_en_sep)[0]
    except Exception:
        return ""

# v1.0 中文管线走 legacy_call（en_callable 不被调用，英文原样透传→模型静音）。
# 包一层 G2P：中文路径与 legacy 完全一致，拉丁字母段走 _en_callable 转 IPA。
import re as _re, cn2an as _cn2an, jieba as _jieba
from misaki import zh as _zhmod

class _ZhG2PEn:
    def __call__(self, text, en_callable=None):
        if not text.strip():
            return "", None
        text = _cn2an.transform(text, "an2cn")
        text = _zhmod.ZHG2P.map_punctuation(text)
        is_zh = _re.match(r"[\u4E00-\u9FFF]", text[0]) is not None
        parts = []
        for segment in _re.findall(r"[\u4E00-\u9FFF]+|[^\u4E00-\u9FFF]+", text):
            if is_zh:
                parts.append(" ".join(_zhmod.ZHG2P.word2ipa(w) for w in _jieba.lcut(segment, cut_all=False)))
            else:
                for en, other in _re.findall(r"([A-Za-z '\-]*[A-Za-z][A-Za-z '\-]*)|([^A-Za-z]+)", segment):
                    if en:
                        parts.append(" " + _en_callable(en.strip()) + " ")
                    else:
                        parts.append(other)
            is_zh = not is_zh
        return "".join(parts).replace(chr(815), ""), None

print(f"[kokoro] loading pipeline (voice={VOICE}) ...", flush=True)
pipeline = KPipeline(lang_code="z")
pipeline.g2p = _ZhG2PEn()
print("[kokoro] Ready (legacy zh + espeak IPA for English)", flush=True)

n = 0
for line in sys.stdin:
    text = line.strip().replace("\n", " ")
    if not text:
        continue
    n += 1
    t0 = time.time()
    chunks = []
    for _gs, _ps, audio in pipeline(text, voice=VOICE):
        chunks.append(audio.numpy())
    wav = np.concatenate(chunks)
    path = os.path.join(OUTDIR, f"out_{n:04d}.wav")
    sf.write(path, wav, 24000)
    dur = len(wav)/24000
    print(f"[kokoro] wrote {path} ({dur:.1f}s 音频, 合成用时 {time.time()-t0:.1f}s, RTF={(time.time()-t0)/max(dur,0.1):.2f})", flush=True)
