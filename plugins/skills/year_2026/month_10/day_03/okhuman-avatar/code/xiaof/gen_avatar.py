#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# 小F (Xiaoba) avatar — port 8452, main color #35B37A (青玉绿),
# emblem = 空白对话气泡 + 三个点 (blank chat bubble + three dots).
# stdlib + Pillow only. Fully deterministic (no random).
# Supersampled 4x on a 2048 canvas, LANCZOS-downscaled to 512.
# All geometry defined in FINAL-512 units (R, line, dots...), then *SCALE for 4x.
import os
import math
from PIL import Image, ImageDraw, ImageFont

SIZE = 512
SCALE = 4
W4 = SIZE * SCALE                       # 2048 supersample
BG = (14, 17, 22)                        # #0E1116
MAIN = (53, 179, 122)                    # #35B37A 青玉绿
TEAL = (63, 184, 160)                    # #3FB8A0 家族之眼 (fixed)
WHITE = (255, 255, 255)                  # 浅底 for emblem bubble
FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
PORT = "8452"
OUTDIR = os.path.dirname(os.path.abspath(__file__))

# ---- FINAL-512 geometry constants (what the spec wants in the output) ----
CENTER = (256, 256)
R_OUT = 150          # ring OUTER radius  (spec 外径150 = outer radius)
LINE_W = 10           # ring line width
R_IN = R_OUT - LINE_W   # ring inner radius = 140
GAP_DEG = 40           # 40deg gap, centered upper-right (315deg)
STR_W, STR_H, STR_GAP = 4, 26, 10        # three stripes: 4x26, edge gap 10
CUR_W, CUR_H = 4, 40                    # cursor line 4x40, from bottom edge
DOT_D = 8                              # family-eye dot diameter
CUR_A = 180                            # cursor max alpha
SCAN_SPAN, SCAN_A = 4, 22               # scan line every 4px, alpha<=25


def fit_font(draw, ref_size=1000):
    """Auto-shrink font so total digit width <= inner_diameter * 0.78 (4x units)."""
    inner_radius = R_IN * SCALE                    # 560 (4x)
    target_w = (2 * inner_radius) * 0.78           # inner_diameter * 0.78
    ref = ImageFont.truetype(FONT, ref_size)
    l, t, r, b = draw.textbbox((0, 0), PORT, font=ref)
    fs = int(ref_size * target_w / (r - l))
    font = ImageFont.truetype(FONT, fs)
    l, t, r, b = draw.textbbox((0, 0), PORT, font=font)
    w = r - l
    assert w <= target_w + 1e-3, f"digit width {w} exceeds {target_w}"
    return font, (l, t, r, b), w


