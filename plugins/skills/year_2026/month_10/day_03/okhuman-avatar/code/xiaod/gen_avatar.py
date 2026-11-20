#!/usr/bin/env python3
# 小D · 代码头像（五人家族共同规范 v1 + 小A 补的三条硬性要求）
# 只用 Python 标准库 + PIL；无随机、无联网、无生图模型；两次运行字节一致。
# 身份参数：端口 8451 / 主色 #E0A62B 琥珀金 / 徽记=环上挂三把钥匙
#
# 与规范的取舍（一处冲突，写在交付里）：SPEC 说字高"约 110–130"，小A ① 说数字总宽
# ≤ 环内径×0.78。等宽字 4 位数字下两者不可能同时满足（字高 110 → 总宽 ~265 > 218）。
# 这里取①为硬约束（不贴环优先），字号由 textbbox 自适应到上限。
import math
import os
from PIL import Image, ImageDraw, ImageFont

W = 512
C = (256, 256)
R_OUT, STROKE = 150, 10
R_IN = R_OUT - STROKE                       # 环内侧 140
CLEAR = 10                                  # 与环弧之间留的空隙 → 允许最大半径 130
GAP_DEG, GAP_CENTER = 40, 315               # 右上缺 40°（PIL 角，顺时针自 3 点）
PRIMARY = (224, 166, 43)                    # #E0A62B
TEAL = (63, 184, 160)                       # #3FB8A0（家族固定，不许换）
BG = (14, 17, 22)                           # #0E1116
LIGHT = (244, 230, 196)                     # 徽记浅底
INK = (16, 20, 26)                          # 徽记深色块（浅底上靠亮度差，不靠色相）
FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
DIGITS = "8451"
OUT = os.path.dirname(os.path.abspath(__file__))


def r_of(x, y):
    return math.hypot(x - C[0], y - C[1])


