import json, urllib.request, urllib.error

caption = (
    "Global Metadata: Comedic cartoon battle underscore, light and playful. 118 BPM, C major, bouncy and cheerful. "
    "A mischievous cartoon tank-battle theme: cheeky staccato brass, bouncy pizzicato strings, playful marimba and xylophone, "
    "light rimshot and shaker percussion with a springy cartoon groove, soft boing and whoosh accents. "
    "Clean, warm and punchy, with gentle comedic tension that never turns dark or heavy. "
    "Fully instrumental background score with moderate dynamics, mixed to leave clear headroom for foreground dialogue.\n\n"
    "Vocal Details: Fully instrumental, no vocals, no spoken words, no humming.\n\n"
    "Arrangement: Intro: quick playful percussion hits and a short marimba motif. "
    "Main loop: bouncy pizzicato strings and staccato brass in a light comedic chase pattern, marimba arpeggios on top, "
    "steady springy rimshot drums. Occasional cartoon boing and soft whoosh for tank movement. "
    "Middle: add a warm rounded bass and a couple of cheeky brass riffs, keeping the groove unhurried. "
    "Outro: percussion and marimba fade with a final playful hit. "
    "Keep overall level moderate and leave clear space for speech."
)

lyrics = "[Intro]\n[Instrumental]\n[Verse]\n[Instrumental]\n[Outro]"

graph = {
    "1": {"class_type": "UNETLoader", "inputs": {"unet_name": "minimax_music3_dit_fp16.safetensors", "weight_dtype": "default"}},
    "2": {"class_type": "CLIPLoader", "inputs": {"clip_name": "minimax_music3_text_encoder_pruned_int8_convrot.safetensors", "type": "minimax", "device": "default"}},
    "3": {"class_type": "VAELoader", "inputs": {"vae_name": "minimax_music3_dav.safetensors"}},
    "4": {"class_type": "MiniMaxMusic3TextEncode", "inputs": {"clip": ["2", 0], "caption": caption, "lyrics": lyrics, "seed": 20260820, "max_duration": 64, "cfg_scale": 1.7, "top_k": 50}},
    "5": {"class_type": "ConditioningZeroOut", "inputs": {"conditioning": ["4", 0]}},
    "6": {"class_type": "EmptyMiniMaxMusic3LatentAudio", "inputs": {"seconds": ["4", 1], "batch_size": 1}},
    "7": {"class_type": "KSampler", "inputs": {"model": ["1", 0], "seed": 20260820, "steps": 30, "cfg": 1.7, "sampler_name": "euler", "scheduler": "simple", "positive": ["4", 0], "negative": ["5", 0], "latent_image": ["6", 0], "denoise": 1}},
    "8": {"class_type": "VAEDecodeAudio", "inputs": {"samples": ["7", 0], "vae": ["3", 0]}},
    "9": {"class_type": "SaveAudioMP3", "inputs": {"audio": ["8", 0], "filename_prefix": "audio/bgm_tankstory_v2", "quality": "320k"}}
}
payload = json.dumps({"prompt": graph}).encode()
req = urllib.request.Request("http://127.0.0.1:8189/prompt", data=payload, headers={"Content-Type": "application/json"})
try:
    with urllib.request.urlopen(req) as r:
        out = json.loads(r.read())
    print("PROMPT_ID:", out.get("prompt_id"))
    print("STATUS:", out.get("status"), "ERRORS:", out.get("node_errors"))
except urllib.error.HTTPError as e:
    print("HTTP", e.code)
    print(e.read().decode()[:2000])
