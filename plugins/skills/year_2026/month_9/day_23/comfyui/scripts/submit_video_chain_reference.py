#!/usr/bin/env python3
"""坦克大战小剧场 v2 — 首尾帧链式 I2V 一镜到底版
规则：只有角色中文台词 + 场景自然音效，无 BGM（后期加）。
EP1: R2V(三视图) 277帧 -> 抽尾帧 -> EP2: I2V(首帧=EP1尾帧) -> ... -> EP5
"""
import json, time, sys, urllib.request, os, subprocess, glob

BASE = "http://127.0.0.1:8189"
OUT_BASE = "<另一台盘>/ComfyUI-0.37.0/output"
REF = "qwen_3view_ref.png"
OUT_DIR = OUT_BASE + "/tankstory_v2"
SEED = 20260820
W, H, LEN = 1120, 640, 277

SUBJECT = ('subject_definitions: <Subject 1> is the cute chibi anime girl: lavender-purple hair in two '
           'round side buns with purple scrunchies, round gold-rimmed glasses, big violet eyes, a '
           'purple-and-white tech jacket with a round Q emblem, purple headphones around her neck, '
           'black shorts, white sneakers.')

L1 = '“网页版坦克大战？写好自己先测好？成交。”'
L2 = '“嗯…再来一个函数，马上就好。”'
L3a = '“测试在跑……咦，这个红色的？”'
L3b = '“抓到你了。38 项全绿。”'
L4 = '“写好了！我自己测过的，零报错。”'
L5 = '“收工～下次再点单！”'

EP = {
"ep1": dict(
  kind="r2v",
  prompt=('''{S}

summary: <Subject 1> lies on her sofa scrolling her phone in a cozy warm evening room. A glowing tank
icon and hearts pop out of the phone. She looks up, winks at camera, then sprints to her desk, plops
down, slams hands on the keyboard, the monitor lights up with green code.

detailed_description: 2D chibi anime cel-shaded style, soft warm evening room with fairy-light bokeh.
(0s-4s) close-up, she lies on the sofa scrolling her phone, the cute tank-icon notification bounces on
the screen (icon only, no readable text), she looks up and says brightly and playfully in Chinese: {L1}
(0s-4s cont.) she winks and gives a thumbs up. (4s-8s) she jumps up and sprints to the desk with a
determined grin. (8s-11.5s) she plops into the chair, slams both hands on the keyboard, the monitor
flares with scrolling green code, she cracks her knuckles. Keep the character's face, hairstyle and
outfit identical in every frame. No readable text, no watermark.

overall_soundscape: cozy room ambience, a phone notification ding, a cartoon whoosh when she dashes,
a keyboard slam, the girl's clear cheerful Chinese voice speaking the line. Natural sounds only,
no background music.

non_diegetic_music: none''')),
"ep2": dict(
  kind="i2v",
  prompt=('''{S}

summary: The same girl at the same desk, now deep in the night, hunching over the keyboard typing
furiously; the monitor full of fast-scrolling green code, a stack of coffee cups on the desk, a wall
clock spinning wildly. She sips coffee, exhausted but eyes glowing with focus.

detailed_description: 2D chibi anime cel-shaded style, night scene, blue moonlight through the window
mixed with green code glow. (0s-4s) medium shot, her fingers blur across the keyboard, code streams on
the monitor, she murmurs sleepily in Chinese: {L2} (4s-8s) close-up: green code reflections in her
round glasses, a tiny sweat drop, a third coffee cup placed on the desk stack, the spinning wall clock
blurs. (8s-11.5s) she takes a sip of coffee, steam swirls, eyes wide and focused, she slaps hands back
on the keyboard, buns bouncing. Keep the character identical in every frame. No readable text, no
watermark.

overall_soundscape: furious mechanical keyboard clacking, a wall clock ticking, a soft coffee slurp,
the girl's tired but determined Chinese voice speaking the line. Natural sounds only, no background
music.

non_diegetic_music: none''')),
"ep3": dict(
  kind="i2v",
  prompt=('''{S}

summary: The same girl now stands before a big glowing test terminal, a long list of green check marks
raining down, one line turns into a red X and a tiny cartoon bug crawls out of the screen; she pokes
the bug and it pops, the red X turns green; she leans back satisfied with two thumbs up.

detailed_description: 2D chibi anime cel-shaded style, cozy room at night, the big monitor shows a
stylized checklist of glowing check icons (abstract marks only, no readable text). (0s-4s) green
checks rain down one by one with sparkles, she watches with a smug smile and says in Chinese: {L3a}
(4s-8s) the red X line, a cute little cartoon bug with antennae crawls out of the screen, she squints,
pokes it firmly with one finger, it pops into sparkles, a bright ding. (8s-11.5s) the red X flips to a
green check, she leans back, crosses her arms, gives two thumbs up and says in Chinese: {L3b} Keep
the character identical in every frame. No readable text, no watermark.

overall_soundscape: rhythmic keyboard tapping, soft pings for each check, a comedic honk when the red X
appears, a bright ding when it turns green, a tiny bubble-pop when the bug is poked, the girl's Chinese
voice speaking both lines. Natural sounds only, no background music.

non_diegetic_music: none''')),
"ep4": dict(
  kind="i2v",
  prompt=('''{S}

summary: Morning. The same girl sits at her desk holding a glowing tablet toward the camera, on the
tablet screen a cute pixel-art tank game visibly plays (pixel tanks on a brick grid); a yellow pixel
tank fires a pixel bullet at a grey enemy tank which explodes; she winks, does a tiny victory spin,
thumbs up.

detailed_description: 2D chibi anime cel-shaded style, morning light. (0s-4s) medium shot, she presents
the glowing tablet to camera, the pixel tank game plays on its screen, she says in Chinese: {L4}
(4s-8s) close-up of the tablet: a yellow pixel tank moves, fires a small pixel bullet, the grey enemy
tank explodes into a little pixel puff. (8s-11.5s) back to her: she winks, does a tiny two-step victory
spin, buns bouncing, confident thumbs up. Keep the character identical in every frame. No readable
text, no watermark.

overall_soundscape: a soft whoosh as the tablet slides forward, cute retro 8-bit game beeps and a tiny
tank blast, the girl's bright confident Chinese voice speaking the line. Natural sounds only, no
background music.

non_diegetic_music: none''')),
"ep5": dict(
  kind="i2v",
  prompt=('''{S}

summary: Celebration. The same girl stands on her desk chair, confetti and little gold stars burst
around her, hero pose with one fist raised, then she points at the camera winking with floating heart
and star icons, finally waves goodbye with both hands.

detailed_description: 2D chibi anime cel-shaded style, warm room, golden confetti and sparkles in the
air. (0s-4s) confetti bursts, she pops up standing on the chair in a hero pose, she says in Chinese:
{L5} (4s-8s) close-up: she points at the camera winking, little heart icons and star icons float up
(icons only, no readable text), her glasses glint. (8s-11.5s) she waves goodbye with both hands, both
purple buns bounce, the screen gently dims to a warm glow. Keep the character identical in every frame.
No readable text, no watermark.

overall_soundscape: a bright fanfare chime, confetti swish, soft applause, a little chime as hearts
float up, the girl's cheerful Chinese voice speaking the line. Natural sounds only, no background
music.

non_diegetic_music: none''')),
}

