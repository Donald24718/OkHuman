#!/usr/bin/env python3
"""Kokoro TTS 的 HTTP 包装（零依赖，只用标准库）。

注意：本文件不能叫 http.py——脚本所在目录在 sys.path 最前面，
同名会遮蔽标准库 http 包，`from http.server import ...` 直接炸。

  POST /tts          {"text": "...", "voice": "zf_xiaoyi"(可选), "format": "json|wav"}
      format=json（默认）-> {"path": "...", "seconds": 1.23, "ms": 456}
      format=wav         -> audio/wav 字节流（24 kHz 16bit）
  GET  /tts?text=...&format=wav   同上，方便 curl / 浏览器
  GET  /health       -> {"ok": true, "voice": "...", "ready": true}

单 pipeline 常驻，合成加锁串行（Kokoro 非线程安全；并发请求排队即可）。
"""
import json, os, sys, threading, time, uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs, unquote

import soundfile as sf
from engine import get_pipeline, synth

OUTDIR = os.environ.get("TTS_OUTDIR", "/tmp/tts_http_out")
VOICE = os.environ.get("TTS_VOICE", "zf_xiaoyi")
PORT = int(os.environ.get("TTS_PORT", "8092"))
HOST = os.environ.get("TTS_HOST", "127.0.0.1")
os.makedirs(OUTDIR, exist_ok=True)

LOCK = threading.Lock()
PIPE = None
READY = threading.Event()


def loader():
    global PIPE
    PIPE = get_pipeline()
    READY.set()
    print("[tts-http] Ready", flush=True)


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, body, ctype="application/json"):
        raw = body if isinstance(body, bytes) else json.dumps(body, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _synth(self, text, voice, want_wav):
        if not text.strip():
            return self._send(400, {"error": "text 为空"})
        t0 = time.time()
        with LOCK:                       # Kokoro 非线程安全：串行合成
            wav = synth(PIPE, text, voice or VOICE)
        el = int((time.time() - t0) * 1000)
        path = os.path.join(OUTDIR, f"tts_{uuid.uuid4().hex[:12]}.wav")
        sf.write(path, wav, 24000)
        if want_wav:
            with open(path, "rb") as f:
                return self._send(200, f.read(), "audio/wav")
        return self._send(200, {"path": path, "seconds": round(len(wav) / 24000, 3), "ms": el})

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/health":
            return self._send(200, {"ok": READY.is_set(), "voice": VOICE, "outdir": OUTDIR})
        if u.path == "/tts":
            q = parse_qs(u.query)
            text = unquote((q.get("text") or [""])[0])
            return self._synth(text, (q.get("voice") or [None])[0],
                               (q.get("format") or ["json"])[0] == "wav")
        return self._send(404, {"error": "用 POST /tts 或 GET /tts?text=..."})

    def do_POST(self):
        u = urlparse(self.path)
        if u.path != "/tts":
            return self._send(404, {"error": "只有 /tts"})
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n).decode("utf-8", "replace") if n else ""
        try:
            d = json.loads(raw) if raw.strip().startswith("{") else {"text": raw}
        except json.JSONDecodeError:
            d = {"text": raw}
        return self._synth(str(d.get("text", "")), d.get("voice"),
                           str(d.get("format", "json")) == "wav")


if __name__ == "__main__":
    threading.Thread(target=loader, daemon=True).start()
    print(f"[tts-http] listening http://{HOST}:{PORT}  (voice={VOICE}, outdir={OUTDIR})", flush=True)
    ThreadingHTTPServer((HOST, PORT), H).serve_forever()
