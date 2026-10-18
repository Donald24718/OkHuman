#!/usr/bin/env python3
"""Kokoro-82M 加载 + 中英混读 G2P（voice-chat 拆出来的独立引擎）。

原 voice-chat 的 kokoro_serve.py 里这段 G2P 包装是必需的：Kokoro v1.0 的中文管线
走 legacy_call，en_callable 不被调用，英文会原样透传导致静音；这里包一层，
中文路径与 legacy 完全一致，拉丁字母段用 espeak IPA 补上。
"""
import os, re
os.environ.setdefault("HF_ENDPOINT", "https://hf-mirror.com")

from phonemizer.backend import EspeakBackend
from phonemizer.separator import Separator
import cn2an, jieba
from misaki import zh as _zhmod

_en_backend = EspeakBackend("en-us")
_en_sep = Separator(word=" ")


def _en_phonemize(en_text):
    try:
        return _en_backend.phonemize([en_text], separator=_en_sep)[0]
    except Exception:
        return ""


class ZhG2PWithEnglish:
    """中文按 jieba 分词转 IPA；拉丁段走 espeak IPA。首字符决定起始语种。"""

    def __call__(self, text, en_callable=None):
        if not text.strip():
            return "", None
        text = cn2an.transform(text, "an2cn")
        text = _zhmod.ZHG2P.map_punctuation(text)
        is_zh = re.match(r"[\u4E00-\u9FFF]", text[0]) is not None
        parts = []
        for segment in re.findall(r"[\u4E00-\u9FFF]+|[^\u4E00-\u9FFF]+", text):
            if is_zh:
                parts.append(" ".join(_zhmod.ZHG2P.word2ipa(w) for w in jieba.lcut(segment, cut_all=False)))
            else:
                for en, other in re.findall(r"([A-Za-z '\-]*[A-Za-z][A-Za-z '\-]*)|([^A-Za-z]+)", segment):
                    if en:
                        parts.append(" " + _en_phonemize(en.strip()) + " ")
                    else:
                        parts.append(other)
            is_zh = not is_zh
        return "".join(parts).replace(chr(815), ""), None


def get_pipeline():
    """加载 Kokoro 并装上中英混读 G2P。返回已就绪的 KPipeline。"""
    from kokoro import KPipeline
    pipeline = KPipeline(lang_code="z")
    pipeline.g2p = ZhG2PWithEnglish()
    return pipeline


def synth(pipeline, text, voice):
    """整段合成 -> float32 numpy 波形（24 kHz）。"""
    import numpy as np
    chunks = [audio.numpy() for _gs, _ps, audio in pipeline(text, voice=voice)]
    return np.concatenate(chunks)