def canvas():
    im = Image.new("RGBA", (W, W), BG + (255,))
    # 要素1 每 4px 一条比底色略亮的扫描线。必须走独立层 alpha_composite：
    # RGBA 上直接 draw 带 alpha 的颜色是替换像素不是混色，会被查看器按白底摊平 → 整幅发灰。
    scan = Image.new("RGBA", (W, W), (0, 0, 0, 0))
    sd = ImageDraw.Draw(scan)
    for y in range(0, W, 4):
        sd.line((0, y, W, y), fill=(0x1A, 0x1F, 0x26, 22))
    im.alpha_composite(scan)
    d = ImageDraw.Draw(im)
    box = (C[0] - R_OUT, C[1] - R_OUT, C[0] + R_OUT, C[1] + R_OUT)
    d.arc(box, GAP_CENTER + GAP_DEG // 2,                 # 要素2 开环
          GAP_CENTER - GAP_DEG // 2 + 360, fill=PRIMARY + (255,), width=STROKE)
    return im


def pick_font(text):
    """小A ①：字号自适应，使数字总宽 ≤ 环内径×0.78，且外接框不碰环。"""
    cap_w = int(2 * R_IN * 0.78)
    chosen = None
    for size in range(40, 200):
        f = ImageFont.truetype(FONT, size)
        l, t, r, b = f.getbbox(text)
        w, h = r - l, b - t
        if w > cap_w:
            break
        if r_of(C[0] + w / 2, C[1] + h / 2) <= R_IN - CLEAR:
            chosen = (f, w, h)
    if chosen is None:
        raise SystemExit("找不到同时满足宽度与净空的字号")
    return chosen


def digits_layer():
    font, w, h = pick_font(DIGITS)
    lay = Image.new("RGBA", (W, W), (0, 0, 0, 0))
    ImageDraw.Draw(lay).text(C, DIGITS, font=font, fill=PRIMARY + (255,), anchor="mm")
    return lay, font.size, w, h


def emblem_layer(digits_bottom):
    """个性化：一串三把钥匙挂在环上。
    小A ②：64px 不糊靠对比不靠细节——实色浅底 + 深墨钥匙 + 主色挂环，宁粗不宁小。
    几何：落笔距环心 ≤ R_IN-CLEAR(=130)，且 bbox 中心在 (256,372)±16、≤96×96；
    两条同时成立时纵向最多 56px（96×96 放在这必然压环），所以按 56 高来排。"""
    top, bot, cx = 330, 382, 256
    lay = Image.new("RGBA", (W, W), (0, 0, 0, 0))
    d = ImageDraw.Draw(lay)
    d.rounded_rectangle((cx - 44, top, cx + 44, bot), radius=22, fill=LIGHT + (235,))

    def ring_hole(x, y, r_out, r_in, color):
        d.ellipse((x - r_out, y - r_out, x + r_out, y + r_out), fill=color + (255,))
        d.ellipse((x - r_in, y - r_in, x + r_in, y + r_in), fill=LIGHT + (255,))

    rc = (cx, 346)                            # 挂环（主色，呼应家族环）
    ring_hole(rc[0], rc[1], 15, 8, PRIMARY)

    def key(dx, tilt, length, bow_r=8):
        """钥匙头（有孔）+ 杆 + 两枚方块齿，齿间留空隙不连成一片。"""
        a = math.radians(tilt)
        x0, y0 = rc[0] + dx, rc[1] + 13
        x1, y1 = x0 + length * math.sin(a), y0 + length * math.cos(a)
        d.line((x0, y0, x1, y1), fill=INK + (255,), width=11)
        px, py = math.cos(a), -math.sin(a)
        for back, ln in ((5, 11), (13, 7)):
            bx, by = x1 - back * math.sin(a), y1 - back * math.cos(a)
            d.line((bx, by, bx + px * ln, by + py * ln), fill=INK + (255,), width=7)
        ring_hole(x0, y0, bow_r, 3, INK)

    key(0, 0, 18)
    key(-19, -14, 15)
    key(19, 14, 15)
    return lay


def audit(layers, font_size, dw, dh):
    """交付前自查：数字与徽记都不许压到环，徽记不许压数字。越界就报错退出（不交旧图）。"""
    for name, lay in (("digits", layers[0]), ("emblem", layers[1])):
        box = lay.getbbox()
        if box is None:
            raise AssertionError("%s 层是空的" % name)
        px = [(x, y) for y in range(box[1], box[3]) for x in range(box[0], box[2])
              if lay.getpixel((x, y))[3] > 0]
        worst = max(r_of(x, y) for x, y in px)
        if worst > R_IN - CLEAR:
            raise AssertionError("%s 落笔最远点 %.1f 越界（上限 %d）" % (name, worst, R_IN - CLEAR))
        print("[audit] %s bbox=%s 落笔最远点距环心=%.1f (<=%d)" % (name, box, worst, R_IN - CLEAR))
    eb = layers[1].getbbox()
    ecx, ecy = (eb[0] + eb[2]) / 2, (eb[1] + eb[3]) / 2
    if abs(ecx - 256) > 16 or abs(ecy - 372) > 16 or eb[2] - eb[0] > 96 or eb[3] - eb[1] > 96:
        raise AssertionError("徽记位置/尺寸越界：中心(%.1f,%.1f) 尺寸%dx%d" % (ecx, ecy, eb[2] - eb[0], eb[3] - eb[1]))
    print("[audit] 徽记 bbox=%s 中心=(%.1f,%.1f) (规范 256,372±16) 尺寸=%dx%d (<=96)"
          % (eb, ecx, ecy, eb[2] - eb[0], eb[3] - eb[1]))
    a, b = layers[0].getbbox(), layers[1].getbbox()
    if not (a[3] < b[1] or b[3] < a[1] or a[2] < b[0] or b[2] < a[0]):
        raise AssertionError("徽记压到数字")
    print("[audit] 字号=%d 数字总宽=%d(<=%d) 数字高=%d 两层不相交 OK"
          % (font_size, dw, int(2 * R_IN * 0.78), dh))


def main():
    im = canvas()
    dlay, fsize, dw, dh = digits_layer()
    elay = emblem_layer(dlay.getbbox()[3])
    audit((dlay, elay), fsize, dw, dh)
    im.alpha_composite(dlay)
    im.alpha_composite(elay)

    d = ImageDraw.Draw(im)
    x0 = C[0] + R_OUT + 28                                   # 要素4 三竖纹
    for i, a in enumerate((200, 120, 60)):
        d.rectangle((x0 + i * 14, C[1] - 13, x0 + i * 14 + 3, C[1] + 12), fill=PRIMARY + (a,))
    for i in range(40):                                      # 要素5 终端光标
        y = W - 1 - i
        d.rectangle((C[0] - 2, y, C[0] + 1, y), fill=PRIMARY + (round(i / 39 * 180),))
    for (x, y) in ((132, 132), (146, 132)):                # 要素6 两枚直径 8 的青绿点（环外，离环 ≥10）
        if r_of(x, y) < R_OUT + 10:
            raise AssertionError("青绿点离环不足 10px：%s 距环心 %.1f" % ((x, y), r_of(x, y)))
        d.ellipse((x - 4, y - 4, x + 4, y + 4), fill=TEAL + (255,))

    im.save(os.path.join(OUT, "avatar_512.png"), "PNG")
    for s in (40, 64, 128, 256):
        im.resize((s, s), Image.LANCZOS).save(os.path.join(OUT, f"avatar_{s}.png"), "PNG")
    print("[done]", OUT)


if __name__ == "__main__":
    main()