FILL = dict(S=SUBJECT, L1=L1, L2=L2, L3a=L3a, L3b=L3b, L4=L4, L5=L5)


def build(tag, first_frame=None):
    spec = EP[tag]
    prompt = spec["prompt"].format(**FILL)
    n = {
        "3":  {"class_type": "UNETLoader", "inputs": {"unet_name": "minimax_h3_ref2va_pruned_bf16.safetensors", "weight_dtype": "default"}},
        "4":  {"class_type": "CLIPLoader", "inputs": {"clip_name": "qwen3-vl-32b-minimax-h3-fp8-comfyui.safetensors", "type": "minimax"}},
        "5":  {"class_type": "VAELoader", "inputs": {"vae_name": "minimax_h3_video_vae_fp16.safetensors"}},
        "50": {"class_type": "VAELoader", "inputs": {"vae_name": "minimax_h3_audio_vae_fp32.safetensors"}},
    }
    if spec["kind"] == "r2v":
        n["6"] = {"class_type": "LoadImage", "inputs": {"image": REF}}
        n["7"] = {"class_type": "MiniMaxH3ReferenceToVideo", "inputs": {
            "clip": ["4", 0], "vae": ["5", 0], "audio_vae": ["50", 0],
            "prompt": prompt, "width": W, "height": H, "length": LEN,
            "ref_image_size": "match", "ref_images": {"ref_image_0": ["6", 0]}}}
    else:
        n["6"] = {"class_type": "LoadImage", "inputs": {"image": first_frame}}
        n["7"] = {"class_type": "MiniMaxH3ImageToVideo", "inputs": {
            "clip": ["4", 0], "vae": ["5", 0],
            "prompt": prompt, "width": W, "height": H, "length": LEN,
            "first_frame": ["6", 0]}}
    n["8"]  = {"class_type": "BasicGuider", "inputs": {"model": ["3", 0], "conditioning": ["7", 0]}}
    n["9"]  = {"class_type": "KSamplerSelect", "inputs": {"sampler_name": "res_multistep"}}
    n["10"] = {"class_type": "BasicScheduler", "inputs": {"model": ["3", 0], "scheduler": "normal", "steps": 20, "denoise": 1.0}}
    n["11"] = {"class_type": "RandomNoise", "inputs": {"noise_seed": SEED}}
    n["12"] = {"class_type": "SamplerCustomAdvanced", "inputs": {"noise": ["11", 0], "guider": ["8", 0], "sampler": ["9", 0], "sigmas": ["10", 0], "latent_image": ["7", 1]}}
    n["13"] = {"class_type": "VAEDecode", "inputs": {"samples": ["12", 0], "vae": ["5", 0]}}
    n["14"] = {"class_type": "VAEDecodeAudio", "inputs": {"samples": ["12", 0], "vae": ["50", 0]}}
    n["15"] = {"class_type": "CreateVideo", "inputs": {"images": ["13", 0], "audio": ["14", 0], "fps": 24, "bit_depth": 8}}
    n["16"] = {"class_type": "SaveVideo", "inputs": {"video": ["15", 0], "filename_prefix": "tankstory_v2/" + tag, "format": "mp4", "codec": "auto"}}
    return n


