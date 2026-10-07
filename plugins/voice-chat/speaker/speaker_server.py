#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
speaker_server.py — 声纹门控服务 (CAM++, 3D-Speaker, CPU)

  POST /verify   body=wav 字节(任意采样率/声道, 服务端转 16k mono)
                 -> {"score": 0.83, "pass": true, "ms": 412}
  GET  /status   -> {"ready": true, "threshold": 0.6, "enrolled": "sample.wav", "n_enroll": 1}
  GET  /enroll?wav=/path/to/ref.wav   追加一段参考音频(可选, 默认用启动时 --enroll)

模型: iic/speech_campplus_sv_zh_en_16k-common_advanced (CAM++, 192-dim, 200k speakers)
用本项目 venv 跑: <项目根>/venv/bin/python  (torch cpu)
fbank: vendor 的纯 python kaldi_fbank (torchaudio C 扩展不可用, 已去依赖)
"""
import argparse
import io
import json
import os
import sys
import time
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import torch

HERE = os.path.dirname(os.path.abspath(__file__))
MODEL_PT = os.path.join(HERE, "models", "campplus_cn_en_common.pt")
SPKLAB = HERE
ENROLL_DIR = os.path.join(HERE, "enrollments")
sys.path.insert(0, HERE)
from kaldi_fbank import fbank as _kaldi_fbank  # noqa: E402  (vendor 自 torchaudio.compliance.kaldi, 纯 python)
SR = 16000
THRESHOLD = float(os.environ.get("VOICE_GATE_THRESHOLD", "0.60"))
PORT = int(os.environ.get("VOICE_GATE_PORT", "8091"))

sys.path.insert(0, SPKLAB)
from speakerlab.models.campplus.DTDNN import CAMPPlus  # noqa: E402

MODEL = CAMPPlus(feat_dim=80, embedding_size=192)
STATE = torch.load(MODEL_PT, map_location="cpu")
MODEL.load_state_dict(STATE)
MODEL.eval()
DEVICE = torch.device("cuda" if torch.cuda.is_available() else "cpu")
MODEL.to(DEVICE)

ENROLL_EMB = []          # list[numpy(192,)] 已注册的声纹
ENROLL_SRC = []          # 来源文件名

# ─────────────────────────── 特征 & 嵌入 ───────────────────────────

def load_wav_16k(data: bytes):
    """wav 字节 -> torch [1, T] @16k mono (float32)。"""
    with wave.open(io.BytesIO(data)) as w:
        nch, sw, fs = w.getnchannels(), w.getsampwidth(), w.getframerate()
        raw = w.readframes(w.getnframes())
    if sw == 2:
        a = np.frombuffer(raw, dtype=np.int16).astype(np.float32) / 32768.0
    elif sw == 1:
        a = (np.frombuffer(raw, dtype=np.uint8).astype(np.float32) - 128.0) / 128.0
    else:
        raise ValueError(f"不支持的位深 {sw*8}bit")
    if nch > 1:
        a = a.reshape(-1, nch).mean(axis=1)
    wav = torch.from_numpy(a).unsqueeze(0)
    if fs != SR:
        from scipy.signal import resample_poly
        wav = torch.from_numpy(
            resample_poly(a, SR, fs, axis=0).astype(np.float32)).unsqueeze(0)
    return wav


def fbank80(wav: torch.Tensor):
    # 与 3D-Speaker 官方 FBank(80, 16000, mean_nor=True) 完全一致
    feat = _kaldi_fbank(wav, num_mel_bins=80, sample_frequency=SR, dither=0)
    feat = feat - feat.mean(0, keepdim=True)
    return feat


@torch.no_grad()
def embed(wav: torch.Tensor) -> np.ndarray:
    feat = fbank80(wav).unsqueeze(0).to(DEVICE)
    return MODEL(feat).detach().squeeze(0).cpu().numpy()


def cosine(a: np.ndarray, b: np.ndarray) -> float:
    return float(np.dot(a, b) / (np.linalg.norm(a) * np.linalg.norm(b) + 1e-8))


def verify(data: bytes) -> dict:
    t0 = time.time()
    wav = load_wav_16k(data)
    if wav.shape[1] < SR * 0.8:          # 不到 0.8s 的片段没法判断
        return {"score": -1.0, "pass": False, "reason": "too_short",
                "ms": int((time.time() - t0) * 1000)}
    emb = embed(wav)
    scores = [cosine(emb, e) for e in ENROLL_EMB]
    best = max(scores)
    return {"score": round(best, 4), "pass": best >= THRESHOLD,
            "ms": int((time.time() - t0) * 1000)}


def enroll_wav(path: str):
    import subprocess
    pcm = subprocess.run(
        ["ffmpeg", "-loglevel", "error", "-i", path,
         "-f", "s16le", "-ac", "1", "-ar", str(SR), "-"],
        capture_output=True, check=True).stdout
    wav = torch.from_numpy(np.frombuffer(pcm, dtype=np.int16).astype(np.float32)
                           / 32768.0).unsqueeze(0)
    emb = embed(wav)
    ENROLL_EMB.append(emb)
    ENROLL_SRC.append(os.path.basename(path))
    os.makedirs(ENROLL_DIR, exist_ok=True)
    np.save(os.path.join(ENROLL_DIR,
                         os.path.basename(path).rsplit(".", 1)[0] + ".npy"), emb)
    return emb


# ─────────────────────────── HTTP ───────────────────────────

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _json(self, code, obj):
        b = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path.startswith("/status"):
            self._json(200, {"ready": True, "threshold": THRESHOLD,
                             "enrolled": ENROLL_SRC, "device": str(DEVICE)})
        elif self.path.startswith("/enroll"):
            from urllib.parse import urlparse, parse_qs
            # 请求行被 BaseHTTPRequestHandler 按 iso-8859-1 解码过：还原 UTF-8，
            # 否则中文路径（如 /home/$USER/语音样本.wav）在 ffmpeg 侧变成乱码找不到文件
            try:
                path = self.path.encode("latin-1").decode("utf-8")
            except (UnicodeEncodeError, UnicodeDecodeError):
                path = self.path
            q = parse_qs(urlparse(path).query, encoding="utf-8")
            p = (q.get("wav") or [None])[0]
            if not p:
                self._json(400, {"error": "need ?wav=/path"}); return
            try:
                enroll_wav(p)
                self._json(200, {"ok": True, "enrolled": ENROLL_SRC})
            except Exception as e:
                self._json(500, {"error": str(e)})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        if self.path.rstrip("/") != "/verify":
            self._json(404, {"error": "not found"}); return
        n = int(self.headers.get("Content-Length", 0))
        data = self.rfile.read(n)
        try:
            self._json(200, verify(data))
        except Exception as e:
            self._json(500, {"score": -1.0, "pass": False, "error": str(e)[:200]})


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--enroll", action="append", default=[],
                    help="参考音频路径(可多次)。不给则用 enrollments/ 里的 .npy")
    args = ap.parse_args()

    os.makedirs(ENROLL_DIR, exist_ok=True)
    for f in sorted(os.listdir(ENROLL_DIR)):
        if f.endswith(".npy"):
            ENROLL_EMB.append(np.load(os.path.join(ENROLL_DIR, f)))
            ENROLL_SRC.append(f.rsplit(".", 1)[0])
    for p in args.enroll:
        if not any(p.split("/")[-1].rsplit(".", 1)[0] == s for s in ENROLL_SRC):
            enroll_wav(p)

    print(f"[gate] device={DEVICE} threshold={THRESHOLD} "
          f"enrolled={ENROLL_SRC} port={PORT}", flush=True)
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), H)
    srv.serve_forever()


if __name__ == "__main__":
    main()
