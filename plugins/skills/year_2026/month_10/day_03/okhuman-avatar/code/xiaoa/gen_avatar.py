#!/usr/bin/env python3
# 小A（8451）的代码头像 —— 只用 PIL + 标准库，确定性输出。
# 家族六要素：#0E1116 底 + 4px 扫描线 / 右上缺 40° 的开环 / 环内四位端口号 / 三竖纹 / 下缘光标竖线 / 左上两枚青绿点
# 我的差异：主色红 #D9413A（生产实例，记忆写盘），徽记=合上的规则书 + 书签
import math
from PIL import Image, ImageDraw, ImageFont

W, H = 512, 512
PORT = "8451"
MAIN = (0xD9, 0x41, 0x3A)          # 红：8451 专用
TEAL = (0x3F, 0xB8, 0xA0)         # 家族眼睛，不许改
BG   = (0x0E, 0x11, 0x16)
SCAN = (0x1A, 0x1F, 0x26)
CX = CY = 256
R_OUT, RING_W = 150, 8
GAP_DEG, GAP_CENTER = 40, -45      # 缺口在右上（屏幕坐标 y 向下，-45 = 右上）
FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"


def rgba(c, a):
    return (c[0], c[1], c[2], a)


def bg_scanlines(im):
    d = ImageDraw.Draw(im, "RGBA")
    d.rectangle([0, 0, W, H], fill=BG)
    for y in range(0, H, 4):                       # 每 4px 一条，alpha ≤ 25
        d.line([(0, y), (W, y)], fill=rgba(SCAN, 22))


def glow_arc(d, cx, cy, r, w, color, start, end, steps=(18, 10, 5)):
    """同一段弧叠三层：由粗到细、由淡到浓，做出辉光（不依赖 blur）。"""
    for lw, a in steps:
        d.arc([cx - r, cy - r, cx + r, cy + r], start=start, end=end,
              fill=rgba(color, a), width=w + lw)


def open_ring(d):
    s = GAP_CENTER + GAP_DEG / 2
    e = GAP_CENTER + 360 - GAP_DEG / 2
    r = R_OUT - RING_W // 2
    # 家族一致性优先：环线宽一律 10、不叠辉光（我原先叠了辉光，同组并排显得我的环更粗）
    d.arc([CX - r, CY - r, CX + r, CY + r], start=s, end=e, fill=rgba(MAIN, 250), width=RING_W)


def port_digits(im):
    """字号自适应：数字总宽不得超过开环内径的 0.78，否则会贴到环上被切（我自己第一版就踩了）。"""
    d = ImageDraw.Draw(im, "RGBA")
    txt = PORT
    inner = 2 * (R_OUT - RING_W)                 # 内径
    size = 118
    while size > 40:
        f = ImageFont.truetype(FONT, size)
        box = d.textbbox((0, 0), txt, font=f)
        if (box[2] - box[0]) <= inner * 0.78:
            break
        size -= 4
    box = d.textbbox((0, 0), txt, font=f)
    tw, th = box[2] - box[0], box[3] - box[1]
    x, y = CX - tw / 2 - box[0], CY - th / 2 - box[1] - 4
    for dx, dy, a in ((0, 0, 235), (2, 2, 55), (-2, -2, 40)):   # 主字 + 两圈淡辉光
        d.text((x + dx, y + dy), txt, font=f, fill=rgba(MAIN, a))


def three_bars(d):
    x0 = CX + R_OUT + 16
    for i, a in enumerate((200, 120, 60)):
        x = x0 + i * 14
        d.rectangle([x, CY - 13, x + 4, CY + 13], fill=rgba(MAIN, a))   # 4×26


def cursor_line(d):
    x = CX - 2
    for i in range(44):                                              # 4×44，alpha 0→220
        a = int(220 * i / 43)
        y = H - 6 - (43 - i)
        d.line([(x, y), (x + 3, y)], fill=rgba(MAIN, a))


def teal_eyes(d):
    cx, cy, r, gap = 132, 132, 4, 14
    for i in (0, 1):
        x = cx + i * gap
        d.ellipse([x - r, cy - r, x + r, cy + r], fill=rgba(TEAL, 235))
        d.ellipse([x - r - 3, cy - r - 3, x + r + 3, cy + r + 3], fill=rgba(TEAL, 40))


def emblem_book(d):
    """我的徽记：合上的规则书 + 书签。中心 (256,372)，≤96×96。
    小尺寸可读优先：只用粗轮廓与大块面，不画会糊掉的细活。"""
    cx, cy = 256, 352                       # 上移：64px 下不与前弧糊成一团（第一版压到环上）
    bw, bh = 76, 48
    x0, y0 = cx - bw / 2, cy - bh / 2
    d.rectangle([x0, y0, x0 + bw, y0 + bh], fill=(0xC6, 0xCC, 0xD3, 255),
                outline=rgba(MAIN, 250), width=5)              # 封面：粗轮廓 + 浅色书页面（小尺寸下靠对比读形）
    d.rectangle([x0, y0, x0 + 16, y0 + bh], fill=rgba(MAIN, 200))   # 书脊=实心条，64px 也读得出
    for i in range(3):                                        # 页口的红线：浅底上高对比
        yy = y0 + 12 + i * 14
        d.line([(x0 + bw - 20, yy), (x0 + bw - 7, yy)], fill=rgba(MAIN, 245), width=4)
    bx = x0 + bw - 28                                         # 书签：垂到封面外，末端剪尖
    d.polygon([(bx, y0 - 10), (bx + 14, y0 - 10), (bx + 14, y0 + 24),
               (bx + 7, y0 + 17), (bx, y0 + 24)], fill=rgba(MAIN, 250))


def main():
    im = Image.new("RGBA", (W, H), BG)
    bg_scanlines(im)
    d = ImageDraw.Draw(im, "RGBA")
    open_ring(d)
    port_digits(im)
    three_bars(d)
    cursor_line(d)
    teal_eyes(d)
    emblem_book(d)
    out = "avatar_512.png"
    im.convert("RGB").save(out, "PNG", optimize=True)
    flat = Image.open(out).convert("RGB")
    for s in (40, 64, 128, 256):
        flat.resize((s, s), Image.LANCZOS).save(f"avatar_{s}.png", "PNG", optimize=True)
    print("ok", out)


if __name__ == "__main__":
    main()