def submit(wf):
    req = urllib.request.Request(BASE + "/prompt",
                                 data=json.dumps({"prompt": wf, "client_id": "tankstory_v2"}).encode(),
                                 method="POST")
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read())


def wait_files(pid, timeout=7200):
    t0 = time.time()
    empty_since = None
    while time.time() - t0 < timeout:
        h = json.loads(urllib.request.urlopen(BASE + f"/history/{pid}").read())
        if h:
            for node_out in h[pid]["outputs"].values():
                cands = node_out.get("videos", []) + [x for x in node_out.get("images", []) if str(x["filename"]).endswith(".mp4")]
                for f in cands:
                    path = os.path.join(OUT_BASE, f.get("subfolder", ""), f["filename"])
                    if os.path.exists(path):
                        return path
        q = json.loads(urllib.request.urlopen(BASE + "/queue").read())
        if not q["queue_running"] and not q["queue_pending"]:
            # 队列空了：可能刚完成(history 还没落盘)也可能真失败，宽限 90s
            if empty_since is None:
                empty_since = time.time()
            elif time.time() - empty_since > 90:
                raise SystemExit(f"FAILED {pid}: queue empty 90s, no output")
        else:
            empty_since = None
        time.sleep(15)
    raise TimeoutError(pid)


def last_frame(video, tag):
    # LoadImage 只认 ComfyUI input 目录，用文件名引用
    out_png = "<另一台盘>/ComfyUI-0.37.0/input/tv2_" + tag + "_last.png"
    subprocess.run(["ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
                    "-sseof", "-0.25", "-i", video, "-frames:v", "1", "-update", "1", out_png],
                   check=True)
    return os.path.basename(out_png)


def main():
    os.makedirs(OUT_DIR, exist_ok=True)
    order = ["ep1", "ep2", "ep3", "ep4", "ep5"]
    for i, tag in enumerate(order):
        # 续跑：已有成片的集直接跳过（链式取首帧仍用上一集成片尾帧）
        if glob.glob(OUT_DIR + "/" + tag + "_*.mp4"):
            print(f"SKIP {tag} (already done)", flush=True)
            continue
        first_frame = None
        if EP[tag]["kind"] == "i2v":
            prev = order[i - 1]
            prevs = sorted(glob.glob(OUT_DIR + "/" + prev + "_*.mp4"))
            if not prevs:
                raise SystemExit(f"missing prev {prev}")
            first_frame = last_frame(prevs[-1], prev)
            print(f"{tag} using first_frame {first_frame}", flush=True)
        res = submit(build(tag, first_frame))
        if res.get("node_errors"):
            raise SystemExit(f"submit errors {tag}: {res['node_errors']}")
        pid = res["prompt_id"]
        print(f"SUBMIT {tag} {pid}", flush=True)
        out = wait_files(pid)
        print(f"DONE {tag} {out}", flush=True)


if __name__ == "__main__":
    main()
