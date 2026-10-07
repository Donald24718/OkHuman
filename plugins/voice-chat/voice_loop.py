#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
voice_loop.py — 语音闭环 (OkHuman 插件 voice-chat)

  [麦克风] --16k mono--> [VAD 端点检测]
        --> [声纹]  speaker_server (CAM++) :8091（只认你的声音）
        --> [ASR]   llama-server (CPU build) + Qwen3-ASR-0.6B :8090
        --> [唤醒词] 多唤醒词路由（小K→8451 / 副本实例→8452；或唤醒后 15s 窗内下一句）
        --> [Agent]  OkHuman POST /enqueue 按唤醒词路由（解耦：只走 HTTP）
        --> [音频源] 各实例 /events SSE 实时流（全渠道）+ /history 轮询兜底
        --> [播放]  消息按条排队（跨实例 FIFO）+ 消息内句子级流式：Kokoro-82M (stdin 管道) → aplay
      广播开关: broadcast_off 文件存在 → 只播语音发起的；否则播报所有渠道（QQ/WebUI/语音）的消息

Ctrl-C 退出。可调项见下方 CONFIG 区 + 本目录 config.json（config 优先）。
"""

import io
import json
import math
import os
import queue
import re
import signal
import subprocess
import sys
import threading
import time
import wave
from array import array

import requests
import urllib.request

# ══════════════════════════════ CONFIG ══════════════════════════════
_HERE      = os.path.dirname(os.path.abspath(__file__))
try:
    with open(os.path.join(_HERE, "config.json"), encoding="utf-8") as _f:
        CFG = json.load(_f)
except (FileNotFoundError, json.JSONDecodeError):
    CFG = {}

ASR_URL      = "http://127.0.0.1:8090/v1/audio/transcriptions"   # build-cpu llama-server
GATE_URL     = "http://127.0.0.1:8091/verify"                    # 声纹门控 (CAM++)
OKHUMAN_URL  = CFG.get("okhuman_url", "http://127.0.0.1:8451")   # OkHuman 实例
MIC          = CFG.get("mic", "default")      # 麦克风 (ffmpeg alsa 设备名: default / hw:3,0 ...)
SPEAKER      = CFG.get("speaker", "0,0")      # 播放设备 (aplay -D)
MAX_REPLY_CHARS = int(CFG.get("max_reply_chars", 500))
# 路由实例：每个实例绑一个唤醒词（= 它的"名字"，播报前缀 "小K说：" 由此生成）。
# 喊哪个唤醒词 → 这句话只发给对应实例（小K→8451 / 副本实例→8452）。
INSTANCES = [(it["wake_word"], it["url"]) for it in CFG.get("instances", [])]
if not INSTANCES:                        # 未配置 instances → 单实例直连（旧行为）
    INSTANCES = [("agent", OKHUMAN_URL)]
WAKE_ROUTES = dict(INSTANCES)            # 唤醒词 -> 实例 URL（路由表）
WAKE_WORDS = list(WAKE_ROUTES) if len(INSTANCES) >= 2 else []  # 多实例才开唤醒门控
WAKE_ARM_S = 15.0                        # 纯唤醒词后保持 15 秒收听窗（窗内下一句发给被唤醒的实例）
PAUSE_FILE  = os.path.join(_HERE, "data", "paused")  # 存在 = 暂停（WebUI 按钮控制）
BROADCAST_FILE = os.path.join(_HERE, "data", "broadcast_off")  # 存在 = 全量播报关（默认开）
TTS_OUTDIR  = "/tmp/voice_loop_tts"
TTS_LOG     = "/tmp/voice_loop_tts.log"

SR          = 16000              # 采样率（ASR 要求 16k）
FRAME_MS    = 50                 # VAD 帧长
FRAME       = SR * FRAME_MS // 1000
MIN_SPEECH_S   = 0.4             # 实际发声短于此视为误触，丢弃
MAX_UTT_S      = 30.0            # 单句上限
END_SILENCE_S = 0.7              # 连续静音这么久 => 说完
SPEECH_START_FRAMES = 2          # 连续 N 帧超阈值 => 开始说话
# 阈值单位是 s16 幅度 rms（静音≈0，正常说话 1000~8000，键盘环境噪声 100~300）
NOISE_HIST_S   = 4.0             # 底噪统计窗口（秒）
NOISE_FLOOR_INIT = 300           # 窗口样本不足时的初始底噪
NOISE_FLOOR_GAIN = 4.0           # 阈值 = max(ABS_FLOOR, P10(窗口) * gain)
ABS_FLOOR      = 250             # 阈值绝对下限（要高于环境底噪）
NOISE_CAND_GATE = 2.5            # 帧能量低于 底噪*gate 才计入底噪统计（防语音污染）
# ═══════════════════════════════════════════════════════════════════

LOG_F = open("/tmp/voice_loop.log", "a", buffering=1)

def log(*a):
    print(*a, file=LOG_F, flush=True)
    print(*a, flush=True)


# ─────────────────────────── TTS (Kokoro, 模型常驻) ───────────────────────────
class KokoroTTSServer:
    """Kokoro-82M 后端：单 serve 单默认女声（晓北），全实例共享，CPU RTF≈0.35。"""
    READY_MARK = "[kokoro] Ready"

    def __init__(self, name="kokoro", outdir=None,
                 log_path="/tmp/voice_loop_tts_kokoro.log"):
        self.name = name
        if outdir is None:
            outdir = os.path.join(TTS_OUTDIR, "kokoro")
        self.outdir = outdir
        self.log_path = log_path
        self.proc = None
        os.makedirs(outdir, exist_ok=True)
        base = os.path.dirname(os.path.abspath(__file__))
        self.py = os.path.join(base, "kokoro_venv", "bin", "python")
        self.serve = os.path.join(base, "kokoro_serve.py")

    def alive(self):
        return self.proc is not None and self.proc.poll() is None

    def start(self):
        log(f"[tts:{self.name}] starting kokoro serve (model loading ~5s) ...")
        env = dict(os.environ, HF_ENDPOINT="https://hf-mirror.com",
                   KOKORO_OUTDIR=self.outdir, KOKORO_VOICE=CFG.get("kokoro_voice", "zf_xiaobei"))
        self.proc = subprocess.Popen(
            [self.py, self.serve],
            stdin=subprocess.PIPE,
            stdout=open(self.log_path, "a"),
            stderr=subprocess.STDOUT, env=env,
        )

    def wait_ready(self, timeout_s=120) -> bool:
        t0 = time.time()
        while time.time() - t0 < timeout_s:
            if not self.alive():
                return False
            try:
                with open(self.log_path, encoding="utf-8", errors="replace") as f:
                    tail = f.readlines()[-20:]
            except OSError:
                tail = []
            if any(self.READY_MARK in ln for ln in tail):
                return True
            time.sleep(0.5)
        return False

    def synth(self, text, timeout_s=240):
        """写一行文本，等 serve 产出新 wav，返回路径。

        serve 重启会从头编号（out_0001 覆盖写），所以不能用"新文件名"
        判断；用 (mtime, size) 快照对比：请求前后状态变化的文件即目标。
        """
        if not self.alive():
            raise RuntimeError("tts server dead")
        before = self._snapshot()
        self.proc.stdin.write((text.strip().replace("\n", " ") + "\n").encode("utf-8"))
        self.proc.stdin.flush()
        t0 = time.time()
        while time.time() - t0 < timeout_s:
            if not self.alive():
                raise RuntimeError("tts server died during synth")
            now = self._snapshot()
            changed = [(name, st) for name, st in now.items()
                       if before.get(name) != st]
            cand = max(changed, key=lambda kv: kv[1][0])[0] if changed else None
            if cand is not None:
                path = os.path.join(self.outdir, cand)
                # 等文件写稳定（2 次采样大小不变）
                last = -1
                for _ in range(50):
                    try:
                        sz = os.path.getsize(path)
                    except FileNotFoundError:
                        time.sleep(0.2); continue
                    if sz == last and sz > 44:
                        return path
                    last = sz
                    time.sleep(0.15)
            time.sleep(0.1)
        raise TimeoutError("tts timeout")

    def _snapshot(self):
        """{wav文件名: (mtime, size)}"""
        out = {}
        try:
            for f in os.listdir(self.outdir):
                if not f.endswith(".wav"):
                    continue
                st = os.stat(os.path.join(self.outdir, f))
                out[f] = (st.st_mtime, st.st_size)
        except FileNotFoundError:
            pass
        return out


# ─────────────────────────── 麦克风 + VAD ───────────────────────────
def rms(frame: bytes) -> float:
    a = array("h")
    a.frombytes(frame[: len(frame) - (len(frame) % 2)])
    if not a:
        return 0.0
    return math.sqrt(sum(x * x for x in a) / len(a))


def vad_read(stream, abort_evt=None, freeze_evt=None):
    """从 16k mono s16le 流里读出一句语音。返回 (pcm_bytes, dur_s) 或 (None, None)。

    stream: 支持 read(n) 的文件对象。读到 EOF 且没检测到语音则返回 (None, None)。
    abort_evt: 置位即中止（喇叭开播时麦克风立刻停录，防回声混入）。
    """
    pre = []            # 触发前的 pre-roll（保留 300ms）
    pre_max = int(300 / FRAME_MS)
    noise_hist = []     # 未说话时最近 NOISE_HIST_S 秒的帧能量
    hist_max = max(4, int(NOISE_HIST_S * 1000 / FRAME_MS))
    noise_floor = NOISE_FLOOR_INIT
    speech_buf = b""
    started = False
    hot_frames = 0      # 连续超阈值帧数
    silent_frames = 0
    voiced_frames = 0   # 说话期间的发声帧（判断误触用）
    t_start = time.time()
    while time.time() - t_start < MAX_UTT_S:
        if abort_evt is not None and abort_evt.is_set():
            return None, None
        chunk = stream.read(FRAME * 2)
        if not chunk:
            break
        e = rms(chunk)
        if not started:
            # 只在空闲时统计底噪（P10 对偶发咳嗽/键盘更鲁棒）。
            # 带门限：只有"像噪声"的帧才入窗，防止流一开头就是语音时
            # 把语音能量当成底噪、导致永远触发不了。
            s = sorted(noise_hist)
            est = s[max(0, len(s) // 10 - 1)] if s else NOISE_FLOOR_INIT
            gate = NOISE_CAND_GATE * max(ABS_FLOOR, est)
            contaminated = freeze_evt is not None and freeze_evt.is_set()
            if contaminated:
                # 喇叭在播：漏声不可信，只读流不更新任何 VAD 状态
                continue
            if e < gate:
                noise_hist.append(e)
                if len(noise_hist) > hist_max:
                    noise_hist.pop(0)
                s2 = sorted(noise_hist)
                est = s2[max(0, len(s2) // 10 - 1)]
            noise_floor = est
            thr = max(ABS_FLOOR, noise_floor * NOISE_FLOOR_GAIN)
            if e > thr:
                hot_frames += 1
            else:
                hot_frames = 0
            if hot_frames >= SPEECH_START_FRAMES:
                started = True
                voiced_frames = 1
                speech_buf = b"".join(pre) + chunk
                log(f"[mic] 检测到语音 (rms={e:.1f}, thr={thr:.1f})")
                continue
            pre.append(chunk)
            if len(pre) > pre_max:
                pre.pop(0)
        else:
            # 说话中：冻结阈值（用触发前估计的底噪），只做端点检测
            speech_buf += chunk
            if e < thr:
                silent_frames += 1
            else:
                silent_frames = 0
                voiced_frames += 1
            if silent_frames * FRAME_MS / 1000 >= END_SILENCE_S:
                break
    if not started:
        return None, None
    # 误触判定用"实际发声时长"（不含 pre-roll 和尾部静音填充）
    if voiced_frames * FRAME_MS / 1000 < MIN_SPEECH_S:
        return None, None
    return speech_buf, len(speech_buf) // 2 / SR


def record_utterance():
    """阻塞直到检测到一句语音（真麦克风）。返回 (wav_bytes, duration_s)。"""
    log("[mic] 听 ...")
    cmd = ["ffmpeg", "-loglevel", "error", "-f", "alsa", "-i", MIC,
           "-f", "s16le", "-ac", "1", "-ar", str(SR), "-"]
    p = subprocess.Popen(cmd, stdout=subprocess.PIPE,
                         stderr=subprocess.DEVNULL)
    try:
        pcm, dur = vad_read(p.stdout, freeze_evt=PLAYING)
    finally:
        p.stdout.close()
        p.wait()
    if pcm is None:
        return None, None
    return build_wav(pcm), dur


def build_wav(pcm16: bytes) -> bytes:
    import io
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(pcm16)
    return buf.getvalue()


# 喇叭正在播放（仅记录输出状态；输入链路不再受其约束——播报回声由声纹门控滤除）
PLAYING = threading.Event()


# ─────────────────────────── 声纹门控 ───────────────────────────
def voice_gate(wav_bytes: bytes) -> tuple:
    """不是我的声音 -> (False, score)。服务挂了 -> (True, 1.0) 放行(降级, 别把门焊死)。"""
    try:
        r = requests.post(GATE_URL, data=wav_bytes, timeout=30)
        r.raise_for_status()
        j = r.json()
        return bool(j.get("pass")), float(j.get("score", -1.0))
    except Exception as e:
        log(f"[gate] 服务异常, 本次放行: {e}")
        return True, 1.0


# ─────────────────────────── ASR ───────────────────────────
def duration_hint(wav_bytes: bytes) -> float:
    """从 wav 字节估算时长（16k mono s16le，含 44 字节头）。"""
    return max(0, (len(wav_bytes) - 44) / 2 / 16000)

def transcribe(wav_bytes: bytes, duration_s: float) -> str:
    t0 = time.time()
    r = requests.post(
        ASR_URL,
        files={"file": ("utterance.wav", wav_bytes, "audio/wav")},
        data={"language": "Chinese"},
        timeout=120,
    )
    r.raise_for_status()
    text = r.json().get("text", "")
    m = re.search(r"<asr_text>(.*)$", text, re.S)
    if m:
        text = m.group(1)
    text = text.strip()
    log(f"[asr] {duration_s:.1f}s 音频 -> {text or '(空)'}  ({time.time() - t0:.1f}s)")
    return text


# ─────────────────────────── OkHuman chat（解耦：只走 HTTP）───────────────────────────
def check_wake(text: str, routes: dict):
    """多唤醒词模糊匹配：唤醒词字符之间允许任意空白/标点（ASR 拆词也认）。

    返回 (命中的唤醒词或 None, 唤醒词之后的内容, 目标实例 URL)。
    routes 为空 = 关闭唤醒门控。长词优先，防前缀互吃。
    """
    if not routes:
        return None, text, None
    gap = r"[\s，,。.！!？?、~·:：;；()（）\"'“”‘’-]*"
    for word in sorted(routes, key=len, reverse=True):
        pat = gap.join(re.escape(c) for c in word)
        m = re.search(pat, text, re.IGNORECASE)
        if m:
            return word, re.sub(f"^{gap}", "", text[m.end():]).strip(), routes[word]
    return None, text, None


def beep(freq=880, dur=0.25, n=2):
    """短促提示音（唤醒成功 → 请说内容）。"""
    import numpy as np
    t = np.linspace(0, dur * n, int(SR * dur * n), endpoint=False)
    sig = (0.3 * np.sin(2 * np.pi * freq * t) * 32767).astype(np.int16)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w2:
        w2.setnchannels(1)
        w2.setsampwidth(2)
        w2.setframerate(SR)
        w2.writeframes(sig.tobytes())
    p = subprocess.Popen(["aplay", "-q", "-D", SPEAKER], stdin=subprocess.PIPE)
    p.stdin.write(buf.getvalue())
    p.stdin.close()
    p.wait()


def clean_for_speech(text: str) -> str:
    """去掉 TTS 读不出来的东西：代码块、markdown 标记、headline 行。"""
    text = re.sub(r"```.*?```", "", text, flags=re.S)
    text = re.sub(r"`([^`]*)`", r"\1", text)
    text = re.sub(r"^\s{0,3}#{1,6}\s*", "", text, flags=re.M)
    text = re.sub(r"\*{1,2}([^*]+)\*{1,2}", r"\1", text)
    text = re.sub(r"^\[[^\]]*\]\(.*\)$", "", text, flags=re.M)
    # headline 行（⟦ ... ⟧）不读
    lines = [ln for ln in text.splitlines() if not (ln.strip().startswith("⟦"))]
    text = "\n".join(lines)
    # 去表情（TTS 可能念出来）
    text = re.sub(
        "[\U0001F000-\U0001FAFF\u2600-\u27BF\u2B00-\u2BFF\uFE0F]", "", text)
    text = re.sub(r"\s{2,}", " ", text).strip()
    return text


# ─────────────────────────── 播放 ───────────────────────────
# 每段 TTS 最大字数：长段合成音质会崩坏，切短段串行播放（段间预取不空转）
SENT_END = re.compile(r"[。！？!?；;\n]")    # 句末标点：正文一到完整句就立刻送 TTS（句子级流式）
SEGMENT_CHARS = 50


def split_speech(text: str, limit: int = SEGMENT_CHARS):
    """三级切分，保证 TTS 断句落在自然停顿上：
    1) 强句界（。！？…；）必切；
    2) 过短段（<8 字）并入前段，避免碎段；
    3) 超过 limit 的长句，优先在 limit 窗口内的次级句界（，、：,——）处切，
       窗口内没有标点才在 limit 处硬切。返回非空段列表。"""
    text = (text or "").strip()
    if not text:
        return []
    parts = [p for p in re.split(r"(?<=[。！？!?…；;])", text) if p.strip()]
    merged = []
    for p in parts:
        p = p.strip()
        if merged and len(merged[-1]) < 8:
            merged[-1] += p
        else:
            merged.append(p)
    segs = []
    for p in merged:
        while len(p) > limit:
            window = p[:limit]
            cut = max((window.rfind(c) for c in "，、，,：:；;—"), default=-1)
            if cut < 8:
                cut = limit
            segs.append(p[:cut].strip())
            p = p[cut:].lstrip("，、，,：:；;—").strip()
        if p:
            segs.append(p)
    return segs


# 常驻 aplay：设备只开一次，段与段之间无启动间隙（原每段起进程≈100-200ms 停顿）
# 2026-09-16 播放架构复盘（两轮实测）：
#  ① 「常驻 aplay+stdin 管道」：aplay 播完一段 wav 就从 stdin 退出（exit 0，"常驻"是假象），
#     下一段靠 _ensure_play_proc 探测死亡后重起。竞态窗口（上一段 aplay 还在排空尾部缓冲时
#     写下一段）会 BrokenPipe（老日志 318 段 75 次），靠 kill+重起+重写兜底，偶发段首 ~100ms
#     重放——听感可接受，段间无缝、无重叠。
#  ② 「每段一次性 aplay」：无管道无竞态，但 aplay 退出时 PipeWire 缓冲里还有尾部音频在播
#     （~100-200ms），下一段立即开播 → 每段边界都有重叠（用户听感=回音、几段一起播）。
# 结论：恢复常驻 stdin 单流（①）。单条流连续喂帧，段间零间隙零重叠；BrokenPipe 只是日志噪音。
PLAY_PROC = None
def _ensure_play_proc():
    global PLAY_PROC
    if PLAY_PROC is None or PLAY_PROC.poll() is not None:
        PLAY_PROC = subprocess.Popen(["aplay", "-q", "-D", SPEAKER], stdin=subprocess.PIPE)
    return PLAY_PROC

def play(wav_path: str):
    """把 wav 写入常驻 aplay 的 stdin（阻塞到播完为止）。单条连续音频流：段间无缝、无重叠。"""
    global PLAY_PROC
    log(f"[play] {wav_path}")
    PLAYING.set()
    log("[play] 开播")
    try:
        with open(wav_path, "rb") as f:
            data = f.read()
        if PLAY_PROC is not None and PLAY_PROC.poll() is not None:
            log("[play] 常驻 aplay 已死, 重启后播放")
            try:
                PLAY_PROC.kill()
            except OSError:
                pass
            PLAY_PROC = None
        p = _ensure_play_proc()
        try:
            p.stdin.write(data)
            p.stdin.flush()
        except (BrokenPipeError, OSError) as e:
            log(f"[play] aplay 异常重启 ({type(e).__name__})")
            if PLAY_PROC is not None:
                try:
                    PLAY_PROC.kill()
                except OSError:
                    pass
            PLAY_PROC = None
            p = _ensure_play_proc()
            p.stdin.write(data)
            p.stdin.flush()
    except Exception as e:
        log(f"[play] 播放失败: {type(e).__name__}: {e}")
    finally:
        PLAYING.clear()
        log("[play] 播完")


# ─────────────────────── 全量播报（任意渠道回复） ───────────────────────
# 服务开着且 WebUI 播报开关为开时，任何 agent 回复（QQ / WebUI / 语音唤醒）都 TTS 朗读：
# 轮询各实例 GET /history，新出现的回复正文（含中间叙述）入播报队列；语音管道已播的由
# mark_broadcast() 打标，轮询器跳过，防双播。
# 2026-09-16: 轮询器从「只播最终回复」放宽为「播全部回复正文（含带 tool_calls 的中间叙述）」——
# 后台任务/系统通知切出的后续 run 的中间叙述原来谁都不播（用户感知=播报停了）。
_BC_SEEN = []        # [(ts, 归一化文本)]，FIFO
_BC_TTL = 1800.0     # 30 分钟内的去重窗口

def _bc_norm(t):
    return re.sub(r"\s+", "", (t or "").strip())

def mark_broadcast(text) -> bool:
    """打标已读。新打标返回 True（调用方可入队播报）；已见过返回 False（跳过，防双播）。
    2026-09-16: 原无返回值，语音管道「标记+无条件入队」与轮询器「检查+入队」竞态时同一条
    两边都入队 → 同一句话播两遍（用户听感=回音/几段一起播）。"""
    n = _bc_norm(text)
    if not n:
        return False
    now = time.time()
    while _BC_SEEN and now - _BC_SEEN[0][0] > _BC_TTL:
        _BC_SEEN.pop(0)
    if any(x == n for _ts, x in _BC_SEEN):
        return False
    _BC_SEEN.append((now, n))
    if len(_BC_SEEN) > 300:
        _BC_SEEN.pop(0)
    return True


# ─────────────────────── 按条广播队列（2026-09-21 架构，用户指定语义）───────────────────────
#   * 队列粒度 = 一条 assistant 消息（MsgItem）；小K/副本实例 的消息按产出顺序进同一个全局 FIFO，
#     一条播完取下一条（不区分实例）——谁先产出谁先播。
#   * 消息内：agent 流式吐出一个完整句（句末标点）就送 TTS 开播，不等整条/整轮结束。
#   * 音频来源：各实例 /events SSE 实时流（全渠道：语音唤醒 / QQ / WebUI / 定时任务）。
#     /history 在轮次运行期间隐藏当前轮条目（WebUI 防重复渲染设计）→ /history 轮询只做兜底：
#     SSE 断线漏掉的整条消息，等该轮结束后由 poller 补播；去重靠全文打标（mark_broadcast）
#     + /events 重连重放按 (session, run_index) 去重。

class MsgItem:
    """一个播报队列项 = 一条 agent 消息；句子流式入队，None 表示该条结束。"""
    def __init__(self, who, run_key=None):
        self.who = who
        self.run_key = run_key      # (url, session, run_index)——轮次死活判定用
        self.q = queue.Queue()
        self.sents = []             # 已入队句子全文（前缀拼接/日志）
        self.chars = 0              # 累计字符数（MAX_REPLY_CHARS 截断）


class RunState:
    """单实例单轮次的消息构建状态（SSE 断线重连后靠 run_key 去重）。"""
    def __init__(self, who, voice, run_key):
        self.who = who
        self.voice = voice          # 语音唤醒发起的轮次（全播报关闭时只有它播）
        self.run_key = run_key
        self.buf = ""
        self.item = None            # MsgItem 或 None（未建 / 被门控弃）
        self.sents = []             # 已接受句子（打标用，截断后继续累积）
        self.suppressed = False
        self.truncated = False
        self.done = False


RUN_LIVE = {}   # run_key -> run_start 时间戳（轮次存活表：消息队列饿死时判轮次死活，防播报员卡死）


def _prune(d, ttl=1800.0, cap=400):
    now = time.time()
    for k in [k for k, ts in d.items() if now - ts > ttl]:
        d.pop(k, None)
    while len(d) > cap:
        d.pop(next(iter(d)), None)



# ─────────────────────────── main ───────────────────────────

def main():
    # TTS: Kokoro-82M（stdin 管道协议，全实例共享；播报员按条串行，同时只有一个在合成）
    shared = KokoroTTSServer()
    shared.start()
    if not shared.wait_ready(timeout_s=120):
        log(f"[tts:kokoro] ⚠ 120 秒内未就绪，继续运行（首次合成可能失败重试）")
    log("=" * 60)
    log(f"voice loop 就绪  路由={INSTANCES}  唤醒词={WAKE_WORDS or '(关闭, 直达)'}  截断={MAX_REPLY_CHARS}字")
    log("说话即可（自动断句）：回复按条排队（跨实例 FIFO）、消息内句子级流式播报。Ctrl-C 退出")
    log("=" * 60)

    stop_evt = threading.Event()
    in_q = {url: queue.Queue() for _, url in INSTANCES}    # 每实例一条输入队列
    out_q = queue.Queue()                                 # 按条播报队列（产出序，跨实例 FIFO）
    voice_tag = {}                                        # url -> ts：语音唤醒入队时刻（判定轮次是否语音发起）
    url_word = {u: w for w, u in INSTANCES}

    # ── 麦克风工人: 听 → 声纹 → 暂停检查 → ASR → 唤醒词 → 入队 ──
    def mic_worker():
        armed = None    # (截止时刻, 唤醒词, 实例 URL)
        while not stop_evt.is_set():
            try:
                wav, dur = record_utterance()
                if wav is None:
                    continue
                ok, score = voice_gate(wav)
                if not ok:
                    log(f"[gate] 非本人声音, 丢弃 (score={score:.3f} < 阈值)")
                    continue
                log(f"[gate] 本人声音 (score={score:.3f})")
                if os.path.exists(PAUSE_FILE):
                    log("[pause] 暂停中, 丢弃")
                    continue
                user_text = transcribe(wav, dur)
                if not user_text:
                    log("[loop] ASR 为空，跳过")
                    continue
                log(f"[user] {user_text}")
                if not WAKE_WORDS:
                    in_q[INSTANCES[0][1]].put((None, user_text))
                    continue
                if armed is not None and time.time() < armed[0]:
                    # 唤醒后 15s 窗内: 我的任何语句发给被唤醒的实例
                    in_q[armed[2]].put((armed[1], user_text))
                    armed = None
                    continue
                if armed is not None:
                    log("[wake] 收听窗超时, 复位")
                    armed = None
                word, content, target_url = check_wake(user_text, WAKE_ROUTES)
                if word is None:
                    log(f"[wake] 未含唤醒词, 丢弃: {user_text}")
                    continue
                if len(content) <= 2:
                    # 纯唤醒词 → 响提示音, 对该实例开 15s 窗
                    armed = (time.time() + WAKE_ARM_S, word, target_url)
                    beep()
                    log(f"[wake] 「{word}」已唤醒, {WAKE_ARM_S:.0f} 秒内请说内容")
                    continue
                in_q[target_url].put((word, content))
            except Exception as e:
                log(f"[mic-err] {type(e).__name__}: {e}")
                time.sleep(1)

    # ── 发送工人: 语音唤醒话 → POST /enqueue（轮次随即开始；回复音频统一走 /events 监听）──
    def _enqueue(url, cmd):
        try:
            r = requests.post(url.rstrip("/") + "/enqueue", json={"message": cmd}, timeout=15)
            r.raise_for_status()
            j = r.json()
            log(f"[route] enqueue ok（position={j.get('position')}, running={j.get('running')}）")
        except Exception as e:
            log(f"[agent] 发送失败: {type(e).__name__}: {e}（这句丢弃）")

    def agent_worker(word, url):
        q = in_q[url]
        while not stop_evt.is_set():
            try:
                w, cmd = q.get(timeout=1)
            except queue.Empty:
                continue
            try:
                log(f"[route] ({w or 'agent'}) -> {url}")
                voice_tag[url] = time.time()
                threading.Thread(target=_enqueue, args=(url, cmd), daemon=True).start()
            except Exception as e:
                log(f"[agent] ({w or 'agent'}) 失败: {type(e).__name__}: {e}")

    # ── /events 监听: 每实例实时按条构建消息（全渠道音频源: 语音/QQ/WebUI/定时任务）──
    def events_listener(word, url):
        seen_runs = {}            # run_key -> ts：重连重放去重
        st = {"current": None}    # (run_key, RunState)

        def accept(rs, s):
            """接受一句: 门控/建队列项 → 入句子队列; 超长截断。sents 始终累积(打标用)。"""
            if rs.done or rs.suppressed:
                return
            if not rs.truncated:
                if rs.item is None:
                    if os.path.exists(BROADCAST_FILE) and not rs.voice:
                        log(f"[ev:{word}] 全播报关且非语音发起 → 弃这条消息")
                        rs.suppressed = True
                    else:
                        rs.item = MsgItem(rs.who, rs.run_key)
                        out_q.put(rs.item)
                if rs.item is not None:
                    rs.item.q.put(s)
                    rs.item.sents.append(s)
                    rs.item.chars += len(s)
            rs.sents.append(s)
            if rs.item is not None and rs.item.chars >= MAX_REPLY_CHARS:
                rs.truncated = True
                log(f"[ev:{word}] 消息达 {MAX_REPLY_CHARS} 字，后续句子弃")

        def finish(rs):
            """消息边界(tool_call/stopped/done): 冲刷尾句、全文打标、发结束信号。"""
            if rs.done:
                return
            tail = rs.buf.strip()
            rs.buf = ""
            if rs.item is not None and tail and not rs.truncated:
                accept(rs, tail)
            rs.done = True
            if rs.item is not None:
                mark_broadcast("".join(rs.sents))
                rs.item.q.put(None)

        def handle(e, ev):
            t = e.get("type")
            if t == "run_start":
                key = (url, e.get("session"), e.get("index"))
                now = time.time()
                _prune(seen_runs)
                if key in seen_runs:
                    rs = RunState(word, False, key)
                    rs.suppressed = True
                    log(f"[ev:{word}] 重放重复轮次 {key} → 抑制（轮询兜底）")
                else:
                    seen_runs[key] = now
                    v = (now - voice_tag.get(url, 0)) < 2.0
                    rs = RunState(word, v, key)
                st["current"] = (key, rs)
                RUN_LIVE[key] = now
            elif t == "delta" and st["current"] is not None:
                key, rs = st["current"]
                if rs.done:
                    # 同一 run 内新消息（上一条在 tool_call/stopped/done 结束）：换新状态
                    # （run_key 不变）。旧实现 st["current"] 一直指向已 done 的死状态，
                    # 第一个 tool_call 之后的所有正文被丢弃，囤到 run 结束由兜底补播
                    # （用户听感=中间到结尾全部 run 完才播，2026-09-21 修复）
                    rs = RunState(word, rs.voice, key)
                    st["current"] = (key, rs)
                text = e.get("text") or ""
                if not text:
                    return
                rs.buf += text
                while not (rs.done or rs.suppressed or rs.truncated):
                    m = SENT_END.search(rs.buf)
                    if not m:
                        break
                    head, rs.buf = rs.buf[:m.end()], rs.buf[m.end():]
                    s = head.strip()
                    if s:
                        accept(rs, s)
            elif t in ("tool_call", "stopped", "done") and st["current"] is not None:
                finish(st["current"][1])
            elif t == "run_end" and st["current"] is not None:
                key, rs = st["current"]
                finish(rs)
                st["current"] = None
                RUN_LIVE.pop(key, None)

        while not stop_evt.is_set():
            try:
                with requests.get(url.rstrip("/") + "/events", stream=True,
                                  timeout=(5, 30)) as r:
                    r.raise_for_status()
                    ev = None
                    saw_hello = False
                    retry = 2
                    for raw in r.iter_lines(decode_unicode=False):
                        if stop_evt.is_set():
                            return
                        if not raw:
                            continue
                        line = raw.decode("utf-8", "replace").strip()
                        if line.startswith("event:"):
                            ev = line[6:].strip()
                        elif line.startswith("data:"):
                            try:
                                e = json.loads(line[5:])
                            except Exception:
                                continue
                            if not saw_hello:
                                saw_hello = True
                                if e.get("running"):
                                    # 连接时 run 活跃：重放会把已完成的整轮重新入队（双播），
                                    # 跳过——该 run 的消息由轮询兜底在 run 结束后补播
                                    log(f"[ev:{word}] 连接时 run 活跃 → 跳过重放，等 run 结束重连")
                                    retry = 5
                                    break
                            handle(e, ev)
            except Exception as e:
                if stop_evt.is_set():
                    return
                log(f"[ev:{word}] SSE 断连（{type(e).__name__}），2s 后重连")
            if stop_evt.is_set():
                return
            time.sleep(retry)

    # ── 播报员: 一条一条播（按条 FIFO）；消息内句子级流式（播第 N 句时合成第 N+1 句）──
    def play_message(item):
        who = item.who
        pfx = f"{who}说：" if who not in (None, "", "agent") else ""
        first_s = item.sents[0] if item.sents else ""
        log(f"[reply] {pfx}{first_s[:50]}{'...' if len(first_s) > 50 else ''}")
        play_q = queue.Queue()

        def producer():
            try:
                first = True
                while not stop_evt.is_set():
                    try:
                        s = item.q.get(timeout=60)
                    except queue.Empty:
                        # 轮次死活判定：轮次已死且队列饿死 → 弃尾，防实例宕机把播报员卡死
                        ts = RUN_LIVE.get(item.run_key, 0)
                        if ts and time.time() - ts > 300:
                            log(f"[tts] {who or 'agent'} 轮次结束已久无新句 → 弃这条消息尾部")
                            break
                        continue
                    if s is None:
                        break
                    s = clean_for_speech(s)
                    if not s:
                        continue
                    if first and pfx:
                        s = pfx + s
                        first = False
                    for seg in split_speech(s, SEGMENT_CHARS):
                        if stop_evt.is_set():
                            break
                        try:
                            p = shared.synth(seg)
                            if p:
                                play_q.put(p)
                        except Exception as e:
                            log(f"[tts:kokoro] 段合成失败: {type(e).__name__}: {e}")
                play_q.put(None)
            except Exception as e:
                log(f"[tts-err] producer: {type(e).__name__}: {e}")
                play_q.put(None)

        threading.Thread(target=producer, daemon=True).start()
        while not stop_evt.is_set():
            w = play_q.get()
            if w is None:
                break
            play(w)

    def player():
        while not stop_evt.is_set():
            try:
                item = out_q.get(timeout=1)
            except queue.Empty:
                continue
            try:
                play_message(item)
            except Exception as e:
                log(f"[tts-err] {type(e).__name__}: {e}（跳过这条）")

    # ── /history 兜底轮询: SSE 断线漏掉的整条消息，轮次结束后补播（全文打标去重防双播）──
    def backstop_poller():
        state = {}   # url -> (session_no, 已见 assistant 条目数)
        pending = {}   # (url, 归一化文本) -> 首见时刻：未打标条目挂起复核，防落库/打标竞态双播
        def alive_idle(u):
            """实例可达且无活跃 run/后台任务（run 中 /history 可能被 WebUI 刷新截断 → 条目数失真，不计数）。
            注意：running 必须用 /queue（=Lock||ActiveDrain，与 /history 截断条件同语义）——
            /status 的 running 只含 Lock，drain 期间锁瞬释放会瞬时报 false（2026-09-21 实测踩坑）。"""
            try:
                q = requests.get(u.rstrip("/") + "/queue", timeout=3).json()
                if q.get("running"):
                    return False
                s = requests.get(u.rstrip("/") + "/status", timeout=3).json()
                return not any(not b.get("settled") for b in s.get("backgrounds", []))
            except Exception:
                return False
        def baseline():
            for _w, u in INSTANCES:
                if not alive_idle(u):
                    continue
                try:
                    h = requests.get(u.rstrip("/") + "/history", timeout=3).json()
                    entries = h.get("entries", [])
                    n = len([e for e in entries if e.get("role") == "assistant"
                             and e.get("content")])
                    state[u] = (h.get("session"), n)
                except Exception as e:
                    log(f"[bc] {u} 初始扫描失败: {e}")
                    state[u] = (None, -1)
        baseline()
        log("[bc] 兜底轮询启动（SSE 为主音频源；此为其断线补漏通道）")
        while not stop_evt.is_set():
            time.sleep(8)
            if os.path.exists(BROADCAST_FILE) or os.path.exists(PAUSE_FILE):
                baseline()    # 关闭/暂停期间静默对齐基线，恢复时不补播旧回复
                continue
            for _w, u in INSTANCES:
                if not alive_idle(u):
                    continue    # run 进行中：流式路径为主；轮询不数（防截断假新条目）
                try:
                    h = requests.get(u.rstrip("/") + "/history", timeout=3).json()
                except Exception:
                    continue
                entries = h.get("entries", [])
                asst = [e for e in entries if e.get("role") == "assistant"
                        and e.get("content")]
                session = h.get("session")
                prev = state.get(u, (None, -1))
                last = prev[1] if prev[0] == session else len(asst)
                if last > len(asst):
                    last = len(asst)    # 历史被压缩，重新对齐
                state[u] = (session, len(asst))
                for e in asst[last:]:
                    t = e["content"].strip()
                    if not t:
                        continue
                    if _bc_norm(t) in {x for _ts, x in _BC_SEEN}:
                        continue    # 流式路径已打标 → 跳过（防双播）
                    # 未打标：可能正处于"条目已落库、流式打标未落地"的竞态窗
                    # → 挂起 1.5s 复核仍无标才补播（真漏播最多 +1.5s 延迟）
                    key = (u, _bc_norm(t))
                    ts = pending.get(key)
                    if ts is None or time.time() - ts > 1.5:
                        mark_broadcast(t)
                        who = url_word.get(u)
                        item = MsgItem(who, None)
                        for p in re.split(r"(?<=[。！？!?…；;\n])", t):
                            p = p.strip()
                            if p:
                                item.q.put(p)
                                item.sents.append(p)
                                item.chars += len(p)
                        item.q.put(None)
                        out_q.put(item)
                        log(f"[bc] 补播 {who or 'agent'} 的消息: {t[:30]}...")
                    elif key not in pending:
                        pending[key] = time.time()

    # ── 起线程 ──
    threads = [threading.Thread(target=mic_worker, name="mic", daemon=True)]
    for word, url in INSTANCES:
        threads.append(threading.Thread(target=agent_worker, args=(word, url),
                                        name=f"agent-{word}", daemon=True))
        threads.append(threading.Thread(target=events_listener, args=(word, url),
                                        name=f"events-{word}", daemon=True))
    threads.append(threading.Thread(target=player, name="player", daemon=True))
    threads.append(threading.Thread(target=backstop_poller, name="bc-poller", daemon=True))
    for t in threads:
        t.start()

    # ── 阻塞直到 Ctrl-C ──
    try:
        while not stop_evt.is_set():
            time.sleep(0.5)
    except (KeyboardInterrupt, SystemExit):
        log("[exit] 退出")
    stop_evt.set()
    if PLAY_PROC is not None:
        try:
            PLAY_PROC.kill()
        except OSError:
            pass
    try:
        if shared.alive():
            shared.proc.stdin.close()
            shared.proc.wait(timeout=5)
    except Exception:
        shared.proc.kill()

if __name__ == "__main__":
    signal.signal(signal.SIGTERM, lambda *a: sys.exit(0))
    main()