def main():
    canvas = Image.new("RGBA", (W4, W4), BG + (255,))
    draw = ImageDraw.Draw(canvas)
    cx = cy = (SIZE // 2) * SCALE            # 1024

    # --- 2) open ring: outer radius 150, line 10, 40deg gap upper-right ---
    r_out, lw = R_OUT * SCALE, LINE_W * SCALE
    bbox = [cx - r_out, cy - r_out, cx + r_out, cy + r_out]
    draw.arc(bbox, start=335, end=295, fill=MAIN, width=lw)   # gap 295..335 (upper-right)

    # --- 3) digits centered, total width <= inner_dia*0.78, bold-not-small ---
    font, (l, t, r, b), w = fit_font(draw)
    px = cx - (l + r) / 2.0
    py = cy - (t + b) / 2.0
    draw.text((px, py), PORT, font=font, fill=MAIN, stroke_width=3, stroke_fill=MAIN)

    # --- 4) three vertical stripes right of ring: 4x26, edge gap 10, alpha 200/120/60 ---
    strip = Image.new("RGBA", (W4, W4), (0, 0, 0, 0))
    sd = ImageDraw.Draw(strip)
    bar_w, bar_h, gap = STR_W * SCALE, STR_H * SCALE, STR_GAP * SCALE
    total = 3 * bar_w + 2 * gap
    grp_cx = (CENTER[0] + R_OUT + STR_GAP + total / 2) * SCALE   # group centered right of ring
    y0 = int(cy - bar_h / 2)
    x0 = int(grp_cx - total / 2)
    for i, a in enumerate((200, 120, 60)):
        bx = x0 + i * (bar_w + gap)
        sd.rectangle([bx, y0, bx + bar_w - 1, y0 + bar_h - 1], fill=MAIN + (a,))
    canvas = Image.alpha_composite(canvas, strip)
    draw = ImageDraw.Draw(canvas)

    # --- 5) cursor line from bottom edge up: 4x40, x=256, alpha 0->CUR_A (bottom->top) ---
    cur = Image.new("RGBA", (W4, W4), (0, 0, 0, 0))
    cd = ImageDraw.Draw(cur)
    cur_w, cur_h = CUR_W * SCALE, CUR_H * SCALE
    x0c = int((SIZE // 2) * SCALE - cur_w / 2)
    y_bottom = W4
    for i in range(cur_h):
        y = y_bottom - i
        a = int(CUR_A * (i + 1) / cur_h)
        cd.rectangle([x0c, y, x0c + cur_w - 1, y], fill=MAIN + (a,))
    canvas = Image.alpha_composite(canvas, cur)
    draw = ImageDraw.Draw(canvas)

    # --- 6) two family-eye dots, upper-left, diameter 8, >=10px off the ring ---
    diag = math.sqrt(2.0) / 2.0
    dot_r = (DOT_D / 2) * SCALE
    for rad in (168, 188):                       # center radii (>= R_OUT + 10 + DOT_D/2)
        x = int(cx - rad * diag * SCALE)
        y = int(cy - rad * diag * SCALE)
        draw.ellipse([x - dot_r, y - dot_r, x + dot_r, y + dot_r], fill=TEAL)

    # --- emblem (KEEP): blank chat bubble white 浅底 + main outline + three dots ---
    # centered (256,352); bounding box <=84x48, lowest edge y<=380 so it clears the
    # lower arc (ring inner edge ~y=396) by >=10px
    tail = [(236 * SCALE, 372 * SCALE), (236 * SCALE, 380 * SCALE), (252 * SCALE, 372 * SCALE)]
    draw.polygon(tail, fill=WHITE, outline=MAIN, width=7 * SCALE // 2)
    bx0, by0, bx1, by1 = 214 * SCALE, 332 * SCALE, 298 * SCALE, 372 * SCALE
    draw.rounded_rectangle([bx0, by0, bx1, by1], radius=11 * SCALE,
                           fill=WHITE, outline=MAIN, width=7 * SCALE // 2)
    dotc_r = 7 * SCALE
    dcy = 352 * SCALE
    for dx in (-20, 0, 20):
        dcx = 256 * SCALE + dx * SCALE
        draw.ellipse([dcx - dotc_r, dcy - dotc_r, dcx + dotc_r, dcy + dotc_r], fill=MAIN)

    # --- downscale 4x -> 512 (LANCZOS) ---
    base = canvas.convert("RGB").resize((SIZE, SIZE), Image.LANCZOS)

    # --- 1) scan lines crisp at final scale: every SCAN_SPAN, alpha<=SCAN_A ---
    base_rgba = base.convert("RGBA")
    scan = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 0))
    sn = ImageDraw.Draw(scan)
    for y in range(0, SIZE, SCAN_SPAN):
        sn.rectangle([0, y, SIZE - 1, y], fill=(150, 195, 205, SCAN_A))
    base = Image.alpha_composite(base_rgba, scan).convert("RGB")

    base.save(os.path.join(OUTDIR, "avatar_512.png"))
    for sz in (40, 64, 128, 256):
        base.resize((sz, sz), Image.LANCZOS).save(os.path.join(OUTDIR, f"avatar_{sz}.png"))
    print("OK", OUTDIR)


if __name__ == "__main__":
    main()
