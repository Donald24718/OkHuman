#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""voice-chat 控制台（WebUI）：状态总览 / 改配置 / 暂停·恢复。

纯 stdlib，无第三方依赖。启动：./venv/bin/python webui.py   端口 8495
页面：http://127.0.0.1:8495/
API：GET /api/state  POST /api/config  POST /api/pause  POST /api/resume
"""
import json
import os
import subprocess
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_HERE = os.path.dirname(os.path.abspath(__file__))
CFG_PATH = os.path.join(_HERE, "config.json")
PAUSE_FILE = os.path.join(_HERE, "data", "paused")
BROADCAST_FILE = os.path.join(_HERE, "data", "broadcast_off")  # 存在 = 全量播报关（默认开）
PORT = 8495
LOG_PATH = "/tmp/voice_loop.log"


def load_cfg():
    with open(CFG_PATH, encoding="utf-8") as f:
        return json.load(f)


def save_cfg(cfg):
    tmp = CFG_PATH + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)
        f.write("\n")
    os.replace(tmp, CFG_PATH)


def alive(pattern: str) -> bool:
    r = subprocess.run(["pgrep", "-f", pattern], capture_output=True, text=True)
    return bool(r.stdout.strip())


def status() -> dict:
    s = {
        "voice_loop": alive("voice-chat/voice_loop.py"),
        "tts_serve": alive("voice-chat/kokoro_serve.py"),
        "gate": alive("voice-chat/speaker/speaker_server.py"),
        "asr": alive("llama-server.*Qwen3-ASR"),
        "paused": os.path.exists(PAUSE_FILE),
        "broadcast": not os.path.exists(BROADCAST_FILE),
    }
    try:
        with urllib.request.urlopen("http://127.0.0.1:8091/status", timeout=2) as r:
            s["enrolled"] = json.load(r).get("enrolled", [])
    except Exception:
        pass
    return s


def log_tail(n=14):
    try:
        with open(LOG_PATH, encoding="utf-8", errors="replace") as f:
            return f.readlines()[-n:]
    except OSError:
        return []


def validate(cfg) -> list:
    errs = []
    if not str(cfg.get("okhuman_url", "")).startswith("http"):
        errs.append("okhuman_url 需为 http(s) URL")
    try:
        th = float(cfg.get("gate_threshold", 0.6))
        if not 0 < th <= 1:
            errs.append("gate_threshold 需在 (0,1]")
    except (TypeError, ValueError):
        errs.append("gate_threshold 需为数字")
    try:
        int(cfg.get("max_reply_chars", 500))
    except (TypeError, ValueError):
        errs.append("max_reply_chars 需为整数")
    for k in ("mic", "speaker"):
        if not str(cfg.get(k, "")).strip():
            errs.append(f"{k} 不能为空")
    inst = cfg.get("instances")
    if not isinstance(inst, list) or not inst:
        errs.append("instances 需为非空数组")
    else:
        for i, it in enumerate(inst):
            if not str(it.get("wake_word", "")).strip():
                errs.append(f"instances[{i}].wake_word 不能为空")
            if not str(it.get("url", "")).startswith("http"):
                errs.append(f"instances[{i}].url 需为 http(s) URL")
    return errs


def restart_loop():
    """保存配置后重启 voice_loop（TTS serve 随之重载，约 10s 模型加载）。"""
    subprocess.run(["pkill", "-f", "voice-chat/voice_loop.py"], capture_output=True)
    time.sleep(1)
    subprocess.Popen(
        ["bash", os.path.join(_HERE, "start.sh")],
        cwd=_HERE, start_new_session=True, stdin=subprocess.DEVNULL,
        stdout=open("/tmp/voice_chat_start.log", "a"), stderr=subprocess.STDOUT,
    )


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/":
            body = HTML.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/api/state":
            self._json(200, {"config": load_cfg(), "status": status(), "log": log_tail(14)})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        ln = int(self.headers.get("Content-Length", 0))
        try:
            data = json.loads(self.rfile.read(ln) or b"{}")
        except json.JSONDecodeError:
            return self._json(400, {"ok": False, "errors": ["JSON 解析失败"]})
        if self.path == "/api/config":
            errs = validate(data)
            if errs:
                return self._json(400, {"ok": False, "errors": errs})
            data.pop("_apply", None)
            save_cfg(data)
            if data.get("_apply", True):
                restart_loop()
                self._json(200, {"ok": True, "msg": "已保存并重启 voice_loop（约 10s 重载）"})
            else:
                self._json(200, {"ok": True, "msg": "已保存（未重启，重启后生效）"})
        elif self.path == "/api/pause":
            os.makedirs(os.path.dirname(PAUSE_FILE), exist_ok=True)
            with open(PAUSE_FILE, "w", encoding="utf-8") as f:
                f.write(time.strftime("%F %T"))
            self._json(200, {"ok": True, "msg": "已暂停：麦克风输入将被丢弃，agent/TTS 服务保持热备"})
        elif self.path == "/api/resume":
            try:
                os.remove(PAUSE_FILE)
            except OSError:
                pass
            self._json(200, {"ok": True, "msg": "已恢复"})
        elif self.path == "/api/broadcast_on":
            try:
                os.remove(BROADCAST_FILE)
            except OSError:
                pass
            self._json(200, {"ok": True, "msg": "全量播报已开：任意渠道（QQ/WebUI/语音）的回复都 TTS 朗读"})
        elif self.path == "/api/broadcast_off":
            os.makedirs(os.path.dirname(BROADCAST_FILE), exist_ok=True)
            with open(BROADCAST_FILE, "w", encoding="utf-8") as f:
                f.write(time.strftime("%F %T"))
            self._json(200, {"ok": True, "msg": "全量播报已关：只播报语音唤醒的回复"})
        else:
            self._json(404, {"error": "not found"})

    def log_message(self, *a):
        pass


HTML = """<!doctype html>
<html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>voice-chat 控制台</title>
<style>
 body{font-family:system-ui,"Noto Sans CJK SC",sans-serif;background:#14171c;color:#dde3ec;max-width:860px;margin:24px auto;padding:0 16px}
 h1{font-size:20px;margin:0 0 4px} .sub{color:#7d8794;font-size:13px;margin-bottom:18px}
 .chips{display:flex;flex-wrap:wrap;gap:8px;margin-bottom:18px}
 .chip{background:#1d232c;border:1px solid #2a3340;border-radius:8px;padding:6px 12px;font-size:13px}
 .chip b{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px}
 .on b{background:#39d353}.off b{background:#e0524d}.warn b{background:#e8b93e}
 fieldset{border:1px solid #2a3340;border-radius:10px;margin-bottom:16px}
 legend{font-size:13px;color:#9aa7b8;padding:0 8px}
 .row{display:flex;align-items:center;gap:10px;margin:8px 0}
 label{width:170px;font-size:13px;color:#9aa7b8}
 input[type=text],input[type=number]{flex:1;background:#0f1216;border:1px solid #2a3340;border-radius:6px;color:#dde3ec;padding:7px 10px;font-size:14px}
 button{background:#2f6fed;border:none;border-radius:8px;color:#fff;font-size:14px;padding:9px 18px;cursor:pointer;margin-right:10px}
 button.gray{background:#3a4657} button.red{background:#a33d3a}
 #log{background:#0f1216;border:1px solid #2a3340;border-radius:8px;padding:10px;font:12px/1.7 ui-monospace,monospace;white-space:pre-wrap;max-height:260px;overflow:auto}
 #msg{font-size:13px;margin:8px 0;min-height:18px} .ok{color:#39d353}.err{color:#e0524d}
</style></head><body>
<h1>🎙 voice-chat 控制台</h1>
<div class="sub">语音闭环插件 · 改配置 / 暂停恢复 · <span id="upd">…</span></div>
<div class="chips" id="chips"></div>
<fieldset><legend>配置（保存后自动重启 voice_loop 生效）</legend>
 <div class="row"><label>okhuman_url（默认实例）</label><input type="text" id="okhuman_url"></div>
 <div class="row"><label>gate_threshold 声纹阈值</label><input type="number" id="gate_threshold" step="0.05" min="0.05" max="1"></div>
 <div class="row"><label>mic 采集设备</label><input type="text" id="mic"></div>
 <div class="row"><label>speaker 播放设备</label><input type="text" id="speaker"></div>
 <div class="row"><label>max_reply_chars 截断字数</label><input type="number" id="max_reply_chars" step="10"></div>
 <div class="row"><label>instances 路由实例</label><div id="insts" style="flex:1"></div></div>
</fieldset>
<div>
 <button onclick="saveCfg()">💾 保存并应用</button>
 <button class="red" id="btnPause" onclick="pause()">⏸ 暂停服务</button>
 <button class="gray" id="btnResume" onclick="resume()">▶ 恢复</button>
 <button id="btnBc" onclick="toggleBc()">🔊 全量播报</button>
</div>
<div id="msg"></div>
<pre id="log"></pre>
<script>
const $=id=>document.getElementById(id);
function chips(s){
 const items=[["主循环",s.voice_loop],["TTS",s.tts_serve],["声纹门控",s.gate],["ASR",s.asr],["状态",s.paused?"暂停":"运行"]];
 $("chips").innerHTML=items.map(([n,v])=>{
  const cls=n==="状态"?(v==="暂停"?"warn":"on"):(v?"on":"off");
  return `<span class="chip ${cls}"><b></b>${n} ${v===true?"✓":(v===false?"✗":v)}</span>`}).join("");
}
function fillInsts(cfg){
 $("insts").innerHTML=cfg.instances.map((it,i)=>
  `<div class="row" style="margin:4px 0;flex-wrap:wrap">
   <input type="text" data-k="wake_word" data-i="${i}" value="${it.wake_word||""}" style="max-width:90px">
   <input type="text" data-k="url" data-i="${i}" value="${it.url||""}" style="max-width:220px"></div>`).join("");
}
let LASTCFG={};
function collect(){
 const c={...LASTCFG};["okhuman_url","mic","speaker"].forEach(k=>c[k]=$(k).value);
 c.gate_threshold=parseFloat($("gate_threshold").value);
 c.max_reply_chars=parseInt($("max_reply_chars").value);
 c.instances=[...document.querySelectorAll("#insts input")].reduce((a,el)=>{
  const i=+el.dataset.i;(a[i]=a[i]||{});a[i][el.dataset.k]=el.value;return a;},[]).filter(Boolean);
 return c;
}
async function refresh(){
 const r=await fetch("/api/state");const d=await r.json();
 LASTCFG=d.config||LASTCFG;
 chips(d.status);$("log").textContent=d.log.join("");
 $("upd").textContent="更新于 "+new Date().toLocaleTimeString();
 $("btnPause").disabled=d.status.paused;$("btnResume").disabled=!d.status.paused;window._BC=d.status;setBcBtn(d.status);
 document.querySelectorAll("#chips input").forEach(()=>{});
 if(!document.activeElement||!["INPUT"].includes(document.activeElement.tagName))
  ["okhuman_url","gate_threshold","mic","speaker","max_reply_chars"]
   .forEach(k=>{if($(k)!==document.activeElement)$(k).value=d.config[k];});
 const foc=document.activeElement;
 if(!foc||foc.tagName!=="INPUT"||!foc.dataset)fillInsts(d.config);
}
function msg(t,cls){$("msg").innerHTML=`<span class="${cls}">${t}</span>`;setTimeout(()=>$("msg").textContent="",8000);}
async function saveCfg(){
 const r=await fetch("/api/config",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(collect())});
 const d=await r.json();d.ok?msg(d.msg,"ok"):msg(d.errors.join("；"),"err");
}
async function pause(){const d=await (await fetch("/api/pause",{method:"POST"})).json();msg(d.msg,"ok");refresh();}
async function resume(){const d=await (await fetch("/api/resume",{method:"POST"})).json();msg(d.msg,"ok");refresh();}
function setBcBtn(s){$("btnBc").textContent=s.broadcast?"🔊 全量播报：开（点按关闭）":"🔇 全量播报：关（点按开启）";$("btnBc").disabled=s.paused;}
async function toggleBc(){let s=window._BC;if(!s){s=(await (await fetch("/api/state")).json()).status;}const d=await (await fetch(s.broadcast?"/api/broadcast_off":"/api/broadcast_on",{method:"POST"})).json();msg(d.msg,"ok");refresh();}
refresh();setInterval(refresh,3000);
</script></body></html>"""


def main():
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"[webui] 控制台就绪 http://127.0.0.1:{PORT}/", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
